package cli

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// TestDaemonForwarder_PostsEnvelopeAndReturnsAnswer guards the m7 transport
// adapter: an envelope written to the forwarder (as the wrapper does via its
// EnvelopeOut) is POSTed to /api/envelopes, and the answer the daemon returns is
// surfaced back on the forwarder's Read side (the wrapper's AnswerIn). The
// ApprovalEnvelope + Answer wire formats are unchanged; only the transport is new.
func TestDaemonForwarder_PostsEnvelopeAndReturnsAnswer(t *testing.T) {
	// A stub daemon that answers any posted envelope with choice "y".
	var gotEnvelope protocol.ApprovalEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/envelopes" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotEnvelope); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ans := protocol.Answer{EnvelopeID: gotEnvelope.ID, ChoiceKey: "y", AnsweredAt: time.Now().UTC()}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ans)
	}))
	defer srv.Close()

	fwd := newDaemonForwarder(srv.URL, "")
	defer fwd.Close()

	// Write an envelope exactly as the wrapper's json.Encoder would (object + \n).
	env := protocol.ApprovalEnvelope{
		ID: "01FWD", AgentID: "claude-1", Prompt: "ok?",
		Choices: []protocol.Choice{{Key: "y", IsDefault: true}, {Key: "n"}},
	}
	line, _ := json.Marshal(env)
	line = append(line, '\n')
	if _, err := fwd.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The answer should arrive on the Read side.
	r := bufio.NewReader(fwd)
	ansCh := make(chan protocol.Answer, 1)
	errCh := make(chan error, 1)
	go func() {
		var a protocol.Answer
		if err := json.NewDecoder(r).Decode(&a); err != nil {
			errCh <- err
			return
		}
		ansCh <- a
	}()

	select {
	case a := <-ansCh:
		if a.EnvelopeID != "01FWD" || a.ChoiceKey != "y" {
			t.Errorf("answer=%+v want {01FWD,y}", a)
		}
	case err := <-errCh:
		t.Fatalf("decode answer: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no answer surfaced from forwarder")
	}

	if gotEnvelope.ID != "01FWD" {
		t.Errorf("daemon received envelope id=%q want 01FWD", gotEnvelope.ID)
	}
}

// TestDaemonForwarder_TimeoutFallsBackToDefault verifies that when the daemon
// returns 504 (no answer within TTL), the forwarder synthesizes the default
// choice so the wrapper still unblocks.
func TestDaemonForwarder_TimeoutFallsBackToDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no answer within ttl", http.StatusGatewayTimeout)
	}))
	defer srv.Close()

	fwd := newDaemonForwarder(srv.URL, "")
	defer fwd.Close()

	env := protocol.ApprovalEnvelope{
		ID: "01TO", AgentID: "a", Prompt: "p",
		Choices: []protocol.Choice{{Key: "y"}, {Key: "n", IsDefault: true}},
	}
	line, _ := json.Marshal(env)
	line = append(line, '\n')
	if _, err := fwd.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	ansCh := make(chan protocol.Answer, 1)
	go func() {
		var a protocol.Answer
		if err := json.NewDecoder(bufio.NewReader(fwd)).Decode(&a); err == nil {
			ansCh <- a
		}
	}()
	select {
	case a := <-ansCh:
		if a.ChoiceKey != "n" {
			t.Errorf("fallback ChoiceKey=%q want default n", a.ChoiceKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no fallback answer on 504")
	}
}
