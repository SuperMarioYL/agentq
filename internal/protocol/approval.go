// Package protocol defines the wire format that agentq components and any
// third-party agent can use to surrender a permission decision to an
// out-of-band human reviewer.
package protocol

import "time"

// ApprovalEnvelope is the JSON wire format for a single permission prompt
// surfaced by a wrapped agent. It is produced by `agentq wrap`, stored by
// the daemon, rendered by the web UI, and answered through an Answer.
//
// Field shape is intentionally stable: making this envelope public is the
// product's moat — third-party agent runtimes can emit it directly and
// participate in the same triage queue without going through stdio scraping.
type ApprovalEnvelope struct {
	// ID is a ULID assigned by the wrapper. Monotonic, so it also serves
	// as the queue position and the bbolt key.
	ID string `json:"id"`

	// AgentID is a wrapper-assigned label like "claude-tab3" used to
	// disambiguate which session is waiting.
	AgentID string `json:"agent_id"`

	// SessionStarted is when the wrapped agent process was launched.
	SessionStarted time.Time `json:"session_started"`

	// Prompt is the human-readable question (e.g. "Allow bash command
	// `rm -rf node_modules`?").
	Prompt string `json:"prompt"`

	// Context is the last ~20 lines of agent stdout for situational
	// awareness. Truncated to MaxContextBytes by the wrapper.
	Context string `json:"context"`

	// Choices are the response options offered by the agent. Exactly one
	// must be marked IsDefault=true (matching the agent's own default).
	Choices []Choice `json:"choices"`

	// ExpiresAt is when the wrapper will give up waiting and abort the
	// prompt. The daemon SHOULD NOT accept answers past this time.
	ExpiresAt time.Time `json:"expires_at"`
}

// Choice is one selectable option in an ApprovalEnvelope.
type Choice struct {
	// Key is the short identifier the wrapper will echo back to the agent
	// (e.g. "y", "n", "always").
	Key string `json:"key"`

	// Label is the human-readable button text (e.g. "Approve",
	// "Deny", "Approve and remember").
	Label string `json:"label"`

	// IsDefault marks the choice that fires on Enter or on expiry. The
	// web UI highlights this and the wrapper picks it on timeout.
	IsDefault bool `json:"is_default"`
}

// Answer is what the daemon sends back to the wrapper after the human
// picks a choice. Kept separate from ApprovalEnvelope so the wrapper can
// distinguish "still waiting" from "answer received but invalid".
type Answer struct {
	// EnvelopeID echoes the id of the envelope being answered.
	EnvelopeID string `json:"envelope_id"`

	// ChoiceKey is the Key of the selected Choice.
	ChoiceKey string `json:"choice_key"`

	// AnsweredAt is when the daemon recorded the human's tap.
	AnsweredAt time.Time `json:"answered_at"`
}

// MaxContextBytes caps the Context field on the wrapper side. 4 KiB is
// enough for ~20 lines at 200 cols and keeps the WebSocket frame small
// enough for a phone on patchy LAN.
const MaxContextBytes = 4096

// DefaultExpiry is how long an envelope is valid without an answer. The
// wrapper sets ExpiresAt to time.Now().Add(DefaultExpiry) unless the
// caller overrides it.
const DefaultExpiry = 10 * time.Minute
