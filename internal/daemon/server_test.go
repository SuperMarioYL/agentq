package daemon

import (
	"bytes"
	"encoding/json"
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
	store, err := OpenStore(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := NewServer(Config{
		Token:       token,
		Store:       store,
		Queue:       NewQueue(),
		EnvelopeTTL: 2 * time.Second,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
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
