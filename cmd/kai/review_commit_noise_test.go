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
	// The carve-out that keeps an untested claimed fix a finding must be in
	// every prompt that judges one, or the gate would refute what the deep
	// review raised.
	for name, prompt := range map[string]string{"review": rcReviewSystem, "challenge": rcChallengeSystemHead, "fast": rcFastReviewSystem} {
		if !strings.Contains(prompt, "claims to fix a bug and adds nothing that would fail without") && !strings.Contains(prompt, "a claimed fix with nothing that would fail without it is") {
			t.Errorf("%s prompt drops the untested-claimed-fix carve-out", name)
		}
	}
	// The old wording invited a finding for any test that would pass on the
	// old code, whether or not the change claimed it as proof.
	if strings.Contains(rcReviewSystem, "If nothing in the diff would fail on the old code, the fix is unverified and that is a finding") {
		t.Error("the review prompt still makes every untested fix a finding")
	}
}

func TestFastPassChecksContractsVisibleInTheDiff(t *testing.T) {
	// The rule and the four concrete mismatches the benchmark missed: a
	// reword that keeps the sentence but drops the examples fails here.
	for _, want := range []string{"a value whose producer and consumer are both in the diff and disagree",
		"an id vs a name", "a credential id vs a user id", "a Response vs its body", "a stale token after a refresh"} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast-pass prompt is missing %q", want)
		}
	}
}

func TestFastPassChecksReadCheckWriteRaces(t *testing.T) {
	for _, want := range []string{"two concurrent requests would both pass",
		"a counter set to the value read plus one instead of an atomic increment",
		"a one-time code checked and then written back"} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast-pass prompt is missing %q", want)
		}
	}
}
