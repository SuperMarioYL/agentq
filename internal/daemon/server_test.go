package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

func newTestServer(t *testing.T, token string) (*httptest.Server, *Store) {
	t.Helper()
	ts, store, _ := newTestServerWithQueue(t, token)
	return ts, store
}

// newTestServerWithQueue is like newTestServer but also returns the Queue so a
// test can Subscribe and assert the broadcasts the answer/expiry paths emit.
func newTestServerWithQueue(t *testing.T, token string) (*httptest.Server, *Store, *Queue) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	q := NewQueue()
	srv := NewServer(Config{
		Token:       token,
		Store:       store,
		Queue:       q,
		EnvelopeTTL: 2 * time.Second,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store, q
}

func TestServer_QueueRequiresToken(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	res, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", res.StatusCode)
	}
}

func TestServer_QueueAcceptsTokenViaQuery(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	res, err := http.Get(ts.URL + "/api/queue?t=secret")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", res.StatusCode)
	}
}

func TestServer_QueueAcceptsTokenViaHeader(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/queue", nil)
	req.Header.Set("Authorization", "Bearer secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", res.StatusCode)
	}
}

// TestServer_QueueAcceptsLowercaseBearerScheme guards fix-auth-bearer-scheme-case-sensitive:
// RFC 7235 mandates the auth-scheme be compared case-insensitively, so a client
// sending "Authorization: bearer <token>" (lowercase scheme — legal) must
// authenticate. The old strings.TrimPrefix(h, "Bearer ") was case-sensitive and
// rejected it with 401, breaking third-party producers that POST conforming
// envelopes to /api/envelopes with a header-normalizing HTTP client.
func TestServer_QueueAcceptsLowercaseBearerScheme(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/queue", nil)
	req.Header.Set("Authorization", "bearer secret") // lowercase scheme — legal per RFC 7235
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 (lowercase bearer scheme must authenticate per RFC 7235)", res.StatusCode)
	}
}

func TestServer_QueueListEmpty(t *testing.T) {
	ts, _ := newTestServer(t, "")
	res, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", res.StatusCode)
	}
	var list []protocol.ApprovalEnvelope
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d", len(list))
	}
}

func TestServer_PostAndAnswerEnvelopeFlow(t *testing.T) {
	ts, store := newTestServer(t, "secret")

	env := protocol.ApprovalEnvelope{
		ID: "01ABC", AgentID: "claude-1", Prompt: "ok?",
		Choices:   []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}},
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
	body, _ := json.Marshal(env)

	type postResult struct {
		ans protocol.Answer
		err error
	}
	postCh := make(chan postResult, 1)
	go func() {
		res, err := http.Post(ts.URL+"/api/envelopes?t=secret", "application/json", bytes.NewReader(body))
		if err != nil {
			postCh <- postResult{err: err}
			return
		}
		defer res.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		if res.StatusCode != http.StatusOK {
			postCh <- postResult{err: fmt.Errorf("post status=%d body=%s", res.StatusCode, buf.String())}
			return
		}
		var ans protocol.Answer
		if err := json.Unmarshal(buf.Bytes(), &ans); err != nil {
			postCh <- postResult{err: err}
			return
		}
		postCh <- postResult{ans: ans}
	}()

	// Wait for the daemon to persist the envelope before we answer it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := store.GetEnvelope(env.ID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("envelope never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	res, err := http.Post(
		ts.URL+"/api/queue/"+env.ID+"/answer?t=secret",
		"application/json",
		strings.NewReader(`{"choice_key":"y"}`),
	)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		t.Fatalf("answer status=%d body=%s", res.StatusCode, buf.String())
	}

	select {
	case got := <-postCh:
		if got.err != nil {
			t.Fatalf("post envelope: %v", got.err)
		}
		if got.ans.ChoiceKey != "y" || got.ans.EnvelopeID != env.ID {
			t.Errorf("unexpected answer: %+v", got.ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post envelope did not return after answer")
	}

	// Queue should now be empty because the envelope has an answer.
	listRes, err := http.Get(ts.URL + "/api/queue?t=secret")
	if err != nil {
		t.Fatalf("queue list: %v", err)
	}
	defer listRes.Body.Close()
	var list []protocol.ApprovalEnvelope
	_ = json.NewDecoder(listRes.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("queue not drained: %+v", list)
	}
}

func TestServer_AnswerRejectsUnknownChoice(t *testing.T) {
	ts, store := newTestServer(t, "")
	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "01", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}},
	})
	res, err := http.Post(
		ts.URL+"/api/queue/01/answer",
		"application/json",
		strings.NewReader(`{"choice_key":"bogus"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", res.StatusCode)
	}
}

func TestServer_AnswerOnMissingEnvelope(t *testing.T) {
	ts, _ := newTestServer(t, "")
	res, err := http.Post(
		ts.URL+"/api/queue/none/answer",
		"application/json",
		strings.NewReader(`{"choice_key":"y"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", res.StatusCode)
	}
}

func TestServer_PostEnvelopeRejectsMissingFields(t *testing.T) {
	ts, _ := newTestServer(t, "")
	body := strings.NewReader(`{"id":"x"}`)
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", res.StatusCode)
	}
}

