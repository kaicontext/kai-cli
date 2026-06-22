package agent

import (
	"strings"
	"testing"
)

func TestPromptHasExploreDirective(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   bool
	}{
		{
			name:   "no EXPLORE token",
			prompt: "edit the file and update the field",
			want:   false,
		},
		{
			name:   "EXPLORE present with --json directive",
			prompt: "EXPLORE: max 3 turns — run kai snapshot list --json to see the exact JSON shape",
			want:   true,
		},
		{
			name:   "EXPLORE present with --help directive",
			prompt: "EXPLORE: check the CLI surface\nrun kai --help first",
			want:   true,
		},
		{
			name:   "EXPLORE present but no verification marker",
			prompt: "EXPLORE: think about it",
			want:   false,
		},
		{
			name:   "EXPLORE with bash directive",
			prompt: "EXPLORE: 1. confirm the format via bash",
			want:   true,
		},
		{
			name:   "word 'explore' in lowercase prose does NOT trigger",
			prompt: "we want to explore the design space; consider running the build",
			want:   false,
		},
		{
			name:   "EXPLORE: with kai command",
			prompt: "EXPLORE: max 2 turns — kai stats --json to discover shape",
			want:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := promptHasExploreDirective(c.prompt); got != c.want {
				t.Errorf("promptHasExploreDirective(%q) = %v, want %v", c.prompt, got, c.want)
			}
		})
	}
}

// TestExploreBeforeEditBlockMessage pins the user-visible blocker
// text. The "BLOCKED" marker + the actionable steps + the dogfood
// citation are load-bearing — they're what the model reads to
// understand WHY the edit was rejected and HOW to recover.
func TestExploreBeforeEditBlockMessage(t *testing.T) {
	out := exploreBeforeEditBlockMessage()
	for _, want := range []string{
		"BLOCKED",
		"EXPLORE",
		"bash",
		"What to do RIGHT NOW",
		"--json",
		"2026-05-26",
		"NOT optional",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block text missing %q; got:\n%s", want, out)
		}
	}
}
