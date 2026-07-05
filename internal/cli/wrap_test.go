package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

func TestNewWrapCmd_FlagsAndDefaults(t *testing.T) {
	cmd := NewWrapCmd()
	cases := []struct {
		name        string
		flag        string
		wantDefault string
	}{
		{"agent-id flag", "agent-id", ""},
		{"envelope-out flag", "envelope-out", ""},
		{"answer-in flag", "answer-in", ""},
		{"agent flag", "agent", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cmd.Flags().Lookup(tc.flag)
			if f == nil {
				t.Fatalf("flag %q missing", tc.flag)
			}
			if f.DefValue != tc.wantDefault {
				t.Errorf("flag %q default=%q want=%q", tc.flag, f.DefValue, tc.wantDefault)
			}
		})
	}
	if f := cmd.Flags().Lookup("expiry"); f == nil || f.DefValue != protocol.DefaultExpiry.String() {
		t.Errorf("expiry flag default=%v want=%v", f, protocol.DefaultExpiry)
	}
	// m7: the --daemon integration flags exist with sensible defaults.
	if f := cmd.Flags().Lookup("daemon"); f == nil || f.DefValue != "false" {
		t.Errorf("daemon flag=%v want default false", f)
	}
	if f := cmd.Flags().Lookup("daemon-listen"); f == nil || f.DefValue != "127.0.0.1:7777" {
		t.Errorf("daemon-listen flag=%v want default 127.0.0.1:7777", f)
	}
}

func TestNewWrapCmd_RequiresArgs(t *testing.T) {
	cmd := NewWrapCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no args given")
	}
}

func TestRunWrap_MissingCommand(t *testing.T) {
	err := RunWrap(context.Background(), WrapOptions{}, nil, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("err=%v want missing-command", err)
	}
}

func TestRunWrap_BadEnvelopePath(t *testing.T) {
	opts := WrapOptions{EnvelopeOut: filepath.Join(t.TempDir(), "missing-dir", "out.jsonl")}
	err := RunWrap(context.Background(), opts, []string{"true"}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "envelope-out") {
		t.Fatalf("err=%v want envelope-out failure", err)
	}
}

func TestRunWrap_BadAnswerPath(t *testing.T) {
	opts := WrapOptions{AnswerIn: filepath.Join(t.TempDir(), "no-such-file.jsonl")}
	err := RunWrap(context.Background(), opts, []string{"true"}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "answer-in") {
		t.Fatalf("err=%v want answer-in failure", err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestRunWrap_DaemonModeLandsCardWithNoPreRunningDaemon is the m7 "done"
// criterion end to end: `wrap --daemon -- <agent>` with NO daemon already
// running must start one in-process, wrap the agent, and land a card on the
// queue. We wrap a fake agent that emits a prompt then blocks; a background
// answerer approves the card; the wrapper then unblocks. This exercises the full
// bootstrap (start) + forward path, not just the helper in isolation.
func TestRunWrap_DaemonModeLandsCardWithNoPreRunningDaemon(t *testing.T) {
	listen := freePort(t)
	token := "e2e-token"
	// Point serve's data dir at a temp dir so we don't touch ~/.agentq.
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// A fake agent: print a prompt, then read one line of stdin (the wrapper
	// forwards the chosen key there) and exit. `head -n1` blocks until the
	// wrapper writes the answer, so the child stays alive until the card is
	// answered — mirroring a real agent waiting on a permission decision.
	agent := []string{"sh", "-c", "echo 'Allow deploy? [y/N]'; head -n1 >/dev/null"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Background answerer: once a card shows on the queue, approve it.
	go func() {
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get("http://" + listen + "/api/queue?t=" + token)
			if err != nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			var list []protocol.ApprovalEnvelope
			_ = json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			if len(list) == 0 {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			// A card landed — this is the milestone's success condition. Answer it
			// so the wrapper unblocks and RunWrap returns.
			id := list[0].ID
			body := strings.NewReader(`{"choice_key":"y"}`)
			ansResp, err := client.Post(
				"http://"+listen+"/api/queue/"+id+"/answer?t="+token,
				"application/json", body)
			if err == nil {
				ansResp.Body.Close()
			}
			return
		}
	}()

	opts := WrapOptions{
		Agent:        "claude",
		Expiry:       protocol.DefaultExpiry,
		Daemon:       true,
		DaemonListen: listen,
		DaemonToken:  token,
	}
	var out, errBuf bytes.Buffer
	if err := RunWrap(ctx, opts, agent, &out, &errBuf, strings.NewReader("")); err != nil {
		t.Fatalf("RunWrap --daemon: %v\nstdout=%s\nstderr=%s", err, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "started daemon at") {
		t.Errorf("expected 'started daemon' notice, got: %s", out.String())
	}
}
