package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

func TestDefaultMatcher(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantMatch  bool
		wantPrompt string
		wantKeys   []string
		wantDflt   string // key expected to have IsDefault=true
	}{
		{
			name:       "y_N_default_no",
			line:       "Allow this command? [y/N]",
			wantMatch:  true,
			wantPrompt: "Allow this command?",
			wantKeys:   []string{"y", "n"},
			wantDflt:   "n",
		},
		{
			name:       "three_choices_explicit_default",
			line:       "Continue? [y/n/A]",
			wantMatch:  true,
			wantPrompt: "Continue?",
			wantKeys:   []string{"y", "n", "a"},
			wantDflt:   "a",
		},
		{
			name:       "no_uppercase_fallback_last",
			line:       "Pick one [y/n/a]",
			wantMatch:  true,
			wantPrompt: "Pick one",
			wantKeys:   []string{"y", "n", "a"},
			wantDflt:   "a",
		},
		{
			name:       "trailing_whitespace",
			line:       "  Are you sure? [Y/n]   ",
			wantMatch:  true,
			wantPrompt: "Are you sure?",
			wantKeys:   []string{"y", "n"},
			wantDflt:   "y",
		},
		{
			name:      "single_choice_not_a_prompt",
			line:      "Press [enter] to continue",
			wantMatch: false,
		},
		{
			name:      "plain_log_line",
			line:      "INFO: starting compile",
			wantMatch: false,
		},
		{
			name:      "bracket_no_slash",
			line:      "build complete [ok]",
			wantMatch: false,
		},
		{
			name:       "yes_no_words",
			line:       "Replace file? [yes/no]",
			wantMatch:  true,
			wantPrompt: "Replace file?",
			wantKeys:   []string{"yes", "no"},
			wantDflt:   "no",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, ok := DefaultMatcher(tc.line, "")
			if ok != tc.wantMatch {
				t.Fatalf("match=%v want=%v (line=%q)", ok, tc.wantMatch, tc.line)
			}
			if !tc.wantMatch {
				return
			}
			if env.Prompt != tc.wantPrompt {
				t.Errorf("prompt=%q want=%q", env.Prompt, tc.wantPrompt)
			}
			gotKeys := make([]string, len(env.Choices))
			for i, c := range env.Choices {
				gotKeys[i] = c.Key
			}
			if strings.Join(gotKeys, ",") != strings.Join(tc.wantKeys, ",") {
				t.Errorf("keys=%v want=%v", gotKeys, tc.wantKeys)
			}
			var gotDflt string
			defaultCount := 0
			for _, c := range env.Choices {
				if c.IsDefault {
					defaultCount++
					gotDflt = c.Key
				}
			}
			if defaultCount != 1 {
				t.Errorf("expected exactly one default, got %d", defaultCount)
			}
			if gotDflt != tc.wantDflt {
				t.Errorf("default=%q want=%q", gotDflt, tc.wantDflt)
			}
		})
	}
}

func TestChoiceLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"y", "Approve"},
		{"Y", "Approve"},
		{"yes", "Approve"},
		{"n", "Deny"},
		{"a", "Approve and remember"},
		{"always", "Approve and remember"},
		{"d", "Deny and remember"},
		{"s", "Skip"},
		{"retry", "Retry"},
		{"FOO", "Foo"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := choiceLabel(tc.in); got != tc.want {
				t.Errorf("choiceLabel(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWrapperProcess_EmitsEnvelopeAndForwardsAnswer(t *testing.T) {
	childOut := strings.NewReader("starting work\nAllow this command? [y/N]\nwork done\n")
	answers := strings.NewReader(`{"envelope_id":"FIXED-ID-1","choice_key":"y","answered_at":"2026-06-04T10:00:00Z"}` + "\n")

	var envOut, mirror, childIn bytes.Buffer
	fixedTime := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	w := &Wrapper{
		Cmd:         []string{"fake-agent"},
		AgentID:     "fake-agent-1",
		EnvelopeOut: &envOut,
		AnswerIn:    answers,
		Stdout:      &mirror,
		Expiry:      5 * time.Minute,
		Now:         func() time.Time { return fixedTime },
		NewID:       func() string { return "FIXED-ID-1" },
	}

	if err := w.Process(context.Background(), childOut, &childIn); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got, want := mirror.String(), "starting work\nAllow this command? [y/N]\nwork done\n"; got != want {
		t.Errorf("mirror=%q want=%q", got, want)
	}
	if got, want := childIn.String(), "y\n"; got != want {
		t.Errorf("childIn=%q want=%q", got, want)
	}

	var env protocol.ApprovalEnvelope
	if err := json.Unmarshal(envOut.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v\nraw=%s", err, envOut.String())
	}
	if env.ID != "FIXED-ID-1" {
		t.Errorf("ID=%q want=FIXED-ID-1", env.ID)
	}
	if env.AgentID != "fake-agent-1" {
		t.Errorf("AgentID=%q", env.AgentID)
	}
	if env.Prompt != "Allow this command?" {
		t.Errorf("Prompt=%q", env.Prompt)
	}
	if len(env.Choices) != 2 || env.Choices[0].Key != "y" || env.Choices[1].Key != "n" {
		t.Errorf("Choices=%+v", env.Choices)
	}
	if !env.ExpiresAt.Equal(fixedTime.Add(5 * time.Minute)) {
		t.Errorf("ExpiresAt=%v want=%v", env.ExpiresAt, fixedTime.Add(5*time.Minute))
	}
	if !strings.Contains(env.Context, "starting work") || !strings.Contains(env.Context, "Allow this command?") {
		t.Errorf("Context missing recent lines: %q", env.Context)
	}
}

func TestWrapperProcess_ErrorPaths(t *testing.T) {
	fixedTime := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	mkWrapper := func(answers io.Reader) (*Wrapper, *bytes.Buffer, *bytes.Buffer) {
		envOut := &bytes.Buffer{}
		childIn := &bytes.Buffer{}
		return &Wrapper{
			Cmd:         []string{"fake"},
			EnvelopeOut: envOut,
			AnswerIn:    answers,
			Stdout:      io.Discard,
			Now:         func() time.Time { return fixedTime },
			NewID:       func() string { return "FIXED-ID-1" },
		}, envOut, childIn
	}

	cases := []struct {
		name        string
		answers     io.Reader
		wantErrSubs string
	}{
		{
			name:        "answer_id_mismatch",
			answers:     strings.NewReader(`{"envelope_id":"OTHER","choice_key":"y"}` + "\n"),
			wantErrSubs: "does not match pending",
		},
		{
			name:        "unknown_choice_key",
			answers:     strings.NewReader(`{"envelope_id":"FIXED-ID-1","choice_key":"maybe"}` + "\n"),
			wantErrSubs: `choice "maybe" not in envelope`,
		},
		{
			name:        "nil_answer_source",
			answers:     nil,
			wantErrSubs: "AnswerIn is nil",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _, childIn := mkWrapper(tc.answers)
			err := w.Process(context.Background(), strings.NewReader("Do it? [y/N]\n"), childIn)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubs) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErrSubs)
			}
		})
	}
}

func TestWrapperProcess_ContextBounded(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line")
	}
	lines = append(lines, "Allow? [y/N]")
	stdin := strings.NewReader(strings.Join(lines, "\n") + "\n")
	answers := strings.NewReader(`{"envelope_id":"ID","choice_key":"n"}` + "\n")

	var envOut bytes.Buffer
	w := &Wrapper{
		Cmd:          []string{"x"},
		EnvelopeOut:  &envOut,
		AnswerIn:     answers,
		Stdout:       io.Discard,
		ContextLines: 5,
		NewID:        func() string { return "ID" },
		Now:          func() time.Time { return time.Unix(0, 0) },
	}
	if err := w.Process(context.Background(), stdin, io.Discard); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env protocol.ApprovalEnvelope
	if err := json.Unmarshal(envOut.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotLines := strings.Count(env.Context, "\n") + 1
	if gotLines != 5 {
		t.Errorf("context lines=%d want=5 (%q)", gotLines, env.Context)
	}
}

