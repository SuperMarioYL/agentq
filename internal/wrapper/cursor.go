package wrapper

import (
	"regexp"
	"strconv"
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
	seen := map[string]int{} // key -> index of the first choice that claimed it
	for _, om := range cursorOptionRE.FindAllStringSubmatch(optsRun, -1) {
		word := strings.TrimSpace(om[1] + om[2]) // "Y"+"es" => "Yes"
		key := disambiguateKey(strings.ToLower(om[1]), word, seen)
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

// disambiguateKey returns a choice key that is unique within one prompt. The
// natural key is the lowercase parenthesized letter-run (e.g. "y" from "(Y)es"),
// but Cursor/Aider prompts routinely offer two options that share a first letter
// — "(A)ll/(A)bort", "(Y)es/(Y)ield" — which would mint two protocol.Choice
// entries with the same Key. Answer resolution (server.choiceKnown /
// wrapper.choiceExists) matches the FIRST choice with a given key, so a collision
// silently fires the wrong option. To keep every choice answerable, on collision
// we fall back to the option's normalized full word ("all"/"abort"); if THAT also
// collides (or is empty), we append a positional suffix ("a-2"). seen tracks the
// keys already handed out for this prompt.
func disambiguateKey(letterKey, word string, seen map[string]int) string {
	pick := func(k string) (string, bool) {
		if k == "" {
			return "", false
		}
		if _, taken := seen[k]; taken {
			return "", false
		}
		seen[k] = len(seen)
		return k, true
	}
	if k, ok := pick(letterKey); ok {
		return k
	}
	// First letter already used: try the full normalized word.
	if k, ok := pick(strings.ToLower(word)); ok {
		return k
	}
	// Word collided too (or was empty): append a positional suffix until unique.
	base := letterKey
	if base == "" {
		base = strings.ToLower(word)
	}
	if base == "" {
		base = "opt"
	}
	for i := 2; ; i++ {
		cand := base + "-" + strconv.Itoa(i)
		if _, taken := seen[cand]; !taken {
			seen[cand] = len(seen)
			return cand
		}
	}
}

// cursorChoiceLabel maps a parenthesized-letter option to the same
// human-readable button text DefaultMatcher uses, so the web UI renders
// Cursor/Aider prompts identically to Claude Code ones.
//
// The mapping keys off the full option WORD ("Yes", "All", "Skip"), NOT the bare
// parenthesized letter. Keying off the letter is wrong: two different actions can
// share a first letter — Aider's "(A)ll" (apply to ALL remaining hunks) and an
// "(A)lways" (approve-and-remember) both start with 'a', but only the latter is a
// remember-my-choice action. Labeling "(A)ll" as "Approve and remember" would show
// a materially misleading button on the phone triage surface. DefaultMatcher's
// choiceLabel already labels off the full token (so "[y/n/all]" renders "All");
// this keeps the two matchers consistent. Falls back to the Title-cased word for
// anything not a recognized verb.
func cursorChoiceLabel(key, word string) string {
	lw := strings.ToLower(strings.TrimSpace(word))
	switch lw {
	case "y", "yes":
		return "Approve"
	case "n", "no":
		return "Deny"
	case "a", "always":
		return "Approve and remember"
	case "all":
		return "All"
	case "s", "skip":
		return "Skip"
	}
	// Aider's "(D)on't ask again" is a deny-and-remember. Recognize it by the word
	// prefix, not the bare "d" key, so an unrelated d-word (e.g. "(D)iff") is not
	// silently relabeled as a remember-my-choice action.
	if strings.HasPrefix(lw, "don't") || strings.HasPrefix(lw, "dont") {
		return "Deny and remember"
	}
	if word == "" {
		return strings.ToUpper(key)
	}
	return strings.ToUpper(word[:1]) + word[1:]
}
