package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Evidence is cited by location; the system extracts the text. Out-of-range
// sources and out-of-bounds line ranges are reported unusable so the caller can
// drop that one citation rather than trust a fabricated excerpt.
func TestReviewCitationExtractsByLocation(t *testing.T) {
	sources := []string{rcCDSource, `cd "$HOME"`}
	for _, tc := range []struct {
		name   string
		ev     rcCheckEvidence
		want   string
		wantOK bool
	}{
		{"first line", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 1}, "cd /tmp && pwd", true},
		{"line range", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 2}, "cd /tmp && pwd\npwd", true},
		{"single line source", rcCheckEvidence{Source: 2, LineStart: 1, LineEnd: 1}, `cd "$HOME"`, true},
		{"source out of range", rcCheckEvidence{Source: 9, LineStart: 1, LineEnd: 1}, "", false},
		{"source zero", rcCheckEvidence{Source: 0, LineStart: 1, LineEnd: 1}, "", false},
		{"line past end", rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 9}, "", false},
		{"inverted range", rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 1}, "", false},
		{"line zero", rcCheckEvidence{Source: 1, LineStart: 0, LineEnd: 1}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rcExtractCitation(sources, tc.ev)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("extract=%q,%v want %q,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	// The model cites against the numbered rendering; the numbers it sees must be
	// the line coordinates extraction resolves.
	if n := rcNumberedSource(1, rcCDSource); !strings.Contains(n, "    1| cd /tmp && pwd") || !strings.Contains(n, "    2| pwd") || strings.Contains(n, "    3|") {
		t.Fatalf("numbered source miscounts lines: %q", n)
	}
}

// One unusable citation drops that citation, not the finding: a supported check
// with any valid citation survives, and the review is not marked incomplete.
func TestReviewChallengeToleratesOneBadCitation(t *testing.T) {
	a := rcChallengeAnswer{
		Assessment:  "Checked the terminal command path.",
		IntentMatch: "partial",
		MergeReady:  3,
		Summary:     "One escaping defect stands.",
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "refuted", RequiresRuntime: rcBool(false), Reason: "cd persists", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "expansion", Finding: "Escape the interpolated path.", Evidence: []rcCheckEvidence{
				{Source: 2, LineStart: 1, LineEnd: 1}, // valid
				{Source: 2, LineStart: 9, LineEnd: 9}, // unusable, dropped
			}},
		},
	}
	got, unresolved, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("a valid citation lost its finding: %v", issues)
	}
	if len(unresolved) != 0 || strings.Contains(got, "This review is incomplete") {
		t.Fatalf("a resolved finding was marked incomplete: %v %s", unresolved, got)
	}
}

// A supported finding whose only citation is unusable is dropped from the
// published ISSUES and surfaced as unresolved — while the OTHER supported
// finding still publishes. One bad citation costs one finding, never the review.
func TestReviewChallengeDropsUnbackedFindingKeepsTheRest(t *testing.T) {
	a := rcChallengeAnswer{
		Assessment:  "Checked both allegations against the sources.",
		IntentMatch: "partial",
		MergeReady:  3,
		Summary:     "One defect confirmed; one could not be settled.",
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "real", Finding: "Fix the cd handling.", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "claimed", Finding: "Fix the escaping.", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 5, LineEnd: 9}}},
		},
	}
	got, unresolved, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatalf("one bad citation withheld the whole review: %v", err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcFalseCDIssue {
		t.Fatalf("published wrong ISSUES: %v", issues)
	}
	if len(unresolved) != 1 || unresolved[0] != rcEscapeIssue {
		t.Fatalf("unresolved list wrong: %v", unresolved)
	}
	if !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("unresolved allegation not surfaced: %s", got)
	}
}

