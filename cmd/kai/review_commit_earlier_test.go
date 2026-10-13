package main

import (
	"strings"
	"testing"
)

// kai-server#437's shape: the reviewer carries an earlier point to a new line
// and tags it; the tag is off while the bullets are worked on by wording, and
// back on the final ISSUES bullet at that location — nowhere else.
func TestEarlierTagsLeaveAndReturn(t *testing.T) {
	draft := "The guard is still missing (earlier: 0123456789ab).\n" +
		"===REVIEW-DATA===\nMERGE_READY: 3\nISSUES:\n" +
		"- internal/smoketest/excerpt.go:13 — when `limit <= 0`, `r[:limit]` panics (earlier: 0123456789ab)\n" +
		"- internal/smoketest/excerpt.go:20 — `ctx.Done` is never checked\n" +
		"DECISIONS:\n- none\n"
	stripped, refs := rcTakeEarlierTags(draft)
	if strings.Contains(stripped, "(earlier:") {
		t.Fatalf("a tag survived into the draft the sweep and challenge read:\n%s", stripped)
	}
	if refs["internal/smoketest/excerpt.go:13"] != "(earlier: 0123456789ab)" || len(refs) != 1 {
		t.Fatalf("refs = %v", refs)
	}

	// The challenge rewrote the bullet's words and folded an "(also: …)" on.
	final := "## Findings\n\n### internal/smoketest/excerpt.go:13 — panics on a non-positive limit\n" +
		"===REVIEW-DATA===\nISSUES:\n" +
		"- internal/smoketest/excerpt.go:13 — `Excerpt` panics on a non-positive `limit` (also: internal/smoketest/excerpt.go:15)\n" +
		"- internal/smoketest/excerpt.go:20 — `ctx.Done` is never checked\n" +
		"DECISIONS:\n- internal/smoketest/excerpt.go:13 — not an issue line\n"
	got := rcPutEarlierTags(final, refs)
	_, risks, _, _, _, _ := rcParseReviewOutput(got)
	if len(risks) != 2 || !strings.HasSuffix(risks[0], "(also: internal/smoketest/excerpt.go:15) (earlier: 0123456789ab)") || strings.Contains(risks[1], "(earlier:") {
		t.Errorf("risks = %q", risks)
	}
	if strings.Count(got, "(earlier:") != 1 {
		t.Errorf("the tag landed outside the ISSUES bullet:\n%s", got)
	}
	if rcPutEarlierTags(final, nil) != final {
		t.Error("no refs changed the review")
	}
}

func TestPriorBlockShowsRefsAndThePromptAsksForThem(t *testing.T) {
	p := &rcPrior{Head: "19db9fca4cfd", Points: []rcPriorPoint{{Kind: "defect", Text: "a.go:3 — `x` is nil", Ref: "0123456789ab"}, {Kind: "nit", Text: "a.go:1 — typo"}}}
	b := rcPriorBlock(p, nil, true)
	if !strings.Contains(b, "- [defect, ref 0123456789ab] a.go:3 — `x` is nil") || !strings.Contains(b, "- [nit] a.go:1 — typo") {
		t.Errorf("block:\n%s", b)
	}
	if !strings.Contains(rcReviewSystem, `"(earlier: <ref>)"`) {
		t.Error("the reviewer prompt does not ask for the tag")
	}
}

// kai-cli#171: a decision that still stands carries its ref through the
// challenge, keyed by its wording since it has no location.
func TestEarlierTagsOnDecisions(t *testing.T) {
	draft := rcReviewDataMarker + "\nMERGE_READY: 4\nISSUES:\n(none)\nDECISIONS:\n" +
		"- The fact-check now supports a defect whose consequence is unverified; every review publishes more findings. (earlier: 0123456789ab)\n" +
		"- A brand-new decision.\n"
	stripped, refs := rcTakeEarlierTags(draft)
	if strings.Contains(stripped, "(earlier:") || len(refs) != 1 {
		t.Fatalf("stripped:\n%s\nrefs=%v", stripped, refs)
	}
	// The challenge re-quotes the bullet with other backticks and punctuation.
	final := rcReviewDataMarker + "\nMERGE_READY: 4\nDECISIONS:\n" +
		"- The fact-check now supports a defect whose consequence is unverified; every review publishes more findings\n" +
		"- A brand-new decision.\n"
	_, _, decisions, _, _, _ := rcParseReviewOutput(rcPutEarlierTags(final, refs))
	if len(decisions) != 2 || !strings.HasSuffix(decisions[0], "(earlier: 0123456789ab)") || strings.Contains(decisions[1], "(earlier:") {
		t.Errorf("decisions = %q", decisions)
	}
}

// A severity tag rides the same way: off for the check, back on the bullet.
func TestSeverityTagsLeaveAndReturn(t *testing.T) {
	draft := rcReviewDataMarker + "\nMERGE_READY: 3\nISSUES:\n" +
		"- a.go:14 — `b.Len()` counts bytes, not letters (severity: Medium)\n" +
		"- a.go:30 — the token is logged (severity: high) (earlier: 0123456789ab)\n" +
		"- nit: a.go:3 — the doc names `max`\n"
	stripped, refs := rcTakeEarlierTags(draft)
	if strings.Contains(stripped, "(severity:") || refs["a.go:14"] != "(severity: medium)" || refs["a.go:30"] != "(severity: high) (earlier: 0123456789ab)" {
		t.Fatalf("stripped:\n%s\nrefs=%v", stripped, refs)
	}
	_, risks, _, _, _, _ := rcParseReviewOutput(rcPutEarlierTags(stripped, refs))
	if len(risks) != 3 || !strings.HasSuffix(risks[0], "(severity: medium)") || !strings.HasSuffix(risks[1], "(severity: high) (earlier: 0123456789ab)") || strings.Contains(risks[2], "severity") {
		t.Errorf("risks = %q", risks)
	}
	for name, prompt := range map[string]string{"review": rcReviewSystem, "fast": rcFastReviewSystem, "sweep": rcSweepSystem} {
		if !strings.Contains(prompt, "(severity: high|medium|low)") {
			t.Errorf("the %s prompt does not ask for a severity", name)
		}
	}
	if !strings.Contains(rcChallengeSystemHead, "ONE ROOT CAUSE, ONE FINDING") || !strings.Contains(rcChallengeSystemHead, "Duplicate of issue N") {
		t.Error("the fact-check prompt lacks the duplicate rule")
	}
}
