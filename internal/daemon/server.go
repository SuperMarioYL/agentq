package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// webAssets is set by SetWebAssets so the daemon package stays
// embed-free at unit-test time. The cli package passes its embedded
// FS in here from init().
var (
	webAssets   fs.FS
	webAssetsMu sync.RWMutex
)

// SetWebAssets wires the embedded SPA into the server. Idempotent;
// later calls overwrite earlier ones (useful for tests that swap in
// a fake FS).
func SetWebAssets(f fs.FS) {
	webAssetsMu.Lock()
	defer webAssetsMu.Unlock()
	webAssets = f
}

func currentWebAssets() fs.FS {
	webAssetsMu.RLock()
	defer webAssetsMu.RUnlock()
	return webAssets
}

// Config groups the runtime knobs for Server.
type Config struct {
	// Listen is the bind address, e.g. "127.0.0.1:7777" or "0.0.0.0:7777".
	Listen string

	// Token is the bearer token clients must present either via the
	// "Authorization: Bearer ..." header or the "?t=" query string.
	// Empty token disables auth (useful for tests).
	Token string

	// EnvelopeTTL caps how long a wrapper's POST /api/envelopes call
	// blocks before the daemon gives up. Default 15 minutes.
	EnvelopeTTL time.Duration

	// Store is the persistence layer. Required.
	Store *Store

	// Queue is the in-flight waiter hub. Required.
	Queue *Queue
}

// Server wires the echo router and middleware. Use Server.Handler
// for httptest, Server.Run for the production HTTP listener.
type Server struct {
	cfg Config
	e   *echo.Echo
}

// NewServer constructs the HTTP server. It registers all routes but
// does not start listening.
func NewServer(cfg Config) *Server {
	if cfg.EnvelopeTTL == 0 {
		cfg.EnvelopeTTL = 15 * time.Minute
	}
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())
	s := &Server{cfg: cfg, e: e}
	s.routes()
	return s
}

// Handler returns the http.Handler the daemon listens with. Useful for
// httptest.NewServer in tests.
func (s *Server) Handler() http.Handler { return s.e }

// Echo exposes the underlying *echo.Echo for advanced uses like
// custom middleware or graceful shutdown hooks.
func (s *Server) Echo() *echo.Echo { return s.e }

// Run starts the listener on cfg.Listen and blocks until ctx is
// cancelled, at which point it triggers a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := s.e.Start(s.cfg.Listen); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.e.Shutdown(shutdownCtx)
	case err, ok := <-errCh:
		if !ok {
			return nil
		}
		return err
	}
}

func (s *Server) routes() {
	api := s.e.Group("/api", s.authMiddleware)
	api.GET("/queue", s.listQueue)
	api.POST("/queue/:id/answer", s.answerEnvelope)
	api.POST("/envelopes", s.postEnvelope)
	s.e.GET("/ws", s.websocketHandler, s.authMiddleware)
	s.e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	// Publish the ApprovalEnvelope JSON Schema unauthenticated: making the wire
	// format publicly fetchable is the protocol moat — a third-party runtime can
	// GET this, validate its own output, and POST conforming envelopes to
	// /api/envelopes without any agentq wrapper.
	s.e.GET("/schema/approval-envelope.json", func(c echo.Context) error {
		return c.Blob(http.StatusOK, protocol.ApprovalEnvelopeSchemaContentType, protocol.ApprovalEnvelopeSchema)
	})
	s.mountStatic()
}

func (s *Server) mountStatic() {
	assets := currentWebAssets()
	if assets == nil {
		s.e.GET("/", func(c echo.Context) error {
			return c.String(http.StatusOK,
				"agentq daemon — UI assets unbuilt; use the REST API at /api/queue.")
		})
		return
	}
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		sub = assets
	}
	handler := http.FileServer(http.FS(sub))
	s.e.GET("/*", echo.WrapHandler(handler))
}

func (s *Server) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.cfg.Token == "" {
			return next(c)
		}
		got := c.QueryParam("t")
		if got == "" {
			h := c.Request().Header.Get("Authorization")
			// RFC 7235: the auth-scheme token is case-insensitive, so a
			// conforming client may send "bearer <token>" (lowercase scheme).
			// The old strings.TrimPrefix(h, "Bearer ") was case-sensitive and
			// left got="bearer <token>", so ConstantTimeCompare failed and the
			// request was rejected with 401 — exactly the interop the public
			// ApprovalEnvelope schema (POST /api/envelopes) is meant to enable.
			// Split scheme + token and compare the scheme case-insensitively
			// before the constant-time token compare. (fix-auth-bearer-scheme-case-sensitive)
			if scheme, rest, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
				got = strings.TrimSpace(rest)
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			return echo.NewHTTPError(http.StatusUnauthorized, "missing or invalid token")
		}
		return next(c)
	}
}

func (s *Server) listQueue(c echo.Context) error {
	envs, err := s.cfg.Store.ListEnvelopes(50)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if envs == nil {
		envs = []*protocol.ApprovalEnvelope{}
	}
	return c.JSON(http.StatusOK, envs)
}

