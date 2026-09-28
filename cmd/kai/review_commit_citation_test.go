package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// Invalid citation handling. A location that does not exist in its source's
// declared coordinates — an unknown source number, a range starting before the
// first line, reversed, or past the last line — is reported precisely (item,
// citation, source, range, reason) so ONE correction can be requested, and it
// makes the item it belongs to unresolved. It withholds neither the review nor
// its verdict: the other item stays published, and the unresolved one is listed
// under "Could not verify" without marking the review incomplete.
func TestReviewCitationDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     int
		start, end int
		want       string
	}{
		{"source", 99, 1, 1, "source number is out of range"},
		{"start before 1", 1, 0, 1, "row range is out of bounds (source has 2 row(s))"},
		{"past the end", 1, 1, 3, "row range is out of bounds (source has 2 row(s))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A SUPPORTED verdict on a bad citation: it would publish a
			// finding on evidence that does not exist, so it is degraded.
			a := rcCDChecks()
			a.Checks[0].Verdict, a.Checks[0].Finding = rcStatusSupported, "the cd does not apply to later lines"
			a.Checks[0].Evidence[0] = rcCheckEvidence{Source: tc.source, LineStart: tc.start, LineEnd: tc.end}
			res, problems, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
			if err != nil {
				t.Fatalf("an invalid location withheld the review: %v", err)
			}
			if len(problems) != 1 || !strings.Contains(problems[0].String(), tc.want) || !strings.HasPrefix(problems[0].String(), "check 1, citation 1, source "+strconv.Itoa(tc.source)) {
				t.Fatalf("imprecise problem report: %+v", problems)
			}
			if got := res.Allegations[0]; got.Status != rcStatusUnresolved || !strings.Contains(got.Reason, "citation 1 could not be resolved") || got.Remedy != "" || got.WithheldRemedy != rcFalseCDRemedy || got.Finding != "" {
				t.Fatalf("item with an unresolvable citation not degraded: %+v", got)
			}
			if len(res.unresolved()) != 1 || res.Allegations[1].Status != rcStatusSupported || !strings.Contains(res.Review, rcEscapeIssue) {
				t.Fatalf("the other finding was lost, or the open item was not reported as unresolved: %+v", res)
			}
			if !strings.Contains(couldNotVerifySection(res.Review), "- "+rcFalseCDIssue+" — citation 1 could not be resolved") {
				t.Fatalf("unresolved item not listed under Could not verify:\n%s", res.Review)
			}
		})
	}
	// A valid location that exists but is the WRONG evidence passes: the
	// system does not judge relevance. Stated here so the limit is tested, not
	// implied.
	a := rcCDChecks()
	a.Checks[0].Evidence[0] = rcCheckEvidence{Source: 2, LineStart: 1, LineEnd: 1}
	if _, problems, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources); err != nil || len(problems) != 0 {
		t.Fatalf("an existing location is valid regardless of relevance: %v %+v", err, problems)
	}
	if e := (rcCitationProblem{Item: 1, Citation: 1, Source: 1, SourceCount: 2, LineStart: 1, LineEnd: 99, Reason: "row range is out of bounds"}).String(); strings.Contains(e, "\n") || len(e) > 400 {
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
	sources := rcCDSources
	a := rcCDChecks()
	a.Checks[0].Evidence[0].LineEnd = 99 // a location that does not exist
	raw := rcTestAnswer(t, a)
	failed := provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "original", Name: "submit_review", Input: raw}}}
	msgs := []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "Check these two issues:\n" + rcFalseCDIssue + "\n" + rcEscapeIssue + "\n" + rcRenderSource(1, sources[0]) + rcRenderSource(2, sources[1])}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	got, err := rcValidateOrRepair(ctx, prov, model, rcChallengeSystemPrompt(false), msgs, failed, raw, "original", rcCDIssues, nil, sources)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got.Review)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("correction changed the expected findings: %v", issues)
	}
	t.Logf("model=%s: corrected seeded citation mismatch; retained only escaping defect", model)
}

