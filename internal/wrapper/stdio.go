// Package wrapper runs a coding-agent child process, watches its stdout
// for permission prompts, and converts each prompt into a protocol
// ApprovalEnvelope that an out-of-band human can answer.
//
// In milestone m1 there is no daemon: the wrapper emits envelopes as
// newline-delimited JSON to EnvelopeOut and reads Answer JSON from
// AnswerIn. m2 will swap those two streams for HTTP+WebSocket without
// changing the wire format.
package wrapper

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// PromptMatcher inspects a single line of agent stdout (with the rolling
// context snapshot for reference) and returns a partially-populated
// ApprovalEnvelope when the line is a permission prompt. The wrapper
// fills in ID, AgentID, SessionStarted, Context, and ExpiresAt afterward
// so matchers can stay stateless.
type PromptMatcher func(line, contextSnapshot string) (*protocol.ApprovalEnvelope, bool)

// Wrapper is the unit of one wrapped agent session. Zero-value fields
// get sensible defaults from applyDefaults; only Cmd is mandatory.
type Wrapper struct {
	// Cmd is the agent command followed by its args, e.g. {"claude", "code"}.
	Cmd []string

	// AgentID labels this session in emitted envelopes. Auto-derived
	// from Cmd[0] + random suffix when empty.
	AgentID string

	// Matchers run in order; the first to return ok wins. Empty means
	// {DefaultMatcher}.
	Matchers []PromptMatcher

	// EnvelopeOut receives newline-delimited JSON ApprovalEnvelopes.
	EnvelopeOut io.Writer

	// AnswerIn supplies newline-delimited JSON Answers. Must not be
	// nil if any prompt is expected.
	AnswerIn io.Reader

	// Stdout mirrors the child's stdout so the operator can still see
	// what the agent is doing.
	Stdout io.Writer

	// Stderr mirrors the child's stderr.
	Stderr io.Writer

	// ContextLines is the size of the rolling stdout buffer that
	// becomes ApprovalEnvelope.Context. Default 20.
	ContextLines int

	// Expiry is the TTL applied to each emitted envelope. Default
	// protocol.DefaultExpiry.
	Expiry time.Duration

	// Now and NewID are injection points for deterministic tests.
	Now   func() time.Time
	NewID func() string

	// childDone, when non-nil, is closed by Run once the child process has
	// exited. Process selects on it so a mid-prompt child crash unblocks the
	// answer read instead of parking forever on stdin. Left nil by direct
	// Process callers (tests) that drive childOut/childIn themselves.
	childDone <-chan struct{}

	// pendingMu guards the currently-pending answer slot used by the
	// long-lived answer-reader goroutine (fix-awaitanswer-leaked-decode-goroutine):
	// pendingID is the envelope a call to awaitAnswer is currently waiting on,
	// and pendingCh is the per-prompt channel a decoded Answer is routed to.
	// Both are zero when no prompt is awaiting an answer. One reader goroutine
	// owns answerDec.Decode for the whole Process loop and routes each decoded
	// Answer here by EnvelopeID, so the decoder's internal buffer/scan state is
	// never used concurrently (the data race the old per-call goroutine caused).
	//
	// bufferedAns / bufferedErr hold a decoded Answer (or a terminal decode
	// error such as EOF) that arrived while NO awaitAnswer was pending — e.g. an
	// answer pre-loaded into a finite reader before the first prompt matched,
	// or a late reply to a prompt that already timed out between awaitAnswer
	// calls. The next awaitAnswer consumes them: a matching Answer is used
	// directly, a non-matching Answer is dropped as stale, and a buffered
	// terminal error is surfaced immediately so a drained source does not make
	// the wrapper wait out the full envelope expiry.
	pendingMu    sync.Mutex
	pendingID    string
	pendingCh    chan decodedAnswer
	bufferedAns  *protocol.Answer
	bufferedErr  error

	sessionStarted time.Time
	contextBuf     []string
	mu             sync.Mutex
}

