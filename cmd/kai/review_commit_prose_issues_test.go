package main

import (
	"strings"
	"testing"
)

// The shape of the 2026-10-12 #430 draft: located bullets in the prose, a
// coda that stops after MERGE_READY.
func TestProseIssuesAreRecoveredWhenTheCodaHasNone(t *testing.T) {
	raw := "The marking path is sound.\n\n" +
		"- Examined kailab-control/internal/api/review_answered.go in full.\n" +
		"- nit: kailab-control/internal/api/github_pr_review.go:1157-1158 — `splitAnswered` is called twice; a comment would carry why.\n" +
		"- kailab-control/internal/db/pr_review_comments.go:268 — `reply[:answerExcerptMax]` can split a rune.\n" +
		"- kailab-control/internal/db/pr_review_comments.go:268 — `reply[:answerExcerptMax]` can split a rune.\n\n" +
		rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 5"
	got, n := rcRecoverProseIssues(raw)
	if n != 2 {
		t.Fatalf("recovered %d, want 2 located, de-duplicated bullets:\n%s", n, got)
	}
	_, issues, _, _, readiness, _ := rcParseReviewOutput(got)
	if len(issues) != 2 || !strings.HasPrefix(issues[0], "nit: kailab-control/internal/api/github_pr_review.go:1157") ||
		!strings.Contains(issues[1], "reply[:answerExcerptMax]") || readiness != 5 {
		t.Errorf("issues = %q readiness = %v", issues, readiness)
	}
}

func TestProseIssuesAreLeftAloneWhenTheCodaHasAList(t *testing.T) {
	for _, coda := range []string{"\nMERGE_READY: 5\nISSUES:\n(none)\n", "\nISSUES:\n- a.go:1 — the real one\n"} {
		raw := "- b.go:2 — a point the reviewer discussed and did not list\n" + rcReviewDataMarker + coda
		if got, n := rcRecoverProseIssues(raw); n != 0 || got != raw {
			t.Errorf("coda %q was changed (%d recovered)", coda, n)
		}
	}
	if _, n := rcRecoverProseIssues("- a.go:1 — no coda at all"); n != 0 {
		t.Error("a draft with no coda was changed")
	}
}

// The 2026-10-12 ensemble runs withheld a real byte-slicing defect 4/4 because
// the bullet added a database consequence the sources could not show.
func TestChallengeJudgesTheDefectNotEveryConsequence(t *testing.T) {
	if !strings.Contains(rcChallengeSystemHead, "JUDGE THE DEFECT, NOT EVERY CONSEQUENCE IT ADDS") ||
		!strings.Contains(rcChallengeSystemHead, "Mark the check unverified only when the defect itself") {
		t.Error("the fact-check prompt lacks the defect-not-consequence rule")
	}
}
