package main

import (
	"strings"
	"testing"
)

// The gate and the fast pass apply the same bar as the deep review: generic
// test or consistency advice, or an intended change, is published only with a
// named requirement or regression — while a concrete defect in a test, a doc
// or a string is supported like any other (the 2026-09-27 rerun missed every
// one of those).
func TestGateAndFastPassDoNotPublishTestAndIntentNoise(t *testing.T) {
	for _, want := range []string{"Refute an allegation that is only generic advice", "describes as intended", "concrete regression (what breaks, for whom, on which input)", "a concrete defect wherever it is, test and doc files included", "Small is not the same as wrong"} {
		if !strings.Contains(rcChallengeSystemHead, want) {
			t.Errorf("challenge prompt is missing %q", want)
		}
	}
	for _, want := range []string{"presents as the proof of its fix", "## What is not a defect", "An intended behaviour change"} {
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
	// The fast pass checks the contracts the diff itself shows, with the
	// shared catalog's kinds of mismatch.
	for _, want := range []string{"a producer and consumer that are both in the diff and disagree on what a value is",
		"## 1. Contracts across a call", "Kind of value", "Shape", "Freshness"} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast-pass prompt is missing %q", want)
		}
	}
}

func TestFastPassChecksReadCheckWriteRaces(t *testing.T) {
	for _, want := range []string{"## 2. Shared state and concurrency", "run it twice at the same time",
		"loses one of two withdrawals", "Name the two requests and the interleaving"} {
		if !strings.Contains(rcFastReviewSystem, want) {
			t.Errorf("fast-pass prompt is missing %q", want)
		}
	}
}