// ErrNoAnswerSource is returned when a prompt is matched but the caller
// did not configure AnswerIn. m1 surfaces this as "wrap was misconfigured";
// the daemon path in m2 never hits it.
var ErrNoAnswerSource = errors.New("wrapper: prompt detected but AnswerIn is nil")

func (w *Wrapper) applyDefaults() {
	if w.Stdout == nil {
		w.Stdout = io.Discard
	}
	if w.Stderr == nil {
		w.Stderr = io.Discard
	}
	if w.EnvelopeOut == nil {
		w.EnvelopeOut = io.Discard
	}
	if w.ContextLines == 0 {
		w.ContextLines = 20
	}
	if w.Expiry == 0 {
		w.Expiry = protocol.DefaultExpiry
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.NewID == nil {
		w.NewID = NewULID
	}
	if len(w.Matchers) == 0 {
		w.Matchers = []PromptMatcher{DefaultMatcher}
	}
	if w.AgentID == "" {
		base := "agent"
		if len(w.Cmd) > 0 && w.Cmd[0] != "" {
			base = w.Cmd[0]
		}
		suf := w.NewID()
		if len(suf) > 6 {
			suf = suf[:6]
		}
		w.AgentID = fmt.Sprintf("%s-%s", base, strings.ToLower(suf))
	}
}

// Run starts Cmd via the platform-appropriate child-process launcher
// (build-tagged spawn_{unix,windows}.go), mirrors its stderr, and drives
// Process against its stdout/stdin until either the child exits or ctx is
// cancelled.
//
// The child's exit is turned into a first-class signal: cmd.Wait runs in a
// goroutine, closes a childDone channel Process selects on, and cancels the
// Process context. That way a mid-prompt child crash unblocks the answer read
// (which would otherwise park on stdin forever) instead of needing kill -9.
func (w *Wrapper) Run(ctx context.Context) error {
	w.applyDefaults()
	if len(w.Cmd) == 0 {
		return errors.New("wrapper: no command to run")
	}
	if spawnChild == nil {
		return errors.New("wrapper: no child-process launcher registered for this platform")
	}
	w.sessionStarted = w.Now()

	// Derive a cancellable context so the child-exit watcher can unblock the
	// IO loop even when the parent ctx is still live.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	child, err := spawnChild(runCtx, w.Cmd[0], w.Cmd[1:]...)
	if err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("wrapper: start %q: %w", w.Cmd[0], err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(w.Stderr, child.Stderr())
	}()

	// Watch for child exit: close childDone + cancel runCtx so a blocked
	// answer read (or the scanner loop) wakes up on child death.
	childDone := make(chan struct{})
	var (
		waitErr  error
		waitOnce sync.Once
	)
	waitDone := make(chan struct{})
	go func() {
		waitErr = child.Wait()
		waitOnce.Do(func() { close(childDone) })
		cancel()
		close(waitDone)
	}()

	w.childDone = childDone
	procErr := w.Process(runCtx, child.Stdout(), child.Stdin())
	_ = child.CloseStdin()

	// Ensure the watcher goroutine has recorded waitErr before we read it.
	<-waitDone
	wg.Wait()

	if procErr != nil && !errors.Is(procErr, context.Canceled) {
		return procErr
	}
	return waitErr
}

// Process is the IO loop split out for testing: it scans childOut line
// by line, fires matchers, emits envelopes, reads answers, and forwards
// each answer's ChoiceKey to childIn. It returns when childOut hits EOF
// or ctx is done.
func (w *Wrapper) Process(ctx context.Context, childOut io.Reader, childIn io.Writer) error {
	w.applyDefaults()
	if w.sessionStarted.IsZero() {
		w.sessionStarted = w.Now()
	}

	scanner := bufio.NewScanner(childOut)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var answerDec *json.Decoder
	if w.AnswerIn != nil {
		answerDec = json.NewDecoder(w.AnswerIn)
		// One long-lived answer-reader goroutine for the whole loop owns
		// answerDec.Decode and routes each decoded Answer to the
		// currently-pending awaitAnswer, dropping stale answers (EnvelopeID
		// != pending) so a late reply to a timed-out prompt can neither race
		// the next prompt's decode on the same *json.Decoder nor trip the
		// EnvelopeID-mismatch crash. This replaces the per-call goroutine that
		// leaked a parked decoder on every ctx/expiry/child-done return.
		// (fix-awaitanswer-leaked-decode-goroutine)
		go w.runAnswerReader(answerDec)
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if _, err := fmt.Fprintln(w.Stdout, line); err != nil {
			return fmt.Errorf("wrapper: mirror stdout: %w", err)
		}
		w.pushContext(line)

		snapshot := w.contextSnapshot()
		env := w.matchAny(line, snapshot)
		if env == nil {
			continue
		}
		w.fillEnvelope(env, snapshot)

		if err := json.NewEncoder(w.EnvelopeOut).Encode(env); err != nil {
			return fmt.Errorf("wrapper: emit envelope %s: %w", env.ID, err)
		}
		if answerDec == nil {
			return ErrNoAnswerSource
		}

		key, err := w.awaitAnswer(ctx, env)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(childIn, key+"\n"); err != nil {
			return fmt.Errorf("wrapper: forward answer to child: %w", err)
		}
	}
	return scanner.Err()
}

// decodedAnswer carries the result of one background answerDec.Decode so the
// blocking read can participate in a select.
type decodedAnswer struct {
	ans protocol.Answer
	err error
}

// awaitAnswer waits for the answer-reader goroutine to route the Answer for env,
// selecting over {routed answer, ctx.Done, envelope-expiry timer, child-exit} so
// the read is cancellable on Ctrl-C, envelope expiry, and child death — it never
// blocks indefinitely on stdin. It registers env as the currently-pending
// envelope (tracked under w.pendingMu) so the single long-lived reader can route
// the matching Answer here; a stale answer (a late reply to an already-timed-out
// prompt) is dropped by the reader and never reaches this select.
//
// There is no per-call decoder goroutine: the reader owns answerDec.Decode for
// the whole Process loop, which closes both the data race on the decoder's
// internal buffer/scan state and the stale-answer EnvelopeID-mismatch crash
// that the old per-call goroutine caused when a prompt timed out and a second
// prompt followed (fix-awaitanswer-leaked-decode-goroutine).
func (w *Wrapper) awaitAnswer(ctx context.Context, env *protocol.ApprovalEnvelope) (string, error) {
	// Register as the current pending so the long-lived answer reader routes
	// the next matching Answer to ch. Cap-1 buffered so a racing late answer
	// (decoded just before we clear the slot on a timeout) never blocks the
	// reader. Cleared on every return path below.
	ch := make(chan decodedAnswer, 1)
	w.pendingMu.Lock()
	// If the reader already decoded something before we registered (an answer
	// pre-loaded into a finite reader, or a late reply that landed between
	// awaitAnswer calls), consume it now: a matching Answer short-circuits, a
	// non-matching Answer is dropped as stale, and a buffered terminal error
	// (EOF) is surfaced immediately instead of waiting out the full expiry.
	var pre decodedAnswer
	havePre := false
	if w.bufferedAns != nil {
		if w.bufferedAns.EnvelopeID == env.ID {
			pre = decodedAnswer{ans: *w.bufferedAns}
			havePre = true
		}
		w.bufferedAns = nil
	}
	if !havePre && w.bufferedErr != nil {
		pre = decodedAnswer{err: w.bufferedErr}
		havePre = true
		w.bufferedErr = nil
	}
	w.pendingID = env.ID
	w.pendingCh = ch
	w.pendingMu.Unlock()
	defer w.clearPending(ch)

	if havePre {
		// A decoded result was already waiting for a waiter; resolve it without
		// entering the select (and without arming the expiry timer).
		return w.resolveAnswer(env, pre)
	}

	// Timer for the envelope's own expiry. A zero/absent ExpiresAt means "no
	// wrapper-side deadline" — leave the timer channel nil so the select ignores
	// it. The deadline is measured against the wrapper's injectable clock (w.Now)
	// so it stays consistent with how fillEnvelope minted ExpiresAt; tests that
	// pin Now to a fixed time therefore never see a spurious immediate expiry.
	var expiryCh <-chan time.Time
	if !env.ExpiresAt.IsZero() {
		d := env.ExpiresAt.Sub(w.Now())
		if d <= 0 {
			// Already expired before we even read: fall back immediately.
			return w.abortKey(env, "envelope already expired")
		}
		t := time.NewTimer(d)
		defer t.Stop()
		expiryCh = t.C
	}

	select {
	case res := <-ch:
		return w.resolveAnswer(env, res)
	case <-ctx.Done():
		return w.abortKey(env, "context cancelled")
	case <-expiryCh:
		return w.abortKey(env, "envelope expired")
	case <-w.childDoneCh():
		return w.abortKey(env, "child process exited")
	}
}

// resolveAnswer validates one decoded Answer against the pending envelope and
// returns the choice key to forward to the child, or an error. A mismatched
// EnvelopeID is treated as a stale answer: the reader already filters by ID
// before routing, so this is a defensive guard against a future regression in
// the reader's routing, surfaced explicitly instead of silently mis-forwarding.
func (w *Wrapper) resolveAnswer(env *protocol.ApprovalEnvelope, res decodedAnswer) (string, error) {
	if res.err != nil {
		return "", fmt.Errorf("wrapper: read answer for %s: %w", env.ID, res.err)
	}
	if res.ans.EnvelopeID != env.ID {
		return "", fmt.Errorf("wrapper: answer envelope_id=%q does not match pending=%q", res.ans.EnvelopeID, env.ID)
	}
	if !choiceExists(env.Choices, res.ans.ChoiceKey) {
		return "", fmt.Errorf("wrapper: choice %q not in envelope %s", res.ans.ChoiceKey, env.ID)
	}
	return res.ans.ChoiceKey, nil
}

// runAnswerReader is the single, long-lived goroutine that decodes Answers from
// answerDec for the whole Process loop. It owns answerDec.Decode — no other
// goroutine calls Decode, so the decoder's internal buffer/scan state is never
// used concurrently (the data race the old per-call goroutine caused when a
// prompt timed out and the next prompt spawned a second decoder on the same
// *json.Decoder). Each decoded Answer is routed to the currently-pending
// awaitAnswer via w.pendingCh; an answer whose EnvelopeID does not match the
// pending envelope (a late reply to a prompt that already timed out) is DROPPED
// as stale so it can neither race the next prompt's decode nor trip the
// EnvelopeID-mismatch crash. An answer (or terminal decode error) that lands
// while NO waiter is pending is buffered for the next awaitAnswer, so a
// finite/pre-loaded answer source still works and a drained source surfaces
// immediately instead of making the wrapper wait out the full expiry.
//
// The goroutine exits when Decode returns an error (EOF when the answer source
// is closed, or a malformed-JSON error): the error is forwarded to a live
// waiter if one exists, otherwise buffered (or dropped if already buffered) and
// the reader stops.
func (w *Wrapper) runAnswerReader(dec *json.Decoder) {
	for {
		var a protocol.Answer
		err := dec.Decode(&a)
		w.pendingMu.Lock()
		ch := w.pendingCh
		id := w.pendingID
		if err != nil {
			// Source exhausted or malformed: forward to a live waiter so it can
			// surface the error; if no one is waiting, park it for the next
			// awaitAnswer (overwriting nothing — a buffered Answer already
			// consumed by then takes precedence via awaitAnswer's check order).
			if ch != nil {
				select {
				case ch <- decodedAnswer{ans: a, err: err}:
				default:
				}
			} else {
				w.bufferedErr = err
			}
			w.pendingMu.Unlock()
			return
		}
		if ch != nil {
			// A waiter is live: route only if the EnvelopeID matches. A stale
			// answer (wrong ID) is dropped here and the loop decodes the next —
			// it is never buffered, because the live waiter will only ever
			// accept the answer it is pending on.
			if a.EnvelopeID == id {
				select {
				case ch <- decodedAnswer{ans: a, err: nil}:
				default:
				}
			}
		} else {
			// No waiter yet (answer pre-loaded, or landed between awaitAnswer
			// calls): park it for the next awaitAnswer to consume by ID.
			w.bufferedAns = &a
		}
		w.pendingMu.Unlock()
	}
}

// clearPending relinquishes the currently-pending slot iff it still points at ch,
// so a later awaitAnswer's registration is never clobbered by a stale return.
func (w *Wrapper) clearPending(ch chan decodedAnswer) {
	w.pendingMu.Lock()
	if w.pendingCh == ch {
		w.pendingCh = nil
		w.pendingID = ""
	}
	w.pendingMu.Unlock()
}

// childDoneCh returns the child-exit channel, or a nil channel (which blocks
// forever in a select) when Run did not wire one — e.g. direct Process callers.
func (w *Wrapper) childDoneCh() <-chan struct{} {
	return w.childDone
}

// abortKey resolves an unanswered prompt to the envelope's default choice so the
// wrapped agent is unblocked with the agent's own safe fallback rather than left
// hanging. If the envelope has no default choice, the prompt is aborted with an
// error naming the reason.
func (w *Wrapper) abortKey(env *protocol.ApprovalEnvelope, reason string) (string, error) {
	for _, c := range env.Choices {
		if c.IsDefault {
			return c.Key, nil
		}
	}
	return "", fmt.Errorf("wrapper: %s and envelope %s has no default choice to fall back on", reason, env.ID)
}

func (w *Wrapper) matchAny(line, snapshot string) *protocol.ApprovalEnvelope {
	for _, m := range w.Matchers {
		if env, ok := m(line, snapshot); ok && env != nil {
			return env
		}
	}
	return nil
}

func (w *Wrapper) fillEnvelope(env *protocol.ApprovalEnvelope, snapshot string) {
	if env.ID == "" {
		env.ID = w.NewID()
	}
	if env.AgentID == "" {
		env.AgentID = w.AgentID
	}
	if env.SessionStarted.IsZero() {
		env.SessionStarted = w.sessionStarted
	}
	if env.Context == "" {
		env.Context = snapshot
	}
	if env.ExpiresAt.IsZero() {
		env.ExpiresAt = w.Now().Add(w.Expiry)
	}
}

func (w *Wrapper) pushContext(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.contextBuf = append(w.contextBuf, line)
	if len(w.contextBuf) > w.ContextLines {
		w.contextBuf = w.contextBuf[len(w.contextBuf)-w.ContextLines:]
	}
}

func (w *Wrapper) contextSnapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.Join(w.contextBuf, "\n")
	if len(s) > protocol.MaxContextBytes {
		s = s[len(s)-protocol.MaxContextBytes:]
	}
	return s
}