func TestServer_PostEnvelopeTimesOutWithoutAnswer(t *testing.T) {
	ts, _ := newTestServer(t, "")
	env := protocol.ApprovalEnvelope{
		ID: "timeout-1", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(100 * time.Millisecond),
	}
	body, _ := json.Marshal(env)
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status=%d want 504", res.StatusCode)
	}
}

func TestServer_Healthz(t *testing.T) {
	ts, _ := newTestServer(t, "secret")
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", res.StatusCode)
	}
}

// TestServer_SecondAnswerDoesNotOverwriteAudit guards fix-double-answer-audit-overwrite:
// once a card is answered, a second answer (a stale reconnected tab, or a second
// phone on the LAN) must NOT overwrite the stored audit record — the wrapper acted
// on the first choice. The endpoint returns 409 with the ORIGINAL answer, and the
// persisted answer is unchanged.
func TestServer_SecondAnswerDoesNotOverwriteAudit(t *testing.T) {
	ts, store := newTestServer(t, "")
	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "dup-1", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}, {Key: "n"}},
	})

	// First answer: y. No waiter is registered (no in-flight wrapper), so the
	// handler persists for audit and the queue reports the wrapper already gone.
	res1, err := http.Post(ts.URL+"/api/queue/dup-1/answer",
		"application/json", strings.NewReader(`{"choice_key":"y"}`))
	if err != nil {
		t.Fatalf("first answer: %v", err)
	}
	res1.Body.Close()
	if res1.StatusCode != http.StatusAccepted {
		t.Fatalf("first answer status=%d want 202", res1.StatusCode)
	}

	// Second answer: n. Must be rejected as already-answered and must NOT overwrite.
	res2, err := http.Post(ts.URL+"/api/queue/dup-1/answer",
		"application/json", strings.NewReader(`{"choice_key":"n"}`))
	if err != nil {
		t.Fatalf("second answer: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("second answer status=%d want 409", res2.StatusCode)
	}
	var returned protocol.Answer
	if err := json.NewDecoder(res2.Body).Decode(&returned); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if returned.ChoiceKey != "y" {
		t.Errorf("409 returned ChoiceKey=%q; want the original y", returned.ChoiceKey)
	}

	// The persisted audit record must still be the first choice.
	stored, err := store.GetAnswer("dup-1")
	if err != nil {
		t.Fatalf("GetAnswer: %v", err)
	}
	if stored.ChoiceKey != "y" {
		t.Errorf("audit record overwritten: ChoiceKey=%q want y", stored.ChoiceKey)
	}
}

