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
		Review: rcTestReview(rcEscapeIssue),
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "refuted", Reason: "cd persists", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", Reason: "expansion", Evidence: []rcCheckEvidence{
				{Source: 2, LineStart: 1, LineEnd: 1}, // valid
				{Source: 2, LineStart: 9, LineEnd: 9}, // unusable, dropped
			}},
		},
	}
	got, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("a valid citation lost its finding: %v", issues)
	}
	if strings.Contains(got, "This review is incomplete") {
		t.Fatalf("a resolved finding was marked incomplete: %s", got)
	}
}

// A supported finding whose only citation is unusable is dropped from the
// published ISSUES and surfaced as unresolved — while the OTHER supported
// finding still publishes. One bad citation costs one finding, never the review.
func TestReviewChallengeDropsUnbackedFindingKeepsTheRest(t *testing.T) {
	a := rcChallengeAnswer{
		Review: rcTestReview(rcFalseCDIssue, rcEscapeIssue),
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "supported", Reason: "real", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", Reason: "claimed", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 5, LineEnd: 9}}},
		},
	}
	got, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatalf("one bad citation withheld the whole review: %v", err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcFalseCDIssue {
		t.Fatalf("published wrong ISSUES: %v", issues)
	}
	if !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("unresolved allegation not surfaced: %s", got)
	}
}

// A runtime allegation needs a review_shell experiment. Without one it is
// unresolved even though the model claimed support; with one it publishes.
func TestReviewChallengeRuntimeClaimNeedsExperiment(t *testing.T) {
	runtime := rcChallengeAnswer{
		Review: rcTestReview(rcEscapeIssue),
		Checks: []rcIssueCheck{{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: true, Reason: "runtime", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}}},
	}
	got, err := rcValidateChallenge(rcTestAnswer(t, runtime), []string{rcEscapeIssue}, []string{`cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, issues, _, _, _, _ := rcParseReviewOutput(got); len(issues) != 0 {
		t.Fatalf("runtime claim published without an experiment: %v", issues)
	}
	if !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("unresolved runtime claim not surfaced: %s", got)
	}
	// With an experiment source backing it, the same claim publishes.
	runtime.Checks[0].Evidence[0].Source = 2 // cite the experiment result
	got, err = rcValidateChallenge(rcTestAnswer(t, runtime), []string{rcEscapeIssue}, []string{`cd "$HOME"`, "expansion observed"}, map[int]bool{2: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, issues, _, _, _, _ := rcParseReviewOutput(got); len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("experiment-backed runtime claim not published: %v", issues)
	}
	if strings.Contains(got, "This review is incomplete") {
		t.Fatalf("experiment-backed claim wrongly marked incomplete: %s", got)
	}
}

// An unverified allegation no longer withholds the whole review: the supported
// findings publish, the unresolved one is listed, and the review is incomplete.
func TestReviewChallengeUnverifiedPublishesSupportedAndMarksIncomplete(t *testing.T) {
	a := rcChallengeAnswer{
		Review: rcTestReview(rcEscapeIssue),
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "unverified", Reason: "could not settle without a shell"},
			{Issue: rcEscapeIssue, Verdict: "supported", Reason: "expansion", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},
		},
	}
	got, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("supported finding not published alongside an unresolved one: %v", issues)
	}
	if !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcFalseCDIssue) {
		t.Fatalf("unresolved allegation not surfaced: %s", got)
	}
}

// The whole challenge runs end-to-end through the provider path (structured
// submission) and reaches the same incomplete-but-published outcome.
func TestReviewChallengeUnverifiedThroughProvider(t *testing.T) {
	a := rcChallengeAnswer{
		Review: rcTestReview(),
		Checks: []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", Reason: "needs a shell"}},
	}
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil)
	if err != nil {
		t.Fatalf("unverified allegation withheld the review: %v", err)
	}
	if !strings.Contains(got, "This review is incomplete") || !strings.Contains(got, rcFalseCDIssue) {
		t.Fatalf("expected an incomplete review naming the unresolved claim: %s", got)
	}
}
