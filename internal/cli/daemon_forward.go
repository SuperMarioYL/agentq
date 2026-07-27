package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// daemonForwarder bridges the wrapper's envelope-out/answer-in streams onto the
// daemon's HTTP contract without changing the wrapper's IO loop: it satisfies
// io.Writer (the wrapper writes newline-delimited ApprovalEnvelope JSON to it)
// and io.Reader (the wrapper reads newline-delimited Answer JSON back from it).
//
// For each envelope written, forwardOne POSTs it to POST /api/envelopes — the
// long-poll submit endpoint — and, when the daemon returns the human's Answer,
// pushes that Answer JSON onto an internal pipe the wrapper reads as its answer
// source. The ApprovalEnvelope + Answer wire formats are unchanged; this is only
// a transport adapter so `wrap --daemon` reuses the exact same Wrapper.
type daemonForwarder struct {
	baseURL string
	token   string
	client  *http.Client

	pr *io.PipeReader
	pw *io.PipeWriter

	// buf accumulates partial envelope bytes until a full line (one JSON object
	// terminated by '\n', as the wrapper's json.Encoder emits) is available.
	mu  sync.Mutex
	buf bytes.Buffer
}

// newDaemonForwarder builds a forwarder targeting baseURL (e.g.
// "http://127.0.0.1:7777") with the given bearer token.
func newDaemonForwarder(baseURL, token string) *daemonForwarder {
	pr, pw := io.Pipe()
	return &daemonForwarder{
		baseURL: baseURL,
		token:   token,
		client:  &http.Client{},
		pr:      pr,
		pw:      pw,
	}
}

// Read supplies Answer JSON back to the wrapper (its AnswerIn).
func (f *daemonForwarder) Read(p []byte) (int, error) { return f.pr.Read(p) }

// Write receives ApprovalEnvelope JSON from the wrapper (its EnvelopeOut). The
// wrapper's json.Encoder writes one object followed by '\n' per prompt, so Write
// buffers until it sees a newline, then forwards each complete envelope.
func (f *daemonForwarder) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.buf.Write(p)
	lines := drainLines(&f.buf)
	f.mu.Unlock()

	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env protocol.ApprovalEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			return len(p), fmt.Errorf("wrap --daemon: decode outgoing envelope: %w", err)
		}
		go f.forwardOne(env)
	}
	return len(p), nil
}

// forwardOne POSTs env to the daemon and, on a successful answer, feeds the
// Answer JSON into the pipe the wrapper reads. On timeout/expiry (HTTP 504) it
// synthesizes the default-choice Answer so the wrapper unblocks with the agent's
// own fallback instead of hanging — the same contract the wrapper honors locally.
//
// Non-504 failures (401/403 auth failure — e.g. reusing `agentq serve` without
// --daemon-token —, connection-refused, 5xx, or a hung-daemon client timeout) are
// surfaced to stderr and the answer pipe is closed with the error so Run exits
// non-zero: approvals must never be silently bypassed by auto-defaulting every
// prompt. (fix-forwarder-swallows-daemon-post-errors)
func (f *daemonForwarder) forwardOne(env protocol.ApprovalEnvelope) {
	ans, err := f.postEnvelope(env)
	if err != nil {
		var psErr *postStatusError
		if errors.As(err, &psErr) && psErr.status == http.StatusGatewayTimeout {
			// 504 = no answer within the envelope TTL: synthesize the default
			// choice so the wrapped agent still unblocks, mirroring the local
			// wrapper's own give-up-and-abort contract.
			ans = defaultAnswer(env)
		} else {
			// Auth failure, transport error, 5xx, or a hung-daemon timeout:
			// surface it so approvals are not silently auto-defaulted, then stop
			// forwarding — the wrapper's answer read sees the closed pipe and Run
			// exits non-zero instead of replaying the default for every prompt.
			fmt.Fprintf(os.Stderr, "wrap --daemon: %v\n", err)
			_ = f.pw.CloseWithError(err)
			return
		}
	}
	data, mErr := json.Marshal(ans)
	if mErr != nil {
		return
	}
	data = append(data, '\n')
	_, _ = f.pw.Write(data)
}

// postStatusError carries the HTTP status of a failed envelope POST so forwardOne
// can distinguish the no-answer 504 (fall back to the default choice) from
// auth/transport failures (401, connection-refused, 5xx) that must be surfaced
// instead of silently auto-defaulting every prompt.
type postStatusError struct {
	status int
	body   string
}

func (e *postStatusError) Error() string {
	return fmt.Sprintf("wrap --daemon: envelope POST status %d: %s", e.status, e.body)
}

// postEnvelope submits one envelope to POST /api/envelopes and returns the
// daemon's Answer. A 504 (no answer within TTL) is surfaced as a postStatusError
// so the caller can classify it (fall back to the default choice). Any other
// non-200 status (401/403/5xx) is surfaced as a postStatusError too; a transport
// error (connection-refused, hung-daemon timeout) is surfaced as the raw error —
// forwardOne surfaces all of these instead of swallowing them.
func (f *daemonForwarder) postEnvelope(env protocol.ApprovalEnvelope) (protocol.Answer, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return protocol.Answer{}, err
	}
	u := f.baseURL + "/api/envelopes"
	if f.token != "" {
		u += "?t=" + url.QueryEscape(f.token)
	}
	// Per-request timeout slightly beyond the envelope's own TTL: the daemon is
	// supposed to return 504 at expiry, so this only fires when the daemon is
	// HUNG (TCP alive but unresponsive) — surfacing the stall instead of
	// blocking forwardOne forever and leaking a goroutine per prompt.
	// (fix-forwarder-swallows-daemon-post-errors)
	deadline := env.ExpiresAt
	if deadline.IsZero() {
		deadline = time.Now().Add(protocol.DefaultExpiry)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline)+2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return protocol.Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return protocol.Answer{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return protocol.Answer{}, &postStatusError{status: resp.StatusCode, body: string(bytes.TrimSpace(raw))}
	}
	var ans protocol.Answer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return protocol.Answer{}, err
	}
	if ans.EnvelopeID == "" {
		ans.EnvelopeID = env.ID
	}
	return ans, nil
}

// Close tears down the answer pipe so the wrapper's answer read sees EOF.
func (f *daemonForwarder) Close() error {
	return f.pw.Close()
}

// drainLines removes and returns every complete '\n'-terminated line currently
// buffered, leaving any trailing partial line in buf.
func drainLines(buf *bytes.Buffer) [][]byte {
	var lines [][]byte
	for {
		data := buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := make([]byte, idx)
		copy(line, data[:idx])
		lines = append(lines, line)
		buf.Next(idx + 1)
	}
	return lines
}

// defaultAnswer builds the Answer that fires the envelope's default choice, used
// when the daemon times out so the wrapped agent still unblocks safely.
func defaultAnswer(env protocol.ApprovalEnvelope) protocol.Answer {
	key := ""
	for _, c := range env.Choices {
		if c.IsDefault {
			key = c.Key
			break
		}
	}
	if key == "" && len(env.Choices) > 0 {
		key = env.Choices[len(env.Choices)-1].Key
	}
	return protocol.Answer{
		EnvelopeID: env.ID,
		ChoiceKey:  key,
		AnsweredAt: time.Now().UTC(),
	}
}
