package agent

import "testing"

func TestClaimsBuildSuccess(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		// Round-21 dogfood verbatim: this is the exact phrase that
		// motivated the guard. If this stops matching, the regression
		// is on the regex, not the bug.
		{
			"round-21 verbatim",
			"The build/vet commands succeeded with no errors (clean output). Let me record the checkpoints for the two edits made:",
			true,
		},
		{"build succeeded", "Confirmed: the build succeeded.", true},
		{"build passed", "build passed", true},
		{"tests pass", "all tests pass now", true},
		{"tests passed", "Tests passed cleanly.", true},
		{"compiles cleanly", "the code compiles cleanly", true},
		{"no compile errors", "There are no compile errors.", true},
		{"no build errors", "no build errors remain", true},
		{"everything compiles", "Everything compiles.", true},
		{"vet passed", "go vet passed without issues", true},
		{"trailing clean output", "Ran the suite — clean output", true},

		// Negatives — should NOT trigger the guard.
		{"empty", "", false},
		{"didn't succeed", "the build didn't succeed", false},
		{"failed", "build failed with 3 errors", false},
		{"benign prose", "the build script should verify this", false},
		{"forward-looking", "I'll now check that the tests pass.", true}, // intentional false-positive: matches our success keyword. Acceptable since this is the final-text only and forward-looking phrasing is rare in closing summaries; better to nudge than miss.
		{"hint mention", "you can run go test to check", false},
		// Question form "should the build pass?" matches the regex
		// because it contains "build pass". Acceptable false positive:
		// the guard only consults the agent's FINAL turn text, and
		// closing summaries are declarative — agents don't end a turn
		// asking the user whether the build should pass. Recording the
		// match keeps the regex simpler than parsing for interrogative
		// vs. declarative mood.
		{"asking about build (acceptable FP)", "should the build pass?", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaimsBuildSuccess(tc.text)
			if got != tc.want {
				t.Errorf("ClaimsBuildSuccess(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