// waitForAnsweredEvent drains the subscriber channel until it sees an
// EventAnswered for id, or fails after a short timeout.
func waitForAnsweredEvent(t *testing.T, ch <-chan Event, id string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == EventAnswered && ev.Answer != nil && ev.Answer.EnvelopeID == id {
				return
			}
		case <-deadline:
			t.Fatalf("no EventAnswered broadcast for %q", id)
		}
	}
}

// TestServer_AnswerBroadcastsOn202Path guards fix-answered-card-not-broadcast-to-other-tabs:
// answering a card whose wrapper already timed out (no live waiter → 202) must
// still broadcast an answered event so OTHER connected phones drop the stale
// card. Before the fix the 202 path emitted no event.
func TestServer_AnswerBroadcastsOn202Path(t *testing.T) {
	ts, store, q := newTestServerWithQueue(t, "")
	ch, cancel := q.Subscribe()
	defer cancel()

	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "b202", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}, {Key: "n"}},
	})

	res, err := http.Post(ts.URL+"/api/queue/b202/answer",
		"application/json", strings.NewReader(`{"choice_key":"y"}`))
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d want 202", res.StatusCode)
	}
	waitForAnsweredEvent(t, ch, "b202")
}

// TestServer_AnswerBroadcastsOn409Path guards the 409 already-answered path: a
// second answer to an already-answered card must broadcast a removal so the
// other phones (whose card is still on screen) drop it. Before the fix the 409
// path emitted no event.
func TestServer_AnswerBroadcastsOn409Path(t *testing.T) {
	ts, store, q := newTestServerWithQueue(t, "")

	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "b409", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}, {Key: "n"}},
	})

	// First answer (202, persisted for audit).
	res1, err := http.Post(ts.URL+"/api/queue/b409/answer",
		"application/json", strings.NewReader(`{"choice_key":"y"}`))
	if err != nil {
		t.Fatalf("first answer: %v", err)
	}
	res1.Body.Close()

	// Now subscribe, THEN send the second answer so we only observe the 409 path.
	ch, cancel := q.Subscribe()
	defer cancel()
	res2, err := http.Post(ts.URL+"/api/queue/b409/answer",
		"application/json", strings.NewReader(`{"choice_key":"n"}`))
	if err != nil {
		t.Fatalf("second answer: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("second answer status=%d want 409", res2.StatusCode)
	}
	waitForAnsweredEvent(t, ch, "b409")
}

// TestServer_PostEnvelopeTimeoutBroadcastsRemoval guards the expiry-removal
// broadcast: when a wrapper's POST /api/envelopes times out (504), the daemon
// must broadcast a removal so connected UIs drop the now-dead card immediately.
func TestServer_PostEnvelopeTimeoutBroadcastsRemoval(t *testing.T) {
	ts, _, q := newTestServerWithQueue(t, "")
	ch, cancel := q.Subscribe()
	defer cancel()

	env := protocol.ApprovalEnvelope{
		ID: "toremove", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(120 * time.Millisecond),
	}
	body, _ := json.Marshal(env)
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504", res.StatusCode)
	}
	// The first event is the EventNewEnvelope from Register; drain until removal.
	waitForAnsweredEvent(t, ch, "toremove")
}

// TestServer_ExpiredEnvelopeNotListed guards fix-expired-envelopes-linger-in-queue:
// an envelope whose ExpiresAt has passed must not appear in GET /api/queue even
// though no answer was ever recorded. Before the fix ListEnvelopes filtered only
// on a stored answer, so aborted envelopes lingered forever.
func TestServer_ExpiredEnvelopeNotListed(t *testing.T) {
	ts, store, _ := newTestServerWithQueue(t, "")

	// One live envelope, one already expired.
	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "live-1", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "expired-1", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(-time.Minute),
	})

	res, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("queue list: %v", err)
	}
	defer res.Body.Close()
	var list []protocol.ApprovalEnvelope
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].ID != "live-1" {
		t.Fatalf("queue=%+v want only live-1 (expired dropped)", list)
	}
}