func choiceExists(cs []protocol.Choice, key string) bool {
	for _, c := range cs {
		if c.Key == key {
			return true
		}
	}
	return false
}

// defaultPromptRE matches the common bracketed-choice form many CLI
// agents use, e.g. "Do something? [y/N]" or "Apply? [y/n/always]".
// At least one slash is required so that incidental "[ok]" log lines
// don't trigger a false match.
var defaultPromptRE = regexp.MustCompile(`^(.*?)\s*\[([a-zA-Z]+(?:/[a-zA-Z]+)+)\]\s*$`)

// DefaultMatcher handles the bracketed-choice prompt form. Uppercase
// letters mark the default choice; if no letter is uppercase the last
// option becomes the default so timeouts always have a fallback.
func DefaultMatcher(line, _ string) (*protocol.ApprovalEnvelope, bool) {
	m := defaultPromptRE.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	prompt := strings.TrimSpace(m[1])
	if prompt == "" {
		prompt = strings.TrimSpace(line)
	}
	parts := strings.Split(m[2], "/")
	choices := make([]protocol.Choice, 0, len(parts))
	anyDefault := false
	for _, p := range parts {
		isDefault := p != strings.ToLower(p)
		if isDefault {
			anyDefault = true
		}
		choices = append(choices, protocol.Choice{
			Key:       strings.ToLower(p),
			Label:     choiceLabel(p),
			IsDefault: isDefault,
		})
	}
	if !anyDefault && len(choices) > 0 {
		choices[len(choices)-1].IsDefault = true
	}
	return &protocol.ApprovalEnvelope{Prompt: prompt, Choices: choices}, true
}

