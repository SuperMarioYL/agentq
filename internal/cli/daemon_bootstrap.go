package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// daemonHandle describes the serve daemon that `agentq wrap --daemon` forwards
// to, and how to release it once the wrap session ends.
type daemonHandle struct {
	// BaseURL is the daemon origin, e.g. "http://127.0.0.1:7777".
	BaseURL string
	// Token is the bearer token to present. Empty when reusing a daemon whose
	// token we do not know (the wrapper still reaches an unauthenticated local
	// daemon, or the operator passed one explicitly).
	Token string
	// Reused reports whether an already-running daemon was reused (true) or a
	// fresh one was started by this wrap (false).
	Reused bool
	// stop tears down a daemon this wrap started. No-op when Reused.
	stop func()
}

// Close releases a daemon this wrap started. Safe to call when the daemon was
// reused (no-op) or already closed.
func (h *daemonHandle) Close() {
	if h != nil && h.stop != nil {
		h.stop()
		h.stop = nil
	}
}

// daemonBootstrap groups the injectable dependencies of ensureDaemon so the
// reuse-vs-start decision is unit-testable without binding real ports.
type daemonBootstrap struct {
	// Listen is the host:port the daemon should live at, e.g. "127.0.0.1:7777".
	Listen string
	// Token is an explicit bearer token; empty means "generate one when starting".
	Token string
	// probe reports whether a daemon is already answering at listen. It is the
	// reuse detector: a bound, healthy port means reuse.
	probe func(listen string) bool
	// start launches a new daemon at listen with the given token and returns a
	// stop func. Only called when probe reports no existing daemon. It must not
	// return until the daemon is accepting connections (or errors).
	start func(ctx context.Context, listen, token string) (stop func(), err error)
	// newToken mints a bearer token when Token is empty and a daemon must start.
	newToken func() (string, error)
}

// ensureDaemon implements the reuse-or-start bootstrap for `wrap --daemon`: if a
// daemon already answers at Listen it is reused (no new process, its token left
// as configured); otherwise a fresh daemon is started and a handle that can stop
// it is returned. This is the one first-class command the milestone asks for —
// the operator no longer has to start `serve` by hand before `wrap`.
func (b daemonBootstrap) ensureDaemon(ctx context.Context) (*daemonHandle, error) {
	base := "http://" + b.Listen
	if b.probe != nil && b.probe(b.Listen) {
		// An existing daemon owns the port — reuse it rather than colliding.
		return &daemonHandle{BaseURL: base, Token: b.Token, Reused: true, stop: nil}, nil
	}

	token := b.Token
	if token == "" {
		mint := b.newToken
		if mint == nil {
			mint = randomDaemonToken
		}
		t, err := mint()
		if err != nil {
			return nil, fmt.Errorf("wrap --daemon: generate token: %w", err)
		}
		token = t
	}
	if b.start == nil {
		return nil, fmt.Errorf("wrap --daemon: no start function configured")
	}
	stop, err := b.start(ctx, b.Listen, token)
	if err != nil {
		return nil, fmt.Errorf("wrap --daemon: start local daemon: %w", err)
	}
	return &daemonHandle{BaseURL: base, Token: token, Reused: false, stop: stop}, nil
}

// probeDaemon reports whether a healthy daemon is already listening at listen by
// hitting the token-free /healthz endpoint. A connection refusal (nothing bound)
// or any non-2xx means "not running / not ours", so wrap should start its own.
func probeDaemon(listen string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + listen + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// waitForDaemon polls /healthz until the daemon answers or the deadline passes.
// Used after starting a fresh daemon so ensureDaemon does not return before the
// listener is actually accepting connections.
func waitForDaemon(listen string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if probeDaemon(listen) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wrap --daemon: daemon at %s did not become ready within %s", listen, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// portIsFree reports whether listen can be bound right now. Used as a secondary
// signal by tests; ensureDaemon itself relies on probe (health), not this.
func portIsFree(listen string) bool {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func randomDaemonToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
