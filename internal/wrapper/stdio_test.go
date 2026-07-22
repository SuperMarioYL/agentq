package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
			// fix-awaitanswer-leaked-decode-goroutine changed the semantics: an
			// answer whose EnvelopeID does not match the pending envelope is now
			// DROPPED as stale (the wrapper keeps waiting) instead of crashing
			// with "does not match pending". With no further answer on the
			// source, the long-lived reader then hits EOF and the read surfaces
			// as a "read answer for <id>" error — the new expected behavior.
			name:        "stale_answer_dropped_then_source_eof",
			answers:     strings.NewReader(`{"envelope_id":"OTHER","choice_key":"y"}` + "\n"),
			wantErrSubs: "read answer for",
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

// blockingReader never returns data and never EOFs until Close is called,
// modelling an answer source that has no answer yet — the exact condition the
// old blocking answerDec.Decode parked on forever.
type blockingReader struct {
	ch chan struct{}
}

func newBlockingReader() *blockingReader { return &blockingReader{ch: make(chan struct{})} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

func (b *blockingReader) Close() { close(b.ch) }

// TestWrapperProcess_CtxCancelUnblocksAnswerRead guards
// fix-wrap-answer-read-ignores-ctx-expiry-child-exit for the ctx path: while the
// wrapper is blocked reading an answer, cancelling ctx (Ctrl-C/SIGTERM) must
// unblock it and forward the envelope's default choice to the child instead of
// hanging until kill -9.
func TestWrapperProcess_CtxCancelUnblocksAnswerRead(t *testing.T) {
	answers := newBlockingReader()
	defer answers.Close()
	var childIn bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	// Default choice is n (uppercase N in [y/N]); expect n forwarded on cancel.
	w := &Wrapper{
		Cmd:         []string{"x"},
		EnvelopeOut: io.Discard,
		AnswerIn:    answers,
		Stdout:      io.Discard,
		Now:         time.Now, // real clock: far-future default expiry, so the timer never fires
		NewID:       func() string { return "ID" },
	}

	// Cancel shortly after Process starts blocking on the answer read.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- w.Process(ctx, strings.NewReader("Allow? [y/N]\n"), &childIn)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return after ctx cancel — answer read still blocking")
	}
	if got := childIn.String(); got != "n\n" {
		t.Errorf("childIn=%q want default choice n forwarded on cancel", got)
	}
}

// TestWrapperProcess_ExpiryUnblocksAnswerRead guards the envelope-expiry path:
// when ExpiresAt passes with no answer, the wrapper forwards the default choice
// (the "give up and abort" contract) rather than blocking forever.
func TestWrapperProcess_ExpiryUnblocksAnswerRead(t *testing.T) {
	answers := newBlockingReader()
	defer answers.Close()
	var childIn bytes.Buffer

	base := time.Now()
	// ExpiresAt is 60ms out relative to the wrapper's clock, so the expiry timer
	// fires while the answer read is blocked.
	w := &Wrapper{
		Cmd:         []string{"x"},
		EnvelopeOut: io.Discard,
		AnswerIn:    answers,
		Stdout:      io.Discard,
		Expiry:      60 * time.Millisecond,
		Now:         func() time.Time { return time.Now() }, // real clock consistent with ExpiresAt below
		NewID:       func() string { return "ID" },
	}
	_ = base

	done := make(chan error, 1)
	go func() {
		done <- w.Process(context.Background(), strings.NewReader("Proceed? [y/N]\n"), &childIn)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return after envelope expiry — answer read still blocking")
	}
	if got := childIn.String(); got != "n\n" {
		t.Errorf("childIn=%q want default choice n forwarded on expiry", got)
	}
}

// TestWrapperProcess_ChildDoneUnblocksAnswerRead guards the child-exit path:
// when the wrapped child dies mid-prompt, the wrapper must observe it and unblock
// the answer read (forwarding the default) instead of parking on stdin.
func TestWrapperProcess_ChildDoneUnblocksAnswerRead(t *testing.T) {
	answers := newBlockingReader()
	defer answers.Close()
	var childIn bytes.Buffer

	childDone := make(chan struct{})
	w := &Wrapper{
		Cmd:         []string{"x"},
		EnvelopeOut: io.Discard,
		AnswerIn:    answers,
		Stdout:      io.Discard,
		Now:         time.Now,
		NewID:       func() string { return "ID" },
		childDone:   childDone,
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(childDone) // child process exited
	}()

	done := make(chan error, 1)
	go func() {
		done <- w.Process(context.Background(), strings.NewReader("Run it? [y/N]\n"), &childIn)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Process did not return after child exit — answer read still blocking")
	}
	if got := childIn.String(); got != "n\n" {
		t.Errorf("childIn=%q want default choice n forwarded on child exit", got)
	}
}

