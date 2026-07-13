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
