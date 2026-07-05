package cli

import (
	"context"
	"errors"
	"testing"
)

// TestEnsureDaemon_ReusesWhenProbeSucceeds guards m7_wrap_daemon_integration:
// when a daemon already answers at the port, ensureDaemon must REUSE it — no new
// daemon started, Reused=true, and the configured token carried through.
func TestEnsureDaemon_ReusesWhenProbeSucceeds(t *testing.T) {
	started := false
	boot := daemonBootstrap{
		Listen: "127.0.0.1:7777",
		Token:  "tok-abc",
		probe:  func(string) bool { return true }, // a daemon is already up
		start: func(context.Context, string, string) (func(), error) {
			started = true
			return func() {}, nil
		},
	}
	h, err := boot.ensureDaemon(context.Background())
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if started {
		t.Fatal("start called even though a daemon was already running (should reuse)")
	}
	if !h.Reused {
		t.Error("Reused=false, want true")
	}
	if h.BaseURL != "http://127.0.0.1:7777" {
		t.Errorf("BaseURL=%q", h.BaseURL)
	}
	if h.Token != "tok-abc" {
		t.Errorf("Token=%q want tok-abc", h.Token)
	}
	h.Close() // must be a safe no-op for a reused daemon
}

// TestEnsureDaemon_StartsWhenProbeFails guards the other branch: with no daemon
// on the port, ensureDaemon must START one, mark Reused=false, and its Close
// must invoke the returned stop func exactly once.
func TestEnsureDaemon_StartsWhenProbeFails(t *testing.T) {
	startCalls := 0
	stopCalls := 0
	var gotListen, gotToken string
	boot := daemonBootstrap{
		Listen: "127.0.0.1:9999",
		Token:  "tok-xyz",
		probe:  func(string) bool { return false }, // nothing listening
		start: func(_ context.Context, listen, token string) (func(), error) {
			startCalls++
			gotListen, gotToken = listen, token
			return func() { stopCalls++ }, nil
		},
	}
	h, err := boot.ensureDaemon(context.Background())
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("start called %d times want 1", startCalls)
	}
	if h.Reused {
		t.Error("Reused=true, want false (fresh start)")
	}
	if gotListen != "127.0.0.1:9999" || gotToken != "tok-xyz" {
		t.Errorf("start got listen=%q token=%q", gotListen, gotToken)
	}
	h.Close()
	h.Close() // idempotent
	if stopCalls != 1 {
		t.Errorf("stop called %d times want exactly 1", stopCalls)
	}
}

// TestEnsureDaemon_MintsTokenWhenAbsent verifies a fresh start with no explicit
// token gets a generated one passed to start (so the daemon is not left open).
func TestEnsureDaemon_MintsTokenWhenAbsent(t *testing.T) {
	var gotToken string
	boot := daemonBootstrap{
		Listen:   "127.0.0.1:8000",
		Token:    "", // none provided
		probe:    func(string) bool { return false },
		newToken: func() (string, error) { return "minted-123", nil },
		start: func(_ context.Context, _, token string) (func(), error) {
			gotToken = token
			return func() {}, nil
		},
	}
	h, err := boot.ensureDaemon(context.Background())
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if gotToken != "minted-123" {
		t.Errorf("start token=%q want minted-123", gotToken)
	}
	if h.Token != "minted-123" {
		t.Errorf("handle Token=%q want minted-123", h.Token)
	}
}

// TestEnsureDaemon_PropagatesStartError ensures a failed daemon start surfaces as
// an error rather than a broken handle.
func TestEnsureDaemon_PropagatesStartError(t *testing.T) {
	boot := daemonBootstrap{
		Listen: "127.0.0.1:8001",
		Token:  "t",
		probe:  func(string) bool { return false },
		start: func(context.Context, string, string) (func(), error) {
			return nil, errors.New("bind failed")
		},
	}
	if _, err := boot.ensureDaemon(context.Background()); err == nil {
		t.Fatal("expected error when start fails")
	}
}

// TestProbeDaemon_NoListenerIsFalse confirms probeDaemon reports false when
// nothing is bound (so wrap starts its own daemon).
func TestProbeDaemon_NoListenerIsFalse(t *testing.T) {
	// A port very unlikely to be bound on the test host.
	if probeDaemon("127.0.0.1:1") {
		t.Error("probeDaemon reported a daemon on an unbound port")
	}
}