// TestServer_SchemaEndpoint guards m5_public_envelope_schema: the ApprovalEnvelope
// JSON Schema is served unauthenticated and is valid JSON describing the envelope.
func TestServer_SchemaEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, "secret") // token set, but the schema route is public
	res, err := http.Get(ts.URL + "/schema/approval-envelope.json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 (schema must be public)", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "schema+json") {
		t.Errorf("Content-Type=%q want application/schema+json", ct)
	}
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc["title"] != "ApprovalEnvelope" {
		t.Errorf("schema title=%v want ApprovalEnvelope", doc["title"])
	}
}

// TestServer_AnswerRejectsExpiredEnvelope guards fix-answer-accepts-expired-envelope:
// per protocol.ApprovalEnvelope.ExpiresAt the daemon must NOT accept an answer past
// that time (the wrapper already acted on its default), so a late tap returns 410 and
// no answer is persisted as a misleading audit record.
func TestServer_AnswerRejectsExpiredEnvelope(t *testing.T) {
	ts, store, q := newTestServerWithQueue(t, "")
	_ = store.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "exp-1", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(-time.Hour), // already expired
	})
	sub, cancel := q.Subscribe()
	defer cancel()

	res, err := http.Post(
		ts.URL+"/api/queue/exp-1/answer",
		"application/json",
		strings.NewReader(`{"choice_key":"y"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Errorf("status=%d want 410 (expired envelope must not accept an answer)", res.StatusCode)
	}
	// The answer must NOT have been persisted — no bogus audit record for a
	// decision the wrapper never acted on.
	if _, gerr := store.GetAnswer("exp-1"); gerr == nil {
		t.Error("an answer was persisted for an expired envelope; expected none")
	}
	// A removal must be broadcast so stale tabs drop the dead card.
	select {
	case ev := <-sub:
		if ev.Kind != EventAnswered || ev.Answer == nil || ev.Answer.EnvelopeID != "exp-1" {
			t.Errorf("unexpected broadcast on expiry: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("expected a removal broadcast for the expired card")
	}
}

// TestServer_PostEnvelopeExpiredOnArrivalReturnsImmediately guards
// fix-postenvelope-blocks-full-ttl-on-expired: an envelope that is already past its
// ExpiresAt on arrival must return 504 immediately instead of blocking the wrapper
// for the full server TTL (EnvelopeTTL is 2s in the test harness).
func TestServer_PostEnvelopeExpiredOnArrivalReturnsImmediately(t *testing.T) {
	ts, _ := newTestServer(t, "")
	env := protocol.ApprovalEnvelope{
		ID: "past-1", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(-time.Minute), // already expired on arrival
	}
	body, _ := json.Marshal(env)
	start := time.Now()
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	elapsed := time.Since(start)
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status=%d want 504", res.StatusCode)
	}
	if elapsed > time.Second {
		t.Errorf("post blocked %v for an already-expired envelope; want an immediate return (< server TTL)", elapsed)
	}
}

// dialWS opens a WebSocket client connection to the test server's /ws
// endpoint. token is appended as ?t= when non-empty.
func dialWS(t *testing.T, httpURL, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
	if token != "" {
		wsURL += "?t=" + token
	}
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	return conn
}

// drainBootstrapEnvelopes reads frames from a freshly-dialed WS connection
// for up to timeout, returning the set of envelope IDs seen in `envelope`
// (EventNewEnvelope) frames. The daemon pushes the current live queue as a
// burst of `envelope` frames immediately after connect (server.go
// websocketHandler) before any live events; this drains that burst. Once the
// burst is exhausted ReadMessage blocks until the deadline, which is how the
// caller observes "the snapshot is over".
func drainBootstrapEnvelopes(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break // deadline or closed — burst drained
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Kind == EventNewEnvelope && ev.Envelope != nil {
			seen[ev.Envelope.ID] = true
		}
	}
	return seen
}

// TestServer_ReconnectBootstrapSnapshotIsResyncSource guards
// fix-ws-reconnect-leaves-stale-cards. The client contract the fix relies on
// is that GET /api/queue and the WebSocket bootstrap snapshot are the
// authoritative resync source: they reflect ONLY live envelopes, so a
// reconnecting phone that re-fetches /api/queue (app.js syncQueueSnapshot on
// ws.onopen) can correctly drop cards that were answered/expired/evicted
// during the disconnect. The daemon's WS handler does NOT replay the
// answer/removal events the phone missed (the prior subscriber was cancelled
// when the old handler returned); the bootstrap snapshot is the resync.
//
// Scenario: the phone knows about card A, then the WS drops. While
// disconnected, A is answered by another phone (202 — persisted for audit, no
// live waiter) and a fresh card B is posted and evicted by a wrapper timeout
// (504 — deleted from the store). On reconnect, both the fresh WS bootstrap
// burst and GET /api/queue must exclude A (answered → filtered by
// ListEnvelopes) and B (evicted → deleted), so the phone's syncQueueSnapshot
// drops the stale A and never re-adds B.
func TestServer_ReconnectBootstrapSnapshotIsResyncSource(t *testing.T) {
	ts, store := newTestServer(t, "")

	// A live card the phone knows about before the drop.
	envA := &protocol.ApprovalEnvelope{
		ID: "resync-A", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.PutEnvelope(envA); err != nil {
		t.Fatalf("PutEnvelope A: %v", err)
	}

	// Initial connect: the bootstrap snapshot must carry A so the phone knows it.
	ws1 := dialWS(t, ts.URL, "")
	defer ws1.Close()
	seen1 := drainBootstrapEnvelopes(t, ws1, 500*time.Millisecond)
	if !seen1["resync-A"] {
		t.Fatalf("initial bootstrap snapshot=%v; want resync-A (phone must know A before the drop)", seen1)
	}

	// --- Phone disconnects (screen off / WS drop). The prior subscriber is
	// cancelled when ws1's handler returns, so events broadcast during this
	// window never reach the phone. ---
	_ = ws1.Close()
	time.Sleep(50 * time.Millisecond) // let the server tear down ws1's subscriber

	// --- While disconnected: A is answered by another phone, and a fresh card
	// B is posted then evicted by a wrapper timeout. Neither event reaches the
	// phone; the resync must come from the bootstrap snapshot + GET /api/queue
	// on reconnect. ---
	ansRes, err := http.Post(ts.URL+"/api/queue/resync-A/answer",
		"application/json", strings.NewReader(`{"choice_key":"y"}`))
	if err != nil {
		t.Fatalf("answer A: %v", err)
	}
	if ansRes.StatusCode != http.StatusAccepted {
		t.Fatalf("answer A status=%d want 202 (persisted for audit, no live waiter)", ansRes.StatusCode)
	}
	ansRes.Body.Close()

	envB := protocol.ApprovalEnvelope{
		ID: "resync-B", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y"}},
		ExpiresAt: time.Now().Add(150 * time.Millisecond), // short → POST times out and evicts
	}
	bBody, _ := json.Marshal(envB)
	postRes, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(bBody))
	if err != nil {
		t.Fatalf("post B: %v", err)
	}
	if postRes.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("post B status=%d want 504 (evicted on wrapper timeout)", postRes.StatusCode)
	}
	postRes.Body.Close()

	// --- Reconnect: a fresh WS subscriber. The bootstrap snapshot is the
	// resync source and must exclude the answered A and the evicted B. ---
	ws2 := dialWS(t, ts.URL, "")
	defer ws2.Close()
	seen2 := drainBootstrapEnvelopes(t, ws2, 500*time.Millisecond)
	if seen2["resync-A"] || seen2["resync-B"] {
		t.Fatalf("reconnect bootstrap snapshot=%v; want neither resync-A (answered) nor resync-B (evicted) — the snapshot is the resync source and must exclude dead cards", seen2)
	}

	// The same live state must be visible via the REST snapshot app.js
	// re-fetches in syncQueueSnapshot on reconnect.
	listRes, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("queue list: %v", err)
	}
	defer listRes.Body.Close()
	var list []protocol.ApprovalEnvelope
	if err := json.NewDecoder(listRes.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range list {
		if e.ID == "resync-A" || e.ID == "resync-B" {
			t.Fatalf("GET /api/queue=%+v; want neither resync-A nor resync-B (REST snapshot must match the resync source)", list)
		}
	}
}

// TestServer_PostEnvelopeTimeoutNoExpiryLeavesStore guards
// fix-postenvelope-timeout-leaves-noexpiry-card-in-store: an envelope POSTed
// WITHOUT an expires_at (allowed by the published schema for third-party
// producers) whose POST times out on the server EnvelopeTTL must be evicted from
// the store — not left to resurrect in GET /api/queue and the WebSocket bootstrap
// snapshot forever. ListEnvelopes filters only on ExpiresAt, so before the fix a
// zero-ExpiresAt dead card lingered indefinitely.
func TestServer_PostEnvelopeTimeoutNoExpiryLeavesStore(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := NewServer(Config{
		Store:       store,
		Queue:       NewQueue(),
		EnvelopeTTL: 60 * time.Millisecond, // short so the POST times out quickly
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// No ExpiresAt: the server TTL governs, and on timeout the zero ExpiresAt must
	// not keep the card alive in the list.
	env := protocol.ApprovalEnvelope{
		ID: "noexp-1", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}},
	}
	body, _ := json.Marshal(env)
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504", res.StatusCode)
	}

	// The timed-out, unanswered card must be gone from the live queue snapshot.
	lres, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("queue list: %v", err)
	}
	defer lres.Body.Close()
	var list []protocol.ApprovalEnvelope
	if err := json.NewDecoder(lres.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("queue=%+v want empty (timed-out no-expiry card must be evicted)", list)
	}
	// And directly gone from the store.
	if _, gerr := store.GetEnvelope("noexp-1"); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("GetEnvelope after timeout err=%v want ErrNotFound", gerr)
	}
}

// newTestServerWithRules builds an unauthenticated test server whose postEnvelope
// path consults the given auto-approve rules. Returns the queue so a test can
// assert broadcasts / Pending state.
func newTestServerWithRules(t *testing.T, rules *AutoApproveRules) (*httptest.Server, *Store, *Queue) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	q := NewQueue()
	srv := NewServer(Config{
		Store:       store,
		Queue:       q,
		EnvelopeTTL: 2 * time.Second,
		AutoApprove: rules,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store, q
}

// TestServer_PostEnvelopeAutoApprovesMatchingPrompt guards m_auto_approve_rules:
// a prompt matching a configured --auto-approve rule is answered with the rule's
// choice and returns 200 immediately, WITHOUT a human tap on the phone queue.
// Before the feature the POST would block on Queue.Wait until the server TTL
// and return 504. The answered card is excluded from the live queue snapshot
// (ListEnvelopes filters answered envelopes), recorded for audit, and an
// EventAnswered is broadcast so the PutEnvelope->PutAnswerIfAbsent race window
// can't leave a stale card on a connected phone.
func TestServer_PostEnvelopeAutoApprovesMatchingPrompt(t *testing.T) {
	rules, err := ParseAutoApproveRules([]string{"make *:y"})
	if err != nil {
		t.Fatalf("ParseAutoApproveRules: %v", err)
	}
	ts, store, q := newTestServerWithRules(t, rules)
	sub, cancel := q.Subscribe()
	defer cancel()

	env := protocol.ApprovalEnvelope{
		ID: "auto-1", AgentID: "a", Prompt: "make test",
		Choices:   []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}, {Key: "n", Label: "Deny"}},
		ExpiresAt: time.Now().Add(time.Minute),
	}
	body, _ := json.Marshal(env)
	start := time.Now()
	res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 (auto-approved, no phone tap); a 504 would mean the rule did not short-circuit", res.StatusCode)
	}
	var ans protocol.Answer
	if err := json.NewDecoder(res.Body).Decode(&ans); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if ans.EnvelopeID != "auto-1" || ans.ChoiceKey != "y" {
		t.Fatalf("answer=%+v want {EnvelopeID:auto-1 ChoiceKey:y}", ans)
	}
	// Auto-approve must be near-instant, not block for the server TTL.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("auto-approve returned in %v; want immediate (< server TTL, no phone tap)", elapsed)
	}

	// The auto-approved card must NOT appear in the live queue (answered -> filtered).
	listRes, err := http.Get(ts.URL + "/api/queue")
	if err != nil {
		t.Fatalf("queue list: %v", err)
	}
	defer listRes.Body.Close()
	var list []protocol.ApprovalEnvelope
	_ = json.NewDecoder(listRes.Body).Decode(&list)
	for _, e := range list {
		if e.ID == "auto-1" {
			t.Errorf("auto-approved card still in queue: %+v", list)
		}
	}

	// The audit answer must be recorded with the rule's choice.
	stored, gerr := store.GetAnswer("auto-1")
	if gerr != nil || stored.ChoiceKey != "y" {
		t.Errorf("audit answer=%+v err=%v want ChoiceKey=y", stored, gerr)
	}

	// A connected phone must be told to drop the card (BroadcastAnswered) so the
	// PutEnvelope->PutAnswerIfAbsent race window can't leave a stale card.
	select {
	case ev := <-sub:
		if ev.Kind != EventAnswered || ev.Answer == nil || ev.Answer.EnvelopeID != "auto-1" || ev.Answer.ChoiceKey != "y" {
			t.Errorf("unexpected broadcast: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("expected an EventAnswered broadcast for the auto-approved card")
	}
}

// TestServer_PostEnvelopeNonMatchingPromptStillCardsPhone guards m_auto_approve_rules:
// a prompt that does NOT match any auto-approve rule follows the unchanged
// human-triage path — the POST BLOCKS until a human answers via the phone, and
// the wrapper receives the HUMAN's choice (not a rule choice). This proves the
// auto-approve branch only short-circuits matching prompts; an unrelated `rm
// -rf` prompt still cards the phone (the plan's "Done" criterion).
func TestServer_PostEnvelopeNonMatchingPromptStillCardsPhone(t *testing.T) {
	rules, err := ParseAutoApproveRules([]string{"make *:y"})
	if err != nil {
		t.Fatalf("ParseAutoApproveRules: %v", err)
	}
	ts, store, _ := newTestServerWithRules(t, rules)

	env := protocol.ApprovalEnvelope{
		ID: "human-1", AgentID: "a", Prompt: "rm -rf /tmp",
		Choices:   []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}, {Key: "n", Label: "Deny"}},
		ExpiresAt: time.Now().Add(time.Minute),
	}
	body, _ := json.Marshal(env)

	type postResult struct {
		ans protocol.Answer
		err error
	}
	postCh := make(chan postResult, 1)
	go func() {
		res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
		if err != nil {
			postCh <- postResult{err: err}
			return
		}
		defer res.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		if res.StatusCode != http.StatusOK {
			postCh <- postResult{err: fmt.Errorf("post status=%d body=%s", res.StatusCode, buf.String())}
			return
		}
		var ans protocol.Answer
		if err := json.Unmarshal(buf.Bytes(), &ans); err != nil {
			postCh <- postResult{err: err}
			return
		}
		postCh <- postResult{ans: ans}
	}()

	// The envelope must persist (the auto-approve branch did NOT short-circuit;
	// the card entered the human-triage queue and the POST is now blocking on
	// Queue.Wait).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, gerr := store.GetEnvelope(env.ID); gerr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("envelope never persisted (auto-approve may have short-circuited a non-matching prompt)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The POST must still be blocked — a non-matching prompt is NOT auto-approved.
	select {
	case got := <-postCh:
		t.Fatalf("non-matching prompt returned immediately with %+v; want it to block on the phone queue", got)
	case <-time.After(200 * time.Millisecond):
	}

	// The human taps "n" (Deny) on the phone. Asserting the POST returns the
	// HUMAN choice "n" (not a rule choice) proves the card went through the
	// queue, not the auto-approve branch.
	res, err := http.Post(
		ts.URL+"/api/queue/human-1/answer",
		"application/json",
		strings.NewReader(`{"choice_key":"n"}`),
	)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("answer status=%d want 200", res.StatusCode)
	}
	select {
	case got := <-postCh:
		if got.err != nil {
			t.Fatalf("post envelope: %v", got.err)
		}
		if got.ans.ChoiceKey != "n" || got.ans.EnvelopeID != "human-1" {
			t.Errorf("answer=%+v want the HUMAN choice n (not a rule choice)", got.ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post envelope did not return after the human answer (the card may not have entered the queue)")
	}
}

// TestServer_AutoApproveRuleChoiceNotInEnvelopeFallsThrough guards m_auto_approve_rules:
// a rule whose Choice is not among the envelope's choices must NOT auto-approve
// with an invalid key — it falls through to the unchanged human-triage path so
// the operator still taps the phone. This is the choiceKnown guard in
// postEnvelope, which keeps the auto-approve path from answering with a key the
// wrapped agent would reject.
func TestServer_AutoApproveRuleChoiceNotInEnvelopeFallsThrough(t *testing.T) {
	rules, err := ParseAutoApproveRules([]string{"make *:z"}) // "z" is not a choice below
	if err != nil {
		t.Fatalf("ParseAutoApproveRules: %v", err)
	}
	ts, _, q := newTestServerWithRules(t, rules)

	env := protocol.ApprovalEnvelope{
		ID: "fall-1", AgentID: "a", Prompt: "make test",
		Choices:   []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}, {Key: "n", Label: "Deny"}},
		ExpiresAt: time.Now().Add(time.Minute),
	}
	body, _ := json.Marshal(env)

	type postResult struct {
		ans protocol.Answer
		err error
	}
	postCh := make(chan postResult, 1)
	go func() {
		res, err := http.Post(ts.URL+"/api/envelopes", "application/json", bytes.NewReader(body))
		if err != nil {
			postCh <- postResult{err: err}
			return
		}
		defer res.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		if res.StatusCode != http.StatusOK {
			postCh <- postResult{err: fmt.Errorf("post status=%d body=%s", res.StatusCode, buf.String())}
			return
		}
		var ans protocol.Answer
		_ = json.Unmarshal(buf.Bytes(), &ans)
		postCh <- postResult{ans: ans}
	}()

	// The prompt matches "make *" but the rule's choice "z" is not in {y,n}, so
	// the auto-approve branch must fall through to human triage: the card must
	// enter the queue (Pending) and the POST must keep blocking.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if q.Pending("fall-1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("card did not enter the queue; auto-approve with an invalid choice must fall through to human triage")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case got := <-postCh:
		t.Fatalf("auto-approve returned immediately with an invalid choice %+v; want it to fall through to human triage", got)
	case <-time.After(150 * time.Millisecond):
		// good: still blocked on Queue.Wait
	}

	// The human answers with a VALID choice; the POST must then return it.
	res, err := http.Post(
		ts.URL+"/api/queue/fall-1/answer",
		"application/json",
		strings.NewReader(`{"choice_key":"y"}`),
	)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("answer status=%d want 200", res.StatusCode)
	}
	select {
	case got := <-postCh:
		if got.err != nil {
			t.Fatalf("post envelope: %v", got.err)
		}
		if got.ans.ChoiceKey != "y" {
			t.Errorf("answer=%+v want ChoiceKey=y (the human's valid choice, not the invalid rule choice z)", got.ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post envelope did not return after the human answer")
	}
}
