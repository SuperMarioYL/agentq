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
	"os/exec"
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

// Run starts Cmd, mirrors its stderr, and drives Process against its
// stdout/stdin until either the child exits or ctx is cancelled.
func (w *Wrapper) Run(ctx context.Context) error {
	w.applyDefaults()
	if len(w.Cmd) == 0 {
		return errors.New("wrapper: no command to run")
	}
	w.sessionStarted = w.Now()

	cmd := exec.CommandContext(ctx, w.Cmd[0], w.Cmd[1:]...)
	childIn, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("wrapper: stdin pipe: %w", err)
	}
	childOut, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("wrapper: stdout pipe: %w", err)
	}
	childErr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("wrapper: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("wrapper: start %q: %w", w.Cmd[0], err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(w.Stderr, childErr)
	}()

	procErr := w.Process(ctx, childOut, childIn)
	_ = childIn.Close()
	wg.Wait()

	waitErr := cmd.Wait()
	if procErr != nil {
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

		var ans protocol.Answer
		if err := answerDec.Decode(&ans); err != nil {
			return fmt.Errorf("wrapper: read answer for %s: %w", env.ID, err)
		}
		if ans.EnvelopeID != env.ID {
			return fmt.Errorf("wrapper: answer envelope_id=%q does not match pending=%q", ans.EnvelopeID, env.ID)
		}
		if !choiceExists(env.Choices, ans.ChoiceKey) {
			return fmt.Errorf("wrapper: choice %q not in envelope %s", ans.ChoiceKey, env.ID)
		}
		if _, err := io.WriteString(childIn, ans.ChoiceKey+"\n"); err != nil {
			return fmt.Errorf("wrapper: forward answer to child: %w", err)
		}
	}
	return scanner.Err()
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
