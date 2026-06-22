package views

import "testing"

// TestIsGreeting covers the dispatcher short-circuit that routes
// chitchat past the planner. Two failure modes to avoid:
//   - False positives (a real code-change request misclassified
//     as a greeting → no planner runs → user's work doesn't get
//     done).
//   - False negatives (a clear greeting reaches the planner → 1k
//     fresh tokens to produce "no work to plan").
// The test table biases toward conservative classification:
// borderline cases fall through to the planner.
func TestIsGreeting(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Standalone — true.
		{"hi", true},
		{"hello", true},
		{"hey", true},
		{"hi!", true},
		{"hello.", true},
		{"thanks", true},
		{"thank you", true},
		{"ok", true},
		{"good morning", true},
		{"bye", true},

		// Prefix-anchored — true.
		{"hi how are you", true},
		{"hello there", true},
		{"hey kai", true},
		{"thanks for the help", true},
		{"good morning, anything new?", true},

		// Greeting + concrete verb — false (route to planner so
		// the work gets planned, not just chatted at).
		{"hi can you add rate limiting to the api", false},
		{"hello please fix the homepage crash", false},
		{"hey rename FooBar to BazQux", false},

		// Real code-change requests — false.
		{"add rate limiting", false},
		{"fix the homepage", false},
		{"refactor the parser", false},

		// Empty / whitespace — false.
		{"", false},
		{"   ", false},

		// Long inputs — false even if they start with a greeting
		// (probably an embedded greeting in a longer description).
		{"hi I'd like you to refactor the config parser to support nested env-var substitution across multiple files including all of the existing tests", false},

		// Borderline: "ok" alone is greeting; "ok do that" is a
		// request that incidentally starts with "ok". "ok" isn't a
		// prefix in the list, so the standalone check is the only
		// way "ok"-prefixed inputs match — meaning "ok do that"
		// correctly falls through to the planner.
		{"ok do that", false},
		{"hi do that thing", false}, // "hi " prefix + "do " verb = false
	}
	for _, c := range cases {
		if got := isGreeting(c.in); got != c.want {
			t.Errorf("isGreeting(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
