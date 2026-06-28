package wrapper

import (
	"regexp"
	"strings"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// This file adds a second-runtime PromptMatcher so agentq is not limited to
// Claude Code's bracketed-choice prompts (handled by DefaultMatcher in
// stdio.go). Cursor's agent CLI and Aider surface permission prompts in the
// "(Y)es/(N)o" parenthesized-letter form instead of "[y/n]", e.g.:
//
//	Apply edit to main.py? (Y)es/(N)o/(D)on't ask again [Yes]
//	Run shell command `npm test`? (y)es/(n)o
//
// The ApprovalEnvelope wire format is unchanged — this just teaches the
// wrapper to recognize a second prompt dialect, which is the whole point of
// the protocol: any runtime can join the same triage queue. Select it with
// `agentq wrap --agent cursor` / `--agent aider` (see CursorMatcher wiring in
// the cli package); both map to this matcher.

// cursorPromptRE matches a trailing run of parenthesized-letter options, with
// an optional "[Default]" suffix. At least two options are required so a lone
// "(note)" log line doesn't false-match. Each option is a parenthesized
// letter (or letters) immediately followed by the rest of the word, e.g.
// "(Y)es", "(D)on't ask again", "(y)es".
var cursorPromptRE = regexp.MustCompile(
	`^(.*?)\s*((?:\(([A-Za-z]+)\)[A-Za-z' ]*)(?:/\(([A-Za-z]+)\)[A-Za-z' ]*){1,})\s*(?:\[([A-Za-z]+)\]\s*)?$`)

// cursorOptionRE pulls each "(K)abel" option out of the matched options run.
var cursorOptionRE = regexp.MustCompile(`\(([A-Za-z]+)\)([A-Za-z' ]*)`)

// NormalizeAgent canonicalizes a user-supplied --agent value to one of
// "claude", "cursor", "aider", or "auto" (the fallback for empty/unknown).
// Centralized here so the cli flag wiring and any future programmatic caller
// agree on the accepted vocabulary.
func NormalizeAgent(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude", "claude-code", "claudecode":
		return "claude"
	case "cursor":
		return "cursor"
	case "aider":
		return "aider"
	default:
		return "auto"
	}
}

// CursorMatcher recognizes Cursor / Aider parenthesized-letter permission
// prompts and converts them into a partially-populated ApprovalEnvelope. It
// follows the same contract as DefaultMatcher: the wrapper fills in ID,
// AgentID, SessionStarted, Context, and ExpiresAt afterward.
func CursorMatcher(line, _ string) (*protocol.ApprovalEnvelope, bool) {
	m := cursorPromptRE.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	prompt := strings.TrimSpace(m[1])
	if prompt == "" {
		// No leading question text — not a useful prompt to surface.
		return nil, false
	}
	optsRun := m[2]
	defaultLabel := strings.TrimSpace(m[5]) // the "[Yes]" hint, may be empty

	var choices []protocol.Choice
	anyDefault := false
	for _, om := range cursorOptionRE.FindAllStringSubmatch(optsRun, -1) {
		key := strings.ToLower(om[1])
		word := strings.TrimSpace(om[1] + om[2]) // "Y"+"es" => "Yes"
		isDefault := defaultLabel != "" &&
			strings.EqualFold(word, defaultLabel)
		if isDefault {
			anyDefault = true
		}
		choices = append(choices, protocol.Choice{
			Key:       key,
			Label:     cursorChoiceLabel(key, word),
			IsDefault: isDefault,
		})
	}
	if len(choices) < 2 {
		return nil, false
	}
	if !anyDefault {
		// No explicit [Default] hint matched: fall back to the first option,
		// which for these tools is the affirmative/safe choice on Enter.
		choices[0].IsDefault = true
	}
	return &protocol.ApprovalEnvelope{Prompt: prompt, Choices: choices}, true
}

// cursorChoiceLabel maps a parenthesized-letter option to the same
// human-readable button text DefaultMatcher uses, so the web UI renders
// Cursor/Aider prompts identically to Claude Code ones. Falls back to the
// option's own word when it isn't a recognized verb.
func cursorChoiceLabel(key, word string) string {
	switch key {
	case "y":
		return "Approve"
	case "n":
		return "Deny"
	case "a":
		return "Approve and remember"
	case "d":
		// Aider's "(D)on't ask again" is a deny-and-remember.
		return "Deny and remember"
	case "s":
		return "Skip"
	}
	if word == "" {
		return strings.ToUpper(key)
	}
	return strings.ToUpper(word[:1]) + word[1:]
}
