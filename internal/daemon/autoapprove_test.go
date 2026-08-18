package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseAutoApproveRules_FirstColonSplits confirms the spec format
// "<glob>:<choice>" parses with the FIRST colon as the separator and trims
// whitespace, so "  make *  :  y  " yields Pattern="make *", Choice="y".
func TestParseAutoApproveRules_FirstColonSplits(t *testing.T) {
	r, err := ParseAutoApproveRules([]string{"  make *  :  y  "})
	if err != nil {
		t.Fatalf("ParseAutoApproveRules: %v", err)
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("Len=%d want 1", got)
	}
	if r.rules[0].Pattern != "make *" || r.rules[0].Choice != "y" {
		t.Fatalf("rule=%+v want {Pattern:make * Choice:y}", r.rules[0])
	}
}

func TestParseAutoApproveRules_EmptySpecsReturnsEmpty(t *testing.T) {
	r, err := ParseAutoApproveRules(nil)
	if err != nil {
		t.Fatalf("ParseAutoApproveRules(nil): %v", err)
	}
	if !r.Empty() {
		t.Fatalf("want empty rule set")
	}
	c, ok := r.Match("anything")
	if ok || c != "" {
		t.Fatalf("Match on empty set: (%q,%v) want (\"\",false)", c, ok)
	}
}

func TestParseAutoApproveRules_RejectsBadSpec(t *testing.T) {
	bad := []string{
		"",          // empty
		"nochoice",  // missing colon
		":y",        // empty glob
		"make *:",   // empty choice
		"  :  ",     // both empty
	}
	for _, spec := range bad {
		if _, err := ParseAutoApproveRules([]string{spec}); err == nil {
			t.Errorf("spec %q: expected parse error, got nil", spec)
		}
	}
}

// TestAutoApproveRules_MatchFirstWins confirms rules are checked in order and
// the first match wins; non-matching prompts report ok=false (so the daemon
// falls back to the unchanged human-triage path).
func TestAutoApproveRules_MatchFirstWins(t *testing.T) {
	r, err := ParseAutoApproveRules([]string{"make *:y", "git status:s", "rm *:n"})
	if err != nil {
		t.Fatalf("ParseAutoApproveRules: %v", err)
	}
	cases := []struct {
		prompt string
		choice string
		ok     bool
	}{
		{"make test", "y", true},
		{"git status", "s", true},
		{"rm -rf /tmp", "n", true}, // '*' crosses '/' — a path.Match glob would NOT match this
		{"ls -la", "", false},      // no rule matches -> human triage
		{"Make test", "", false},   // case-sensitive: "Make" != "make"
		{"make", "", false},        // "make *" needs the literal "make " prefix + a token
	}
	for _, tc := range cases {
		t.Run(tc.prompt, func(t *testing.T) {
			c, ok := r.Match(tc.prompt)
			if ok != tc.ok {
				t.Fatalf("Match(%q) ok=%v want %v", tc.prompt, ok, tc.ok)
			}
			if c != tc.choice {
				t.Fatalf("Match(%q) choice=%q want %q", tc.prompt, c, tc.choice)
			}
		})
	}
}

// TestGlobMatch_AsteriskMatchesAnyBytes confirms the custom glob's '*' matches
// any run of bytes including '/', so prompts containing file paths are
// matchable (the reason a path.Match glob was NOT used).
func TestGlobMatch_AsteriskMatchesAnyBytes(t *testing.T) {
	yes := []struct{ pat, s string }{
		{"*", ""},               // '*' matches the empty string
		{"*", "anything"},       // '*' matches the whole string
		{"make *", "make test"}, // trailing '*' matches "test"
		{"rm *", "rm -rf /tmp"}, // '*' crosses '/'
		{"*test*", "run make test now"}, // leading+trailing '*' substring match
		{"a*c", "aXc"},          // '*' in the middle
		{"abc", "abc"},          // exact literal
		{"", ""},                // empty pattern matches empty string
	}
	for _, c := range yes {
		if !globMatch(c.pat, c.s) {
			t.Errorf("globMatch(%q,%q)=false want true", c.pat, c.s)
		}
	}
	no := []struct{ pat, s string }{
		{"abc", "abd"},      // mismatched tail
		{"make *", "rm -rf"}, // prefix differs
		{"make", "make "},    // pattern shorter and no '*'
		{"", "x"},            // empty pattern does not match non-empty
		{"make *", "make"},   // pattern requires the literal "make " + token
	}
	for _, c := range no {
		if globMatch(c.pat, c.s) {
			t.Errorf("globMatch(%q,%q)=true want false", c.pat, c.s)
		}
	}
}

// TestAutoApproveRules_NilReceiverMatchesNothing confirms a nil rule set (the
// default when serve is run without --auto-approve) matches nothing so every
// prompt goes to the human-triage path.
func TestAutoApproveRules_NilReceiverMatchesNothing(t *testing.T) {
	var r *AutoApproveRules
	if !r.Empty() {
		t.Fatalf("nil receiver should be Empty")
	}
	c, ok := r.Match("make test")
	if ok || c != "" {
		t.Fatalf("nil.Match returned (%q,%v) want (\"\",false)", c, ok)
	}
}

// TestLoadAutoApproveFile_SkipsBlankAndComments confirms the config file parser
// ignores blank lines and #-prefixed comments and returns the real specs in
// order. An empty path returns nil (no specs).
func TestLoadAutoApproveFile_SkipsBlankAndComments(t *testing.T) {
	t.Setenv("AGENTQ_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")
	content := []byte("# trusted commands\nmake *:y\n\n   \n# a comment with a colon : in it\ngit status:s\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write rules.txt: %v", err)
	}
	specs, err := LoadAutoApproveFile(path)
	if err != nil {
		t.Fatalf("LoadAutoApproveFile: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs %v, want 2", len(specs), specs)
	}
	if specs[0] != "make *:y" || specs[1] != "git status:s" {
		t.Fatalf("specs=%v want [make *:y git status:s]", specs)
	}
	// An empty path returns nil so callers can pass an unset flag through.
	if specs, err := LoadAutoApproveFile(""); err != nil || specs != nil {
		t.Fatalf("LoadAutoApproveFile(\"\") = (%v,%v) want (nil,nil)", specs, err)
	}
}

func TestLoadAutoApproveFile_MissingFileErrors(t *testing.T) {
	if _, err := LoadAutoApproveFile("/nonexistent/autoapprove.txt"); err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
}