// One bounded correction, then degradation — never withholding. The first
// answer cites a location that does not exist; the correction round reports
// it and asks for a complete resubmission under the original deadline, with
// tools restricted to submit_review. Whatever the correction cannot fix leaves
// the affected allegation unresolved and the review incomplete; a correction
// that cannot be obtained at all publishes the first answer in that degraded
// form.
func TestReviewCitationCorrection(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, outcome := range []string{"corrected", "still-invalid", "unverified", "missing-check", "new-issue", "truncated", "tool", "provider-error", "cancelled"} {
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
						if !strings.Contains(feedback, "check 1, citation 1, source 1") || !strings.Contains(feedback, "lines 1-99") || !strings.Contains(feedback, "row range is out of bounds") {
							t.Fatalf("missing precise feedback: %s", feedback)
						}
						switch outcome {
						case "still-invalid":
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
				if err != nil || got == nil {
					t.Fatalf("a citation slip withheld the review: %+v %v", got, err)
				}
				// The supported finding is published in every outcome.
				if !strings.Contains(got.Review, rcEscapeIssue) {
					t.Fatalf("supported finding lost: %s", got.Review)
				}
				switch outcome {
				case "corrected":
					// Refuted with a valid citation: clean, complete review.
					if got.Allegations[0].Status != rcStatusRefuted {
						t.Fatalf("correction not applied: %+v", got.Allegations[0])
					}
				case "unverified":
					if len(got.unresolved()) != 1 || got.Allegations[0].Status != rcStatusUnresolved || !strings.Contains(got.Allegations[0].Reason, "shell state") {
						t.Fatalf("unverified resubmission not published as unresolved: %+v", got.Allegations[0])
					}
				default:
					// Still invalid, or no usable correction at all: the first
					// answer's validated verdicts stand. Its verdict on the
					// affected item was a REFUTATION, which withholds the item
					// either way, so it stays refuted with the citation noted —
					// never listed under "Could not verify" as a doubt.
					if len(got.unresolved()) != 0 || got.Allegations[0].Status != rcStatusRefuted || !strings.Contains(got.Allegations[0].Reason, "could not be resolved") {
						t.Fatalf("item with an unresolvable citation not degraded: %+v", got.Allegations[0])
					}
					if strings.Contains(got.Review, rcFalseCDRemedy) {
						t.Fatalf("withheld remedy published:\n%s", got.Review)
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
	// Not retried — and not withheld: the item is unresolved and listed under
	// "Could not verify", the supported finding is kept, and the review is not
	// marked incomplete for one open item.
	if calls != 1 || err != nil || len(got.unresolved()) != 1 || !strings.Contains(got.Review, rcEscapeIssue) || !strings.Contains(couldNotVerifySection(got.Review), "- "+rcFalseCDIssue) || strings.Contains(got.Review, "## Findings\n\n### "+rcFalseCDIssue) {
		t.Fatalf("uncertainty retried, withheld, or published as a finding: calls=%d %+v %v", calls, got, err)
	}
}

// A REFUTED verdict on a bad citation withholds the allegation exactly as a
// refutation does. Degrading it to unresolved used to list it under "Could not
// verify" — publishing, as a doubt, a claim the check had rejected (8 of 10
// such items in a live run with openai/gpt-5.4-mini as the gate).
func TestRefutationOnABadCitationStaysWithheld(t *testing.T) {
	a := rcCDChecks()
	a.Checks[0].Evidence[0] = rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 99}
	res, _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Allegations[0]; got.Status != rcStatusRefuted || !strings.Contains(got.Reason, "could not be resolved") {
		t.Fatalf("allegation = %+v, want refuted with the citation noted", got)
	}
	if strings.Contains(res.Review, rcFalseCDIssue) {
		t.Fatalf("a refuted claim was published:\n%s", res.Review)
	}
}

// A check that re-quotes its bullet with other backticks, spacing or trailing
// punctuation is still a check of that bullet.
func TestCheckMatchesItsBulletDespiteCosmeticDifferences(t *testing.T) {
	a := rcCDChecks()
	a.Checks[0].Issue = "  " + strings.ReplaceAll(rcFalseCDIssue, " ", "  ") + "."
	if _, _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources); err != nil {
		t.Fatalf("a cosmetically different echo was rejected: %v", err)
	}
	a = rcCDChecks()
	a.Checks[1].Issue = a.Checks[0].Issue // two checks of one bullet
	if _, _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources); err == nil {
		t.Fatal("a duplicated check was accepted")
	}
}

func TestEmptyListItemWithAnExplanation(t *testing.T) {
	for _, s := range []string{"(none — the one concern has no trigger today)", "none: nothing found", "(none)"} {
		if !rcIsEmptyListItem(s) {
			t.Errorf("%q should be an empty list item", s)
		}
	}
	if rcIsEmptyListItem("nonempty.go:3 — none of the callers check the error") {
		t.Error("a real finding was taken for an empty list")
	}
}

// A range written end-first names the same lines and resolves.
func TestReversedCitationRangeResolves(t *testing.T) {
	a := rcCDChecks()
	a.Checks[0].Evidence[0] = rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 1}
	_, problems, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
	if err != nil || len(problems) != 0 {
		t.Fatalf("reversed range not accepted: %v %+v", err, problems)
	}
}

// Two bullets that differ only cosmetically share a key; a check that echoes
// one of them cosmetically is ambiguous and must not be filed under either.
func TestAmbiguousCosmeticKeyIsNotUsed(t *testing.T) {
	issues := []string{"a.go:1 — `x` is nil", "a.go:1 — x is nil"}
	a := rcChallengeAnswer{IntentMatch: "verified", MergeReady: 4, Checks: []rcIssueCheck{
		{Issue: issues[0], Verdict: "refuted", Reason: "r", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}},
		{Issue: "a.go:1 — x is nil.", Verdict: "refuted", Reason: "r", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}},
	}}
	res, _, err := rcValidateChallenge(rcTestAnswer(t, a), issues, nil, rcCDSources)
	if err == nil {
		t.Fatalf("an ambiguous cosmetic echo was filed under a bullet: %+v", res.Allegations)
	}
}
