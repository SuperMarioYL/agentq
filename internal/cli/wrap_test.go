package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

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
