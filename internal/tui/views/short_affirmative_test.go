package views

import "testing"

// TestIsShortAffirmative pins the follow-up short-circuit. The
// dispatcher routes these to the chat agent (with prior session
// history) instead of the planner — without this, "yes" / "a" /
// "go explore" run a cold planner that emits "too vague" because
// the response has no context on its own. 2026-05-14 dogfood was
// the canonical failure mode: chat agent found a bug, asked
// "want me to fix it?", user replied "yes", planner ran cold and
// demanded clarification 4 turns in a row.
func TestIsShortAffirmative(t *testing.T) {
	yes := []string{
		// Affirmatives
		"yes", "Yes", "YES", "yes!", "yeah", "yep", "yup", "y",
		"sure", "absolutely", "definitely", "correct", "right", "exactly",
		// Imperatives
		"go", "Go", "go ahead", "go for it", "do it", "do that",
		"proceed", "continue", "keep going", "make it so",
		"go explore", "explore",
		// Choice picks
		"a", "A", "b", "c", "d", "1", "2", "3",
		"first", "second", "third",
		"option a", "option b", "option 1", "option 2",
		"the first", "the second",
		// Continuations
		"more", "more please", "again",
		// Punctuation tolerance
		"yes.", "yes!", "yes?", "a.", "go!",
	}
	for _, s := range yes {
		if !isShortAffirmative(s) {
			t.Errorf("isShortAffirmative(%q) = false, want true", s)
		}
	}

	no := []string{
		"",
		// Real (if short) plan requests must NOT match
		"fix it", "ship it", "rename foo to bar",
		"add a flag",
		// Long affirmatives with trailing detail are out of scope —
		// the planner can handle them
		"yes please do that thing we discussed",
		// Question forms
		"why",
		"what about it",
		// Ambiguous single words that aren't affirmatives
		"maybe", "perhaps", "later", "hmm",
	}
	for _, s := range no {
		if isShortAffirmative(s) {
			t.Errorf("isShortAffirmative(%q) = true, want false", s)
		}
	}
}

// TestIsShortAffirmative_HardLengthCap pins the >20 char cutoff.
// Past that, even an affirmative-sounding phrase probably carries
// enough new context to merit running the planner.
func TestIsShortAffirmative_HardLengthCap(t *testing.T) {
	// 21 chars of pure 'y's — should fall out on the length cap.
	long := "yyyyyyyyyyyyyyyyyyyyy"
	if isShortAffirmative(long) {
		t.Errorf("input longer than the cap should not match: %q", long)
	}
}
