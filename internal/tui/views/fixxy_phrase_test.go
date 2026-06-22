package views

import "testing"

// TestIsFixxyFeedbackPhrase pins the mode-2 trigger phrase.
// The matcher is intentionally fuzzy on punctuation/casing
// (the user is venting, not parsing) but strict on word
// presence + order so unrelated "no" / "i don't like X" lines
// don't accidentally fire.
func TestIsFixxyFeedbackPhrase(t *testing.T) {
	cases := map[string]bool{
		// Match — canonical and stylistic variants.
		"no sir i don't like it":         true,
		"NO SIR I DONT LIKE IT":          true,
		"no, sir — i don't like it.":     true,
		"no sir, i really don't like it": true,
		"NO SIR I DO NOT LIKE IT AT ALL": true,

		// No match — missing one of the three keywords.
		"no, that's wrong":          false,
		"i don't like the answer":   false,
		"sir, you've got it wrong":  false,
		"":                          false,

		// No match — keywords present but wrong order.
		// "like" before "sir" → not the trigger phrase.
		"i don't like it sir, no": false,

		// No match — word-substring false positives. "tankle"
		// contains "no", "Tarsier" contains "sir", etc.
		// Our matcher uses substring (not word boundary)
		// for fuzziness, so this CAN false-positive on
		// pathological input. Document the trade with this
		// test: we accept the rare false-positive in exchange
		// for not having to tokenize.
		"snowy day": false, // "no" substring; missing sir+like
	}
	for in, want := range cases {
		if got := isFixxyFeedbackPhrase(in); got != want {
			t.Errorf("isFixxyFeedbackPhrase(%q) = %v, want %v", in, got, want)
		}
	}
}