func choiceLabel(p string) string {
	switch strings.ToLower(p) {
	case "y", "yes":
		return "Approve"
	case "n", "no":
		return "Deny"
	case "a", "always":
		return "Approve and remember"
	case "d":
		return "Deny and remember"
	case "s", "skip":
		return "Skip"
	default:
		if p == "" {
			return p
		}
		return strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
}

// crockfordAlpha is the Crockford base32 alphabet used by NewULID.
const crockfordAlpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidMu guards the monotonic factory state so concurrent NewULID callers
// (multiple wrapped agents, the daemon) never mint two IDs that sort out of
// creation order within the same millisecond.
var (
	ulidMu    sync.Mutex
	ulidLastT uint64   // last 48-bit ms timestamp handed out
	ulidLastE [10]byte // last 80-bit entropy handed out for ulidLastT
)

// NewULID returns a 26-char Crockford-base32 ULID: 48 bits of millisecond
// timestamp followed by 80 bits of entropy. IDs are MONOTONIC — when two
// calls land in the same millisecond the entropy is incremented by one
// rather than redrawn, so lexicographic order always matches creation order.
// The queue relies on this: the bbolt store lists envelopes by ascending ID
// and treats that as chronological / queue position (see daemon.store).
// Sufficient for queue keys and envelope IDs without pulling in a dependency.
func NewULID() string {
	ts := uint64(time.Now().UnixMilli()) & 0xFFFFFFFFFFFF // 48 bits

	ulidMu.Lock()
	if ts > ulidLastT {
		// New (or first) millisecond: draw fresh entropy.
		ulidLastT = ts
		_, _ = rand.Read(ulidLastE[:])
	} else {
		// Same millisecond, or a clock that went backwards: keep the prior
		// (monotonic) timestamp and increment the entropy so the new ID
		// still sorts after the last one. (A backwards clock is pinned to
		// ulidLastT, never allowed to mint a lower-sorting ID.)
		ts = ulidLastT
		incrementEntropy(&ulidLastE)
	}
	var b [16]byte
	b[0] = byte(ts >> 40)
	b[1] = byte(ts >> 32)
	b[2] = byte(ts >> 24)
	b[3] = byte(ts >> 16)
	b[4] = byte(ts >> 8)
	b[5] = byte(ts)
	copy(b[6:], ulidLastE[:])
	ulidMu.Unlock()

	return encodeCrockford(b[:])
}

// incrementEntropy adds one to the 80-bit big-endian entropy in place.
// On the astronomically-unlikely overflow it wraps to zero, which still
// preserves uniqueness within the practical envelope volume of one ms.
func incrementEntropy(e *[10]byte) {
	for i := len(e) - 1; i >= 0; i-- {
		e[i]++
		if e[i] != 0 {
			return
		}
	}
}

func encodeCrockford(src []byte) string {
	// 128 bits → 26 chars × 5 bits = 130 bits. Pad with two leading
	// zero bits via initial nbits=2.
	var out [26]byte
	var (
		bits  uint64
		nbits uint = 2
		j     int
	)
	for i := 0; i < len(src) && j < len(out); i++ {
		bits = (bits << 8) | uint64(src[i])
		nbits += 8
		for nbits >= 5 && j < len(out) {
			shift := nbits - 5
			out[j] = crockfordAlpha[(bits>>shift)&0x1F]
			j++
			nbits -= 5
			bits &= (1 << nbits) - 1
		}
	}
	return string(out[:])
}