// A runtime allegation needs a review_shell experiment. Without one it is
// unresolved even though the model claimed support; with one it publishes.
func TestReviewChallengeRuntimeClaimNeedsExperiment(t *testing.T) {
	answer := func() rcChallengeAnswer {
		return rcChallengeAnswer{
			Assessment:  "Assessed the escaping behavior.",
			IntentMatch: "partial",
			MergeReady:  3,
			Summary:     "Escaping needs runtime confirmation.",
			Checks:      []rcIssueCheck{{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "runtime", Finding: "Escape the path.", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}}},
		}
	}
	got, unresolved, err := rcValidateChallenge(rcTestAnswer(t, answer()), []string{rcEscapeIssue}, []string{`cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, issues, _, _, _, _ := rcParseReviewOutput(got); len(issues) != 0 {
		t.Fatalf("runtime claim published without an experiment: %v", issues)
	}
	if len(unresolved) != 1 || !strings.Contains(got, "This review is incomplete") {
		t.Fatalf("unresolved runtime claim not surfaced: %v %s", unresolved, got)
	}
	// With an experiment source backing it, the same claim publishes.
	backed := answer()
	backed.Checks[0].Evidence[0].Source = 2 // cite the experiment result
	got, unresolved, err = rcValidateChallenge(rcTestAnswer(t, backed), []string{rcEscapeIssue}, []string{`cd "$HOME"`, "expansion observed"}, map[int]bool{2: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, issues, _, _, _, _ := rcParseReviewOutput(got); len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("experiment-backed runtime claim not published: %v", issues)
	}
	if len(unresolved) != 0 || strings.Contains(got, "This review is incomplete") {
		t.Fatalf("experiment-backed claim wrongly marked incomplete: %v %s", unresolved, got)
	}
}

// An unverified allegation no longer withholds the whole review: the supported
// findings publish, the unresolved one is listed, and the review is incomplete.
func TestReviewChallengeUnverifiedPublishesSupportedAndMarksIncomplete(t *testing.T) {
	a := rcChallengeAnswer{
		Assessment:  "One allegation could not be settled without a shell.",
		IntentMatch: "partial",
		MergeReady:  3,
		Summary:     "Escaping confirmed; cd behavior unresolved.",
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "could not settle without a shell"},
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "expansion", Finding: "Escape the path.", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},
		},
	}
	got, unresolved, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("supported finding not published alongside an unresolved one: %v", issues)
	}
	if len(unresolved) != 1 || !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcFalseCDIssue) {
		t.Fatalf("unresolved allegation not surfaced: %v %s", unresolved, got)
	}
}

// The whole challenge runs end-to-end through the provider path and reaches the
// same incomplete-but-published outcome, returning the unresolved list.
func TestReviewChallengeUnverifiedThroughProvider(t *testing.T) {
	a := rcChallengeAnswer{
		Assessment:  "Could not settle the allegation without a shell.",
		IntentMatch: "partial",
		MergeReady:  4,
		Summary:     "Unresolved pending runtime evidence.",
		Checks:      []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "needs a shell"}},
	}
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	got, unresolved, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil)
	if err != nil {
		t.Fatalf("unverified allegation withheld the review: %v", err)
	}
	if len(unresolved) != 1 || !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcFalseCDIssue) {
		t.Fatalf("expected an incomplete review naming the unresolved claim: %v %s", unresolved, got)
	}
}

// The model-authored assessment/summary/decisions must not assert an allegation
// the challenge did not support. A summary that confidently repeats a finding
// validation downgraded fails the challenge closed rather than publishing a
// review that contradicts its own verdicts.
func TestReviewChallengeRejectsFreeTextAssertingNonSupportedFinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rcChallengeAnswer)
	}{
		{"summary repeats a downgraded finding", func(a *rcChallengeAnswer) {
			a.Checks[1].Evidence = []rcCheckEvidence{{Source: 2, LineStart: 9, LineEnd: 9}} // escape downgraded
			a.Summary = "Confirmed defect: " + rcEscapeIssue
		}},
		{"assessment repeats a refuted finding", func(a *rcChallengeAnswer) {
			a.Assessment = "The change is broken because " + rcFalseCDIssue // falseCD is refuted
		}},
		{"decision repeats a refuted finding", func(a *rcChallengeAnswer) {
			a.Decisions = []string{rcFalseCDIssue}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			tc.mutate(&a)
			got, _, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
			if err == nil || got != "" {
				t.Fatalf("published a review whose free text asserts a non-supported finding: %q %v", got, err)
			}
		})
	}
}

// Each unresolved allegation is reported with its ACTUAL reason. A claim
// downgraded for an invalid citation must not be described as needing a runtime
// sandbox, and vice versa.
func TestReviewChallengeIncompleteBannerGivesPerClaimReason(t *testing.T) {
	a := rcChallengeAnswer{
		Assessment:  "Neither allegation could be settled.",
		IntentMatch: "partial",
		MergeReady:  4,
		Summary:     "Two open questions remain.",
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "x", Finding: "fix", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 9, LineEnd: 9}}}, // bad citation
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(true), Reason: "y", Finding: "fix", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},   // runtime, no experiment
		},
	}
	got, unresolved, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("want both allegations unresolved: %v", unresolved)
	}
	if !strings.Contains(got, "no usable citation") {
		t.Fatalf("citation-invalid claim not given its real reason: %s", got)
	}
	if !strings.Contains(got, "requires a runtime experiment") {
		t.Fatalf("runtime claim not given its real reason: %s", got)
	}
}