// TestWrapperRun_ChildExitDoesNotHang is an end-to-end guard: Run wraps a real
// child that prints a prompt and then exits without any answer being provided.
// With the fix, Run returns promptly (child-exit cancels the answer read); the
// pre-fix code would block forever on the answer decode.
func TestWrapperRun_ChildExitDoesNotHang(t *testing.T) {
	// A shell that emits a prompt line then exits 0. No answer is ever supplied.
	answers := newBlockingReader()
	defer answers.Close()
	w := &Wrapper{
		Cmd:         []string{"sh", "-c", "echo 'Allow? [y/N]'; exit 0"},
		EnvelopeOut: io.Discard,
		AnswerIn:    answers,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	select {
	case <-done:
		// Returned — the child-exit signal unblocked the wrapper.
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung after child exit — answer read never cancelled")
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

// TestWrapperProcess_SecondPromptAfterTimeoutNoCrash guards
// fix-awaitanswer-leaked-decode-goroutine: when a prompt times out (no human
// answer) and a second prompt follows, the wrapper must NOT spawn a second
// decoder goroutine racing the first on the same *json.Decoder. A late answer
// to the timed-out first prompt must be DROPPED as stale (no
// EnvelopeID-mismatch crash), and the second prompt's real answer must be
// received and forwarded — no crash. The buggy per-call `go func(){ dec.Decode
// }()` leaked a parked decoder on the first timeout and let the second
// prompt's awaitAnswer spawn a second decoder on the same *json.Decoder, a
// data race on its internal buffer/scan state (run under -race to surface it).
func TestWrapperProcess_SecondPromptAfterTimeoutNoCrash(t *testing.T) {
	// Two prompts back-to-back; both default to N ([y/N]).
	childOut := strings.NewReader("Allow? [y/N]\nProceed? [y/N]\n")

	// Answer source is a pipe the test writes to with controlled timing: a
	// STALE answer for prompt 1 arrives well after prompt 1 has timed out
	// (so it must be dropped), then the real answer for prompt 2 — delivered
	// before prompt 2's own (later) expiry so the answer path is exercised.
	pr, pw := io.Pipe()

	var idCount int
	mkID := func() string {
		idCount++
		return fmt.Sprintf("ID-%d", idCount)
	}

	// Real advancing clock + 500ms expiry: prompt 1 is minted at T0 with
	// ExpiresAt=T0+500ms (times out at T0+500ms), and prompt 2 — matched only
	// AFTER prompt 1's timeout — is minted at T0+500ms with ExpiresAt=T0+1000ms.
	// So the stale ID-1 (written at ~600ms) lands after prompt 1 timed out but
	// the real ID-2 (written at ~650ms) lands well before prompt 2's timeout,
	// exercising the "drop stale, accept matching" path instead of a 2nd timeout.
	w := &Wrapper{
		Cmd:         []string{"x"},
		AgentID:     "x-1", // set explicitly so applyDefaults does not consume a NewID call
		EnvelopeOut: io.Discard,
		AnswerIn:    pr,
		Stdout:      io.Discard,
		Expiry:      500 * time.Millisecond,
		Now:         time.Now, // real advancing clock so the two prompts get distinct expiries
		NewID:       mkID,
	}

	var childIn bytes.Buffer
	procDone := make(chan error, 1)
	go func() { procDone <- w.Process(context.Background(), childOut, &childIn) }()

	// Wait past prompt 1's 500ms expiry so the stale answer below lands AFTER
	// awaitAnswer1 returned the default and Process is parked in awaitAnswer2 —
	// the exact race window the buggy per-call goroutine hit.
	time.Sleep(600 * time.Millisecond)

	// Stale answer for the timed-out prompt 1 — under the buggy per-call
	// goroutine this raced a second decoder on the same *json.Decoder and
	// either crashed with an EnvelopeID-mismatch or silently mis-routed.
	_, _ = pw.Write([]byte(`{"envelope_id":"ID-1","choice_key":"y"}` + "\n"))
	time.Sleep(50 * time.Millisecond)
	// Real answer for prompt 2 (still live: its 1000ms expiry is well away).
	_, _ = pw.Write([]byte(`{"envelope_id":"ID-2","choice_key":"y"}` + "\n"))
	_ = pw.Close()

	select {
	case err := <-procDone:
		if err != nil {
			t.Fatalf("Process returned error (bug: stale answer crashed wrapper): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Process did not return (bug: second prompt's answer never received)")
	}
	if got, want := childIn.String(), "n\ny\n"; got != want {
		t.Errorf("childIn=%q want %q (prompt1 default n + prompt2 real answer y)", got, want)
	}
}
