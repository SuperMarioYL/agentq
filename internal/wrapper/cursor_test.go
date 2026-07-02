package wrapper

import "testing"

func TestCursorMatcher_AiderApplyEdit(t *testing.T) {
	env, ok := CursorMatcher("Apply edit to main.py? (Y)es/(N)o/(D)on't ask again [Yes]", "")
	if !ok || env == nil {
		t.Fatal("expected match for aider apply-edit prompt")
	}
	if env.Prompt != "Apply edit to main.py?" {
		t.Fatalf("prompt=%q", env.Prompt)
	}
	if len(env.Choices) != 3 {
		t.Fatalf("choices=%d want 3 (%+v)", len(env.Choices), env.Choices)
	}
	gotKeys := []string{env.Choices[0].Key, env.Choices[1].Key, env.Choices[2].Key}
	want := []string{"y", "n", "d"}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("choice[%d].Key=%q want %q", i, gotKeys[i], want[i])
		}
	}
	// "[Yes]" marks (Y)es as the default.
	if !env.Choices[0].IsDefault {
		t.Fatalf("expected (Y)es to be default from [Yes] hint")
	}
	if env.Choices[1].IsDefault || env.Choices[2].IsDefault {
		t.Fatalf("only (Y)es should be default: %+v", env.Choices)
	}
	if env.Choices[0].Label != "Approve" || env.Choices[1].Label != "Deny" || env.Choices[2].Label != "Deny and remember" {
		t.Fatalf("labels=%q/%q/%q", env.Choices[0].Label, env.Choices[1].Label, env.Choices[2].Label)
	}
}

func TestCursorMatcher_CursorRunCommand(t *testing.T) {
	env, ok := CursorMatcher("Run shell command `npm test`? (y)es/(n)o", "")
	if !ok || env == nil {
		t.Fatal("expected match for cursor run-command prompt")
	}
	if len(env.Choices) != 2 {
		t.Fatalf("choices=%d want 2", len(env.Choices))
	}
	// No [Default] hint → first option becomes default.
	if !env.Choices[0].IsDefault {
		t.Fatalf("expected first option default when no [hint]: %+v", env.Choices)
	}
}

func TestCursorMatcher_IgnoresClaudeBracketForm(t *testing.T) {
	// The bracketed "[y/n]" dialect belongs to DefaultMatcher; CursorMatcher
	// must not steal it.
	if _, ok := CursorMatcher("Do something? [y/N]", ""); ok {
		t.Fatal("CursorMatcher should not match bracketed claude prompts")
	}
}

func TestCursorMatcher_IgnoresNonPrompts(t *testing.T) {
	for _, line := range []string{
		"just a log line",
		"writing file (done)",      // single parenthesized token, no question
		"(Y)es/(N)o",               // options but no question text
		"Building project... 100%", // no options
	} {
		if _, ok := CursorMatcher(line, ""); ok {
			t.Errorf("unexpected match for %q", line)
		}
	}
}

// TestCursorMatcher_DisambiguatesCollidingKeys guards fix-cursor-choice-key-collision:
// when two options share a first letter (e.g. "(A)ll/(A)bort"), the matcher must NOT
// mint two choices with the same key — otherwise answer resolution matches the first
// and silently fires the wrong option. Every emitted key must be unique.
func TestCursorMatcher_DisambiguatesCollidingKeys(t *testing.T) {
	env, ok := CursorMatcher("Apply all pending edits? (A)ll/(A)bort/(S)kip", "")
	if !ok || env == nil {
		t.Fatal("expected match for colliding-key prompt")
	}
	if len(env.Choices) != 3 {
		t.Fatalf("got %d choices, want 3: %+v", len(env.Choices), env.Choices)
	}
	seen := map[string]bool{}
	for _, c := range env.Choices {
		if c.Key == "" {
			t.Errorf("empty choice key in %+v", env.Choices)
		}
		if seen[c.Key] {
			t.Fatalf("duplicate choice key %q in %+v", c.Key, env.Choices)
		}
		seen[c.Key] = true
	}
	// The first "(A)ll" keeps the natural letter key; "(A)bort" falls back to its word.
	if env.Choices[0].Key != "a" {
		t.Errorf("first choice key=%q want a", env.Choices[0].Key)
	}
	if env.Choices[1].Key == "a" {
		t.Errorf("second colliding choice must not reuse key %q", env.Choices[1].Key)
	}
}

// TestCursorMatcher_TripleCollisionStillUnique exercises the positional-suffix
// fallback: three options with the same first letter AND same word must all get
// distinct keys.
func TestCursorMatcher_TripleCollisionStillUnique(t *testing.T) {
	env, ok := CursorMatcher("Pick? (A)pply/(A)pply/(A)pply", "")
	if !ok || env == nil {
		t.Fatal("expected match")
	}
	seen := map[string]bool{}
	for _, c := range env.Choices {
		if seen[c.Key] {
			t.Fatalf("duplicate key %q among %+v", c.Key, env.Choices)
		}
		seen[c.Key] = true
	}
	if len(seen) != len(env.Choices) {
		t.Errorf("keys not all unique: %+v", env.Choices)
	}
}

func TestNormalizeAgent(t *testing.T) {
	cases := map[string]string{
		"claude":      "claude",
		"Claude-Code": "claude",
		"cursor":      "cursor",
		"AIDER":       "aider",
		"":            "auto",
		"nonsense":    "auto",
	}
	for in, want := range cases {
		if got := NormalizeAgent(in); got != want {
			t.Errorf("NormalizeAgent(%q)=%q want %q", in, got, want)
		}
	}
}