func TestWrapperProcess_ContextTruncatedToMaxBytes(t *testing.T) {
	big := strings.Repeat("x", protocol.MaxContextBytes*2)
	in := big + "\nGo? [y/N]\n"
	answers := strings.NewReader(`{"envelope_id":"ID","choice_key":"n"}` + "\n")
	var envOut bytes.Buffer
	w := &Wrapper{
		Cmd:         []string{"x"},
		EnvelopeOut: &envOut,
		AnswerIn:    answers,
		Stdout:      io.Discard,
		NewID:       func() string { return "ID" },
		Now:         func() time.Time { return time.Unix(0, 0) },
	}
	if err := w.Process(context.Background(), strings.NewReader(in), io.Discard); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var env protocol.ApprovalEnvelope
	_ = json.Unmarshal(envOut.Bytes(), &env)
	if len(env.Context) > protocol.MaxContextBytes {
		t.Errorf("context len=%d exceeds %d", len(env.Context), protocol.MaxContextBytes)
	}
}

func TestWrapperProcess_NoPromptsPassesThrough(t *testing.T) {
	in := "hello\nworld\n"
	var mirror bytes.Buffer
	w := &Wrapper{
		Cmd:    []string{"x"},
		Stdout: &mirror,
	}
	if err := w.Process(context.Background(), strings.NewReader(in), io.Discard); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if mirror.String() != in {
		t.Errorf("mirror=%q want=%q", mirror.String(), in)
	}
}

func TestWrapperApplyDefaults(t *testing.T) {
	w := &Wrapper{Cmd: []string{"claude"}, NewID: func() string { return "ABCDEFGHIJK" }}
	w.applyDefaults()
	if w.ContextLines != 20 {
		t.Errorf("ContextLines=%d", w.ContextLines)
	}
	if w.Expiry != protocol.DefaultExpiry {
		t.Errorf("Expiry=%v", w.Expiry)
	}
	if len(w.Matchers) == 0 {
		t.Error("Matchers empty")
	}
	if !strings.HasPrefix(w.AgentID, "claude-") {
		t.Errorf("AgentID=%q want prefix claude-", w.AgentID)
	}
	// idempotent
	prev := w.AgentID
	w.applyDefaults()
	if w.AgentID != prev {
		t.Errorf("AgentID changed on second applyDefaults: %q→%q", prev, w.AgentID)
	}
}

func TestNewULID_ShapeAndUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		u := NewULID()
		if len(u) != 26 {
			t.Fatalf("len(%q)=%d want=26", u, len(u))
		}
		for _, r := range u {
			if !strings.ContainsRune(crockfordAlpha, r) {
				t.Fatalf("rune %q in %q not in alphabet", r, u)
			}
		}
		if _, dup := seen[u]; dup {
			t.Fatalf("duplicate ULID %q at i=%d", u, i)
		}
		seen[u] = struct{}{}
	}
}

// TestNewULID_Monotonic guards the queue-ordering invariant: IDs minted in a
// tight loop (many within the same millisecond) must be strictly increasing
// in lexicographic order, because the bbolt store lists envelopes by
// ascending ID and treats that as arrival order. The pre-fix factory redrew
// random entropy each call, so same-ms IDs sorted arbitrarily.
func TestNewULID_Monotonic(t *testing.T) {
	const n = 5000
	prev := NewULID()
	for i := 1; i < n; i++ {
		u := NewULID()
		if u <= prev {
			t.Fatalf("ULID not monotonic at i=%d: %q <= %q", i, u, prev)
		}
		prev = u
	}
}

// TestNewULID_ConcurrentMonotonic confirms the factory stays monotonic and
// unique under concurrent callers (the real topology: N wrapped agents plus
// the daemon minting IDs at once). Run with -race.
func TestNewULID_ConcurrentMonotonic(t *testing.T) {
	const goroutines, per = 16, 500
	out := make(chan string, goroutines*per)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				out <- NewULID()
			}
		}()
	}
	wg.Wait()
	close(out)
	seen := make(map[string]struct{}, goroutines*per)
	for u := range out {
		if len(u) != 26 {
			t.Fatalf("len(%q)=%d want=26", u, len(u))
		}
		if _, dup := seen[u]; dup {
			t.Fatalf("duplicate ULID %q under concurrency", u)
		}
		seen[u] = struct{}{}
	}
	if len(seen) != goroutines*per {
		t.Fatalf("got %d unique IDs want %d", len(seen), goroutines*per)
	}
}
