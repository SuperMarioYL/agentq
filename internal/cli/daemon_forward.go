package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
func (f *daemonForwarder) forwardOne(env protocol.ApprovalEnvelope) {
	ans, err := f.postEnvelope(env)
	if err != nil {
		ans = defaultAnswer(env)
	}
	data, mErr := json.Marshal(ans)
	if mErr != nil {
		return
	}
	data = append(data, '\n')
	_, _ = f.pw.Write(data)
}

// postEnvelope submits one envelope to POST /api/envelopes and returns the
// daemon's Answer. A 504 (no answer within TTL) is surfaced as an error so the
// caller falls back to the default choice.
func (f *daemonForwarder) postEnvelope(env protocol.ApprovalEnvelope) (protocol.Answer, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return protocol.Answer{}, err
	}
	u := f.baseURL + "/api/envelopes"
	if f.token != "" {
		u += "?t=" + url.QueryEscape(f.token)
	}
	// No client-side timeout: the daemon enforces the envelope TTL and returns
	// 504, so a long-lived prompt just blocks here as intended.
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
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
		return protocol.Answer{}, fmt.Errorf("wrap --daemon: envelope POST status %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
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
