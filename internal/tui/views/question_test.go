package views

import "testing"

// TestIsQuestion covers the chat-vs-planner classifier. Two failure
// modes:
//   - False positive: a real code-change request gets routed to
//     chat. Chat is read-only — the user's fix never lands and they
//     see a prose explanation instead of a plan card.
//   - False negative: a question ("how does auth work?") reaches
//     the planner, which produces ErrTooVague and falls back to
//     chat with a confusing "request too vague" surface.
//
// The 2026-05-12 dogfood pinned the false-positive case: "I want
// the `?` key to toggle..." was misrouted to chat because the
// literal backticked ? read as an interrogative mark. The fix
// strips inline-code spans before classification.
func TestIsQuestion(t *testing.T) {
	cases := map[string]bool{
		// True questions — should route to chat.
		"what does this project do?":            true,
		"how does auth work?":                   true,
		"why is the build failing?":             true,
		"explain how the cache works":           true,
		"describe the planner flow":             true,
		"tell me what kai_grep does":            true,

		// Code-change requests — must NOT be classified as questions.
		"add a /health endpoint":                false,
		"fix the route conflict in routes.go":   false,
		"rename filterExistingPaths to keep":    false,
		"make the planner emit structured json": false,

		// The 2026-05-12 dogfood case: backticked `?` is a keyboard
		// key, not a question mark. Must classify as a code-change
		// request so it reaches the planner.
		"I want the `?` key to toggle back instead of being idempotent": false,
		"the `?` shortcut should collapse, not be idempotent":           false,
		// Other inline-code spans that previously confused the
		// classifier: ternary operators, regex chars, etc.
		"refactor the `a ? b : c` ternary to a switch": false,
		"the `++` post-increment is wrong":             false,

		// Mixed: question-word prefix WITH an action verb — the
		// action verb wins (user wants work, not an explanation).
		"how do I add a /health endpoint":     false,
		"what would change if we rename foo": false,

		// Empty / whitespace.
		"":   false,
		"  ": false,
	}
	for in, want := range cases {
		got := isQuestion(in)
		if got != want {
			t.Errorf("isQuestion(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestStripInlineCode pins the helper directly so a future change
// can't silently regress its handling of unmatched backticks or
// triple-fence blocks.
func TestStripInlineCode(t *testing.T) {
	cases := map[string]string{
		"plain prose":                     "plain prose",
		"the `?` key":                     "the  key",
		"`a` and `b` and `c`":             " and  and ",
		// Triple backticks toggle three times (ON/OFF/ON) so the
		// inner " block " AND the trailing fence's middle backtick
		// are both inside a code span and dropped. Acceptable: the
		// classifier doesn't care what's inside a fence anyway.
		"triple ``` block ``` stripped": "triple  stripped",
		"unmatched ` backtick eats rest":  "unmatched ", // unclosed backtick drops trailing
		"":                                "",
	}
	for in, want := range cases {
		got := stripInlineCode(in)
		if got != want {
			t.Errorf("stripInlineCode(%q) = %q, want %q", in, got, want)
		}
	}
}
