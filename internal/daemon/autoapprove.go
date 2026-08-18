package daemon

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// AutoApproveRule is one compiled pattern->choice auto-approve rule.
//
// Pattern is a minimal shell-style glob matched against an incoming
// ApprovalEnvelope's Prompt: a single metacharacter '*' matches any (possibly
// empty) sequence of bytes, INCLUDING '/' (free-text prompts are not paths,
// so a path.Match glob that refuses to cross '/' would silently fail to match
// prompts containing file paths — e.g. "rm -rf *" would not match "rm -rf
// /tmp"). Every other byte is a literal, so there are no malformed patterns.
// Choice is the Choice.Key the daemon auto-applies when Pattern matches,
// WITHOUT blocking on the phone triage queue.
//
// Matching is whole-string: a rule "make *" matches the prompt "make test"
// but not "rm -rf /tmp". Use leading/trailing "*" to match substrings.
type AutoApproveRule struct {
	Pattern string
	Choice  string
}

// AutoApproveRules is a compiled, ordered set of auto-approve rules. The FIRST
// rule whose Pattern matches a prompt wins. A nil or empty rule set matches
// nothing, so the daemon falls back to the unchanged human-triage path for
// every prompt — auto-approve is purely opt-in via `agentq serve --auto-approve`.
//
// The rule set is read-only after construction; postEnvelope consults it
// without mutating it, so no lock is needed.
type AutoApproveRules struct {
	rules []AutoApproveRule
}

// ParseAutoApproveRules compiles one or more "<glob>:<choice>" specs into an
// AutoApproveRules. The FIRST colon in a spec is the separator: glob before
// it, choice after it. An empty specs slice returns a non-nil empty rule set
// (Match reports ok=false for every prompt). Specs missing the colon or either
// half are rejected at parse time so a bad --auto-approve flag fails the daemon
// at startup, not silently at match time.
func ParseAutoApproveRules(specs []string) (*AutoApproveRules, error) {
	r := &AutoApproveRules{}
	for _, spec := range specs {
		rule, err := parseAutoApproveSpec(spec)
		if err != nil {
			return nil, err
		}
		r.rules = append(r.rules, rule)
	}
	return r, nil
}

func parseAutoApproveSpec(spec string) (AutoApproveRule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return AutoApproveRule{}, fmt.Errorf("daemon: empty auto-approve rule")
	}
	// Split on the FIRST colon: "<glob>:<choice>". A choice key is a short
	// token (y / n / always) with no colon, so the first colon delimits the
	// glob from the choice reliably.
	glob, choice, ok := strings.Cut(spec, ":")
	if !ok {
		return AutoApproveRule{}, fmt.Errorf("daemon: invalid auto-approve rule %q (want \"<glob>:<choice>\")", spec)
	}
	glob = strings.TrimSpace(glob)
	choice = strings.TrimSpace(choice)
	if glob == "" || choice == "" {
		return AutoApproveRule{}, fmt.Errorf("daemon: invalid auto-approve rule %q (want \"<glob>:<choice>\")", spec)
	}
	return AutoApproveRule{Pattern: glob, Choice: choice}, nil
}

// Match returns the auto-applied choice key for the first rule whose Pattern
// matches prompt, and ok=false when no rule matches (or the rule set is
// empty). The caller is responsible for checking that the returned choice is
// among the envelope's choices before auto-approving; a rule whose Choice is
// not a valid option for a given envelope must fall through to human triage
// rather than silently answering with an invalid key.
func (r *AutoApproveRules) Match(prompt string) (choice string, ok bool) {
	if r == nil {
		return "", false
	}
	for _, rule := range r.rules {
		if globMatch(rule.Pattern, prompt) {
			return rule.Choice, true
		}
	}
	return "", false
}

// Empty reports whether the rule set has no rules. A nil receiver is empty.
func (r *AutoApproveRules) Empty() bool {
	if r == nil {
		return true
	}
	return len(r.rules) == 0
}

// Len reports the number of compiled rules. A nil receiver has length 0.
func (r *AutoApproveRules) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rules)
}

// globMatch reports whether pattern matches s. It is a minimal shell-style
// glob: a single metacharacter '*' matches any (possibly empty) sequence of
// bytes, including '/' (free-text prompts are not paths, so a path.Match glob
// that refuses to cross '/' would silently fail to match prompts containing
// file paths). Every other byte is a literal. The classic two-pointer
// algorithm with '*' backtracking, O(len(s)) on a non-backtracking match.
func globMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			mark = si
			pi++
		case pi < len(pattern) && pattern[pi] == s[si]:
			pi++
			si++
		case star != -1:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	// A trailing run of '*' in the pattern still matches the (empty) tail of s.
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// LoadAutoApproveFile reads auto-approve specs (one "<glob>:<choice>" per line)
// from path. Blank lines and lines whose first non-space character is "#" are
// ignored, so a config file can carry comments. A path of "" returns nil
// (no specs) so callers can pass an unset --auto-approve-file through blindly.
func LoadAutoApproveFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("daemon: open auto-approve-file %q: %w", path, err)
	}
	defer f.Close()
	var specs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		specs = append(specs, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("daemon: read auto-approve-file %q: %w", path, err)
	}
	return specs, nil
}