func (s *Server) postEnvelope(c echo.Context) error {
	var env protocol.ApprovalEnvelope
	if err := c.Bind(&env); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid envelope JSON: "+err.Error())
	}
	if env.ID == "" || env.AgentID == "" || env.Prompt == "" || len(env.Choices) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "envelope missing required fields (id, agent_id, prompt, choices)")
	}
	// The envelope arrived already past its own ExpiresAt (a tight expiry that
	// elapsed in transit, or clock skew). It is a dead card everywhere else —
	// ListEnvelopes filters it and answerEnvelope rejects it — so registering it
	// as a live prompt and blocking the wrapper for the FULL server TTL (the
	// `d > 0` guard below otherwise leaves ttl at EnvelopeTTL for a non-positive
	// remaining time) is wrong. Report the timeout immediately so the wrapper
	// falls back to its default without a dead card ever entering the queue.
	if !env.ExpiresAt.IsZero() && !time.Now().Before(env.ExpiresAt) {
		return echo.NewHTTPError(http.StatusGatewayTimeout, "envelope already expired on arrival")
	}
	if err := s.cfg.Store.PutEnvelope(&env); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := s.cfg.Queue.Register(&env); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	ttl := s.cfg.EnvelopeTTL
	if !env.ExpiresAt.IsZero() {
		if d := time.Until(env.ExpiresAt); d > 0 && d < ttl {
			ttl = d
		}
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), ttl)
	defer cancel()
	ans, err := s.cfg.Queue.Wait(ctx, env.ID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The wrapper gave up (envelope expired / TTL elapsed). Evict the dead
			// card from the store so it does not linger: ListEnvelopes only filters
			// on ExpiresAt, so an envelope POSTed WITHOUT an expires_at (allowed by
			// the published schema for third-party producers) would otherwise be
			// resurrected in GET /api/queue and the WebSocket bootstrap snapshot for
			// every (re)connecting phone forever. Then tell every connected phone to
			// drop the now-dead card.
			_ = s.cfg.Store.DeleteEnvelope(env.ID)
			s.cfg.Queue.BroadcastRemoved(env.ID)
			return echo.NewHTTPError(http.StatusGatewayTimeout, "no answer within ttl")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, ans)
}

func (s *Server) answerEnvelope(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing envelope id")
	}
	var body struct {
		ChoiceKey string `json:"choice_key"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid answer JSON: "+err.Error())
	}
	if body.ChoiceKey == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "choice_key required")
	}
	env, err := s.cfg.Store.GetEnvelope(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such envelope")
	}
	// Reject answers past the envelope's ExpiresAt. Per
	// protocol.ApprovalEnvelope.ExpiresAt the wrapper has already given up at that
	// point and acted on its default choice, so persisting a human choice now
	// would write a misleading audit record for a decision that never took effect
	// — and ListEnvelopes already treats an expired card as dead. Reject WITHOUT
	// persisting, and broadcast the removal so any stale phone/tab drops the card.
	if !env.ExpiresAt.IsZero() && time.Now().After(env.ExpiresAt) {
		s.cfg.Queue.BroadcastRemoved(id)
		return echo.NewHTTPError(http.StatusGone, "envelope expired; the wrapper already acted on its default")
	}
	if !choiceKnown(env.Choices, body.ChoiceKey) {
		return echo.NewHTTPError(http.StatusBadRequest, "choice_key not in envelope.choices")
	}
	ans := protocol.Answer{
		EnvelopeID: id,
		ChoiceKey:  body.ChoiceKey,
		AnsweredAt: time.Now().UTC(),
	}
	// Persist create-only: the FIRST answer to a card is the audit record, and a
	// later/racing answer (a stale reconnected tab, or a second phone on the LAN)
	// must NOT overwrite it — the wrapper already acted on the first choice. If an
	// answer is already recorded, keep it and report the original, not this one.
	stored, err := s.cfg.Store.PutAnswerIfAbsent(&ans)
	if err != nil {
		if errors.Is(err, ErrAnswerExists) {
			// Already answered earlier by another phone/tab. Broadcast the
			// removal so EVERY connected phone (not just this one) drops the
			// stale card — Queue.Answer only emits an event when it delivers to
			// a live waiter, which no longer exists on this path.
			s.cfg.Queue.BroadcastAnswered(*stored)
			return c.JSON(http.StatusConflict, stored)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err := s.cfg.Queue.Answer(ans); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Wrapper already gave up; the answer is still persisted for audit.
			// Broadcast so all connected phones drop the dead card — the success
			// branch of Queue.Answer never ran because there was no live waiter.
			s.cfg.Queue.BroadcastAnswered(ans)
			return c.JSON(http.StatusAccepted, ans)
		}
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusOK, ans)
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Local LAN only; CSRF is irrelevant when the daemon is bound to
	// 127.0.0.1 or a token-gated LAN address.
	CheckOrigin: func(*http.Request) bool { return true },
}

func (s *Server) websocketHandler(c echo.Context) error {
	conn, err := wsUpgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	events, cancel := s.cfg.Queue.Subscribe()
	defer cancel()

	// Push the initial queue snapshot so a freshly-connected phone is
	// in sync without needing a separate REST round-trip.
	if envs, err := s.cfg.Store.ListEnvelopes(50); err == nil {
		for _, env := range envs {
			if werr := conn.WriteJSON(Event{Kind: EventNewEnvelope, Envelope: env}); werr != nil {
				return nil
			}
		}
	}

	pingTick := time.NewTicker(30 * time.Second)
	defer pingTick.Stop()

	// Drain client-side messages so the read pump sees Close frames
	// and the connection actually shuts down when the phone tab closes.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if werr := conn.WriteJSON(ev); werr != nil {
				return nil
			}
		case <-pingTick.C:
			if werr := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); werr != nil {
				return nil
			}
		case <-closed:
			return nil
		}
	}
}

func choiceKnown(cs []protocol.Choice, key string) bool {
	for _, c := range cs {
		if c.Key == key {
			return true
		}
	}
	return false
}
