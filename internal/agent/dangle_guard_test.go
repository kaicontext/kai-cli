package agent

import "testing"

func TestIsChangeDescription_AdjectiveBetweenTheAndNoun(t *testing.T) {
	// Round-14 dogfood (2026-05-13): worker ended with "here are the
	// EXACT edits needed:" and exited clean. The original regex required
	// the change-noun to appear immediately after optional "the" — the
	// adjective "exact" slipped past, dangle guard never fired, and the
	// worker produced zero changes silently. Tests pin the broader form.
	cases := []string{
		"Based on my thorough analysis of the file, here are the exact edits needed:",
		"Here are the proposed changes:",
		"Here is the specific change to make.",
		"Here are the recommended modifications to repl.go.",
		"Here are the necessary fixes.",
		// Also the existing forms (regression guard for the broader regex):
		"Here are the changes needed.",
		"Here are the edits to make.",
	}
	for _, s := range cases {
		if !IsChangeDescription(s) {
			t.Errorf("IsChangeDescription failed to match: %q", s)
		}
	}
}

func TestIsChangeDescription_DoesNotMatchOrdinaryProse(t *testing.T) {
	// False-positive watchlist: phrasings that read like change-talk
	// but shouldn't fire the dangle guard.
	cases := []string{
		"",
		"The tests passed.",
		"I edited the file successfully.",
		"Done. All edits applied.",
	}
	for _, s := range cases {
		if IsChangeDescription(s) {
			t.Errorf("IsChangeDescription false-positive on: %q", s)
		}
	}
}

func TestIsExplicitBlock_SuppressesDangleGuard(t *testing.T) {
	// When the model explicitly signals blocked-on-user-input, the
	// dangle guard suppresses itself — this is the acceptable
	// non-edit terminal state. Confirm the blocked regexes still match
	// alongside the broader change-description ones.
	cases := []string{
		"I'm blocked because the API key is missing.",
		"I am blocked on confirmation of the schema.",
		"Could you clarify whether to use snake_case or camelCase?",
	}
	for _, s := range cases {
		if !IsExplicitBlock(s) {
			t.Errorf("IsExplicitBlock failed to match: %q", s)
		}
	}
}
