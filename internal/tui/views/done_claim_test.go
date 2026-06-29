package views

import "testing"

func TestLooksLikeDoneClaim(t *testing.T) {
	yes := []string{
		"✓ Already done — nothing to do",
		"This is already implemented across kai-server and kai-cli.",
		"The three tiers already exist in config.go.",
		"Nothing to do here — the feature is already in place.",
		"No changes needed; the pricing tiers are already wired.",
	}
	for _, r := range yes {
		if !looksLikeDoneClaim(r) {
			t.Errorf("looksLikeDoneClaim(%q) = false, want true", r)
		}
	}
	no := []string{
		"I added the starter tier to config.go and proxy.go.",
		"Here's how the pricing tiers work: free, starter, pro.",
		"I couldn't find a starter tier — it isn't implemented yet.",
		"Done loading the file; here are the results.", // 'done' but not a completion CLAIM about the task
	}
	for _, r := range no {
		if looksLikeDoneClaim(r) {
			t.Errorf("looksLikeDoneClaim(%q) = true, want false", r)
		}
	}
}
