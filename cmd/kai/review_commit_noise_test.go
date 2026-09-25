package main

import (
	"strings"
	"testing"
)

// The gate and the fast pass apply the same bar as the deep review: a missing
// test, a consistency point or an intended change is published only with a
// named requirement or regression.
func TestGateAndFastPassDoNotPublishTestAndIntentNoise(t *testing.T) {
	for _, want := range []string{"REFUTE an allegation that is only a missing or weak test", "describes as intended", "concrete regression (what breaks, for whom, on which input)"} {
		if !strings.Contains(rcChallengeSystemHead, want) {
			t.Errorf("challenge prompt is missing %q", want)
		}
	}
	for _, want := range []string{"presents as the proof of its fix", "A path without a test, a style or consistency point, or a change the author says is intended is not an issue"} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast-pass prompt is missing %q", want)
		}
	}
	// The old wording invited a finding for any test that would pass on the
	// old code, whether or not the change claimed it as proof.
	if strings.Contains(rcReviewSystem, "If nothing in the diff would fail on the old code, the fix is unverified and that is a finding") {
		t.Error("the review prompt still makes every untested fix a finding")
	}
}
