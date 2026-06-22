package orchestrator

import "testing"

// TestShouldVerify_CodingMode covers the 2026-05-25 extension of
// shouldVerify to ModeCoding. Coding agents with edits + a bash
// command now get a verify pass; pre-extension they didn't. The
// kai-desktop dogfood pinned the gap: a coding agent applied a
// "fix" that left the build failing, no verify ran, and the
// orchestrator auto-promoted before anything could catch it.
func TestShouldVerify_CodingMode(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		editsApplied bool
		firstBash    string
		want         bool
	}{
		{"coding + edits + bash → verify", "Coding", true, "npm run dev", true},
		{"coding + edits + no bash → skip", "Coding", true, "", false},
		{"coding + no edits → skip", "Coding", false, "npm run dev", false},
		{"debug + edits + bash → verify (legacy)", "Debug", true, "go test ./...", true},
		{"planning → skip", "Planning", true, "go test", false},
		{"review → skip", "Review", true, "go test", false},
		// Conversation merged into coding (2026-05-29): a conversational
		// turn that actually applied edits + ran a build is real work and
		// gets the same verify pass as any coding run.
		{"conversation → verify (merged into coding)", "Conversation", true, "go test", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldVerify(c.mode, c.editsApplied, c.firstBash); got != c.want {
				t.Errorf("shouldVerify(%q, %v, %q) = %v, want %v",
					c.mode, c.editsApplied, c.firstBash, got, c.want)
			}
		})
	}
}
