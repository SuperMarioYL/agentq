package cli

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestDaemonForwarder_AuthFailureSurfacedNotSwallowed guards
// fix-forwarder-swallows-daemon-post-errors: when the daemon returns 401 (the
// real failure when `agentq wrap --daemon` reuses a separately-started
// `agentq serve` without --daemon-token, so the POST carries no ?t= and
// authMiddleware rejects it), the forwarder must surface the error and close
// the answer pipe instead of silently synthesizing the default choice for every
// prompt — which would bypass approvals with zero signal. The pre-fix code
// turned EVERY postEnvelope error into defaultAnswer(env), so this test would
// see a clean default answer and never fail.
func TestDaemonForwarder_AuthFailureSurfacedNotSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized) // 401
	}))
	defer srv.Close()

	fwd := newDaemonForwarder(srv.URL, "")
	defer fwd.Close()

	// Far-future expiry so the per-request client timeout never fires here;
	// the 401 response is what this test exercises.
	env := protocol.ApprovalEnvelope{
		ID: "01AUTH", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y", IsDefault: true}, {Key: "n"}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	line, _ := json.Marshal(env)
	line = append(line, '\n')
	if _, err := fwd.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Without the fix, the 401 is swallowed and a default answer ("y") lands on
	// the pipe; with the fix, the pipe is closed with the 404/401 error and the
	// decoder surfaces it.
	var ans protocol.Answer
	decErr := json.NewDecoder(bufio.NewReader(fwd)).Decode(&ans)
	if decErr == nil {
		t.Fatalf("expected the 401 to be surfaced as a read error, but got a silent default answer %+v (approvals bypassed with zero signal)", ans)
	}
	if !strings.Contains(decErr.Error(), "401") {
		t.Errorf("read error=%q want substring 401", decErr.Error())
	}
}

// TestDaemonForwarder_ConnectionRefusedSurfaced guards the transport-error path
// of fix-forwarder-swallows-daemon-post-errors: when the daemon is down (the TCP
// connect fails — e.g. the separately-started `agentq serve` crashed
// mid-session or the network dropped), the forwarder must surface the
// connection error instead of silently auto-defaulting every prompt.
func TestDaemonForwarder_ConnectionRefusedSurfaced(t *testing.T) {
	// A server we immediately tear down so its old address refuses connections.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := srv.URL
	srv.Close()

	fwd := newDaemonForwarder(baseURL, "")
	defer fwd.Close()

	env := protocol.ApprovalEnvelope{
		ID: "01REFUSED", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y", IsDefault: true}, {Key: "n"}},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	line, _ := json.Marshal(env)
	line = append(line, '\n')
	if _, err := fwd.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	decErr := json.NewDecoder(bufio.NewReader(fwd)).Decode(&protocol.Answer{})
	if decErr == nil {
		t.Fatal("expected connection-refused to be surfaced as a read error, got a silent default answer (approvals bypassed)")
	}
}

// TestDaemonForwarder_HungDaemonTimesOutAndSurfaced guards the client-timeout
// half of fix-forwarder-swallows-daemon-post-errors: when the daemon's TCP is
// alive but it never responds (a HUNG daemon), the per-request timeout (set
// slightly beyond env.ExpiresAt) must fire so forwardOne surfaces the stall
// instead of blocking forever — the pre-fix http.Client had no timeout, so a
// hung daemon stalled the wrapper for 10 min/prompt (its own envelope expiry)
// and leaked a goroutine per prompt.
func TestDaemonForwarder_HungDaemonTimesOutAndSurfaced(t *testing.T) {
	// Model a hung daemon: the server accepts the request but never responds
	// until the test releases the gate on cleanup.
	hungCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hungCh
	}))
	defer srv.Close()
	defer close(hungCh)

	fwd := newDaemonForwarder(srv.URL, "")
	defer fwd.Close()

	// Short expiry so the per-request timeout (expires_at + 2s) fires within a
	// couple of seconds instead of the 10-minute envelope TTL.
	env := protocol.ApprovalEnvelope{
		ID: "01HUNG", AgentID: "a", Prompt: "p",
		Choices:   []protocol.Choice{{Key: "y", IsDefault: true}, {Key: "n"}},
		ExpiresAt: time.Now().Add(100 * time.Millisecond),
	}
	line, _ := json.Marshal(env)
	line = append(line, '\n')
	if _, err := fwd.Write(line); err != nil {
		t.Fatalf("Write: %v", err)
	}

	decErrCh := make(chan error, 1)
	go func() {
		// ReadString returns the error when the pipe is closed with it; it
		// returns nil only if a full answer line was written (the silent
		// default-answer path the fix must eliminate).
		_, err := bufio.NewReader(fwd).ReadString('\n')
		decErrCh <- err
	}()

	select {
	case err := <-decErrCh:
		if err == nil {
			t.Fatal("expected the hung-daemon client timeout to be surfaced, got a clean answer (approvals bypassed)")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("forwarder stalled on hung daemon — no per-request client timeout (bug: leaks goroutine, stalls 10 min/prompt)")
	}
}
