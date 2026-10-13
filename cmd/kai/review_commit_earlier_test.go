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
	if refs["loc:internal/smoketest/excerpt.go:13"] != "(earlier: 0123456789ab)" {
		t.Fatalf("refs = %v", refs)
	}

	// The challenge keeps each bullet's own words (check.Issue = issues[id]);
	// the duplicate fold may add an "(also: …)".
	final := "## Findings\n\n### internal/smoketest/excerpt.go:13 — panics on a non-positive limit\n" +
		"===REVIEW-DATA===\nISSUES:\n" +
		"- internal/smoketest/excerpt.go:13 — when `limit <= 0`, `r[:limit]` panics (also: internal/smoketest/excerpt.go:15)\n" +
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
	if strings.Contains(stripped, "(severity:") || refs["loc:a.go:14"] != "(severity: medium)" || refs["loc:a.go:30"] != "(severity: high) (earlier: 0123456789ab)" {
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

// From Kai's review of kai-cli#174: two different defects on one line keep
// their own severities, and a bullet that already has one tag still gets the
// other back.
func TestTagsStayWithTheirOwnDefect(t *testing.T) {
	draft := rcReviewDataMarker + "\nISSUES:\n" +
		"- a.go:14 — `b.Len()` counts bytes, not letters (severity: medium)\n" +
		"- a.go:14 — the limit is checked before the rune is written, one too late (severity: low)\n"
	stripped, refs := rcTakeEarlierTags(draft)
	_, risks, _, _, _, _ := rcParseReviewOutput(rcPutEarlierTags(stripped, refs))
	if len(risks) != 2 || !strings.HasSuffix(risks[0], "(severity: medium)") || !strings.HasSuffix(risks[1], "(severity: low)") {
		t.Errorf("risks = %q", risks)
	}

	refs = map[string]string{}
	rcRememberTags(refs, "issues", "- a.go:30 — the token is logged", "(severity: high) (earlier: 0123456789ab)")
	got := rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- a.go:30 — the token is logged (severity: high)\n", refs)
	_, risks, _, _, _, _ = rcParseReviewOutput(got)
	if len(risks) != 1 || !strings.HasSuffix(risks[0], "(severity: high) (earlier: 0123456789ab)") || strings.Count(risks[0], "severity") != 1 {
		t.Errorf("a bullet with its severity did not get its earlier-ref back once: %q", risks)
	}
}

// From Kai's review of kai-cli#174: the location fallback only serves one
// bullet the fact-check reworded; an untagged bullet sharing the line stays
// untagged, and a nit takes no severity.
func TestLocationFallbackOnlyForALoneBullet(t *testing.T) {
	refs := map[string]string{}
	rcRememberTags(refs, "issues", "- a.go:14 — `b.Len()` counts bytes", "(severity: medium)")

	// Reworded by the fact-check, alone on its line: the fallback finds it.
	got := rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- a.go:14 — the length check counts bytes, not letters\n", refs)
	_, risks, _, _, _, _ := rcParseReviewOutput(got)
	if len(risks) != 1 || !strings.HasSuffix(risks[0], "(severity: medium)") {
		t.Errorf("a lone reworded bullet lost its tag: %q", risks)
	}

	// A different, untagged bullet on the same line beside it: no tag for it.
	got = rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- a.go:14 — `b.Len()` counts bytes\n- a.go:14 — the check runs before the write\n", refs)
	_, risks, _, _, _, _ = rcParseReviewOutput(got)
	if len(risks) != 2 || !strings.HasSuffix(risks[0], "(severity: medium)") || strings.Contains(risks[1], "severity") {
		t.Errorf("risks = %q", risks)
	}

	// A bullet that became a nit keeps its earlier-ref but takes no severity.
	refs = map[string]string{}
	rcRememberTags(refs, "issues", "- a.go:3 — the doc names `max`", "(severity: low) (earlier: 0123456789ab)")
	got = rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- nit: a.go:3 — the doc names `max`\n", refs)
	_, risks, _, _, _, _ = rcParseReviewOutput(got)
	if len(risks) != 1 || strings.Contains(risks[0], "severity") || !strings.HasSuffix(risks[0], "(earlier: 0123456789ab)") {
		t.Errorf("nit = %q", risks)
	}
}

// From Kai's review of kai-cli#174: the location fallback never carries an
// earlier-ref — a new defect alone on a line where a carried point was
// refuted must not be taken for that point and suppressed.
func TestLocationFallbackNeverCarriesAnEarlierRef(t *testing.T) {
	refs := map[string]string{}
	rcRememberTags(refs, "issues", "- a.go:30 — the token is logged", "(severity: high) (earlier: 0123456789ab)")
	got := rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- a.go:30 — the retry loop never backs off\n", refs)
	_, risks, _, _, _, _ := rcParseReviewOutput(got)
	if len(risks) != 1 || strings.Contains(risks[0], "earlier") {
		t.Errorf("a different defect took the earlier-ref: %q", risks)
	}
	// The exact bullet still gets both back.
	got = rcPutEarlierTags(rcReviewDataMarker+"\nISSUES:\n- a.go:30 — the token is logged\n", refs)
	_, risks, _, _, _, _ = rcParseReviewOutput(got)
	if len(risks) != 1 || !strings.HasSuffix(risks[0], "(severity: high) (earlier: 0123456789ab)") {
		t.Errorf("the exact bullet lost its tags: %q", risks)
	}
}
