package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Invalid citation handling. A location that does not exist — an unknown
// source number, a line range starting before 1, reversed, or past the last
// line — is a precise, retryable citation error that names the check, the
// citation and the reason. Validation still fails on it; cite-by-location
// removed the exact-quote requirement, not the location check.
func TestReviewCitationDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     int
		start, end int
		want       string
	}{
		{"source", 99, 1, 1, "source number is out of range"},
		{"start before 1", 1, 0, 1, "line range is out of bounds (source has 2 line(s))"},
		{"reversed", 1, 2, 1, "line range is out of bounds"},
		{"past the end", 1, 1, 3, "line range is out of bounds (source has 2 line(s))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			a.Checks[0].Evidence[0] = rcCheckEvidence{Source: tc.source, LineStart: tc.start, LineEnd: tc.end}
			_, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
			var citation *rcCitationError
			if !errors.As(err, &citation) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "check 1, citation 1") {
				t.Fatalf("imprecise error: %v", err)
			}
		})
	}
	// A valid location that exists but is the WRONG evidence passes validation:
	// the system does not judge relevance. Stated here so the limit is tested,
	// not implied.
	a := rcCDChecks()
	a.Checks[0].Evidence[0] = rcCheckEvidence{Source: 2, LineStart: 1, LineEnd: 1}
	if _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources); err != nil {
		t.Fatalf("an existing location is valid regardless of relevance: %v", err)
	}
	if e := (&rcCitationError{Check: 1, Citation: 1, Source: 1, SourceCount: 2, LineStart: 1, LineEnd: 99, Reason: "line range is out of bounds"}).Error(); strings.Contains(e, "\n") || len(e) > 400 {
		t.Fatalf("diagnostic is not a single bounded line: %q", e)
	}
}

// Deliberately seeds a citation formatting error in the #418-derived specimen.
// This evaluates live correction, not the unknown exact production mismatch.
func TestReviewCitationLiveRepairDesktop418(t *testing.T) {
	if os.Getenv("KAI_REVIEW_LIVE_EVAL") != "1" {
		t.Skip("opt-in paid model evaluation")
	}
	if dir := os.Getenv("KAI_REVIEW_EVAL_CONFIG_DIR"); dir != "" {
		old := kaiDir
		kaiDir = dir
		t.Cleanup(func() { kaiDir = old })
	}
	prov, model, _ := rcReviewProvider()
	if prov == nil {
		t.Fatal("no configured provider")
	}
	sources := []string{rcCDSource, `cd "$HOME"`}
	a := rcCDChecks()
	a.Checks[0].Evidence[0].LineEnd = 99 // a location that does not exist
	raw := rcTestAnswer(t, a)
	failed := provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "original", Name: "submit_review", Input: raw}}}
	msgs := []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "Check these two issues:\n" + rcFalseCDIssue + "\n" + rcEscapeIssue + "\n" + rcNumberedSource(1, sources[0]) + rcNumberedSource(2, sources[1])}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	got, err := rcValidateOrRepairCitation(ctx, prov, model, msgs, failed, raw, "original", rcCDIssues, nil, sources)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got.Review)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("correction changed the expected findings: %v", issues)
	}
	t.Logf("model=%s: corrected seeded citation mismatch; retained only escaping defect", model)
}

func TestReviewCitationCorrection(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, outcome := range []string{"corrected", "mismatch", "unverified", "missing-check", "new-issue", "truncated", "tool", "provider-error", "cancelled"} {
			t.Run(outcome+map[bool]string{false: "-text", true: "-tool"}[structured], func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				var firstCtx context.Context
				p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
					calls++
					a := rcCDChecks()
					if calls == 1 {
						firstCtx = c
						a.Checks[0].Evidence[0].LineEnd = 99 // a location that does not exist
						if outcome == "cancelled" {
							cancel()
						}
					} else {
						if calls > 2 {
							t.Fatal("unbounded retry")
						}
						if c != firstCtx {
							t.Fatal("correction reset deadline")
						}
						if len(req.Tools) != 1 || req.Tools[0].Name != "submit_review" {
							t.Fatal("correction gained tools")
						}
						if len(req.Messages) != 3 {
							t.Fatal("original evidence or failed answer lost")
						}
						last := req.Messages[2].Parts[0]
						var feedback string
						if structured {
							r, ok := last.(message.ToolResult)
							if !ok || !r.IsError || r.ToolCallID != "submission" {
								t.Fatalf("bad tool feedback: %#v", last)
							}
							feedback = r.Content
						} else {
							feedback = last.(message.TextContent).Text
						}
						if !strings.Contains(feedback, "check 1, citation 1, source 1") || !strings.Contains(feedback, "lines 1-99") || !strings.Contains(feedback, "line range is out of bounds") {
							t.Fatalf("missing precise feedback: %s", feedback)
						}
						switch outcome {
						case "mismatch":
							a.Checks[0].Evidence[0].LineStart = 0 // still a location that does not exist
						case "unverified":
							a.Checks[0].Verdict = "unverified"
						case "missing-check":
							a.Checks = a.Checks[:1]
						case "new-issue":
							a.Checks = append(a.Checks, rcIssueCheck{Issue: "invented new defect", Verdict: "supported", Reason: "r", Finding: "f", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}})
						case "truncated":
							return provider.Response{FinishReason: message.FinishReasonMaxTokens}, nil
						case "tool":
							return provider.Response{Parts: []message.ContentPart{message.ToolCall{Name: "review_shell", Input: `{"script":"pwd"}`}}}, nil
						case "provider-error":
							return provider.Response{}, errors.New("provider down")
						}
					}
					var part message.ContentPart = message.TextContent{Text: rcTestAnswer(t, a)}
					if structured {
						part = message.ToolCall{ID: "submission", Name: "submit_review", Input: rcTestAnswer(t, a)}
					}
					return provider.Response{Parts: []message.ContentPart{part}}, nil
				}}
				got, err := rcChallengeReview(ctx, p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
				switch outcome {
				case "corrected":
					if err != nil || !strings.Contains(got.Review, rcEscapeIssue) || strings.Contains(got.Review, rcFalseCDIssue) {
						t.Fatalf("correction rejected: %+v %v", got, err)
					}
				case "unverified":
					// An unverified item is no longer a reason to withhold the
					// review: it is published as unresolved and the review is
					// marked incomplete, with the supported finding kept.
					if err != nil || !got.Incomplete || !strings.Contains(got.Review, rcEscapeIssue) {
						t.Fatalf("unresolved item not published as incomplete: %+v %v", got, err)
					}
				default:
					if err == nil || got != nil {
						t.Fatalf("unchecked result escaped: %+v %v", got, err)
					}
				}
				want := 2
				if outcome == "cancelled" {
					want = 1
				}
				if calls != want {
					t.Fatalf("calls=%d want %d", calls, want)
				}
			})
		}
	}
}

func TestReviewCitationDoesNotRetrySemanticUncertainty(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		calls++
		a := rcCDChecks()
		a.Checks[0].Verdict = "unverified"
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	// Not retried — and no longer withheld: the item is unresolved, the
	// supported finding is kept, and the review is marked incomplete.
	if calls != 1 || err != nil || !got.Incomplete || !strings.Contains(got.Review, rcEscapeIssue) || strings.Contains(got.Review, "## Findings\n\n### "+rcFalseCDIssue) {
		t.Fatalf("uncertainty retried, withheld, or published as a finding: calls=%d %+v %v", calls, got, err)
	}
}
