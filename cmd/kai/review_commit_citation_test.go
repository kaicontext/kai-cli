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

func TestReviewCitationDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, quote string
		source      int
		want        string
	}{
		{"source", "pwd", 99, "source number is out of range"},
		{"empty", " \n", 1, "quote is empty"},
		{"mismatch", "cd /tmp && pwd; pwd", 1, "quote does not exactly match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			a.Checks[0].Evidence[0] = rcCheckEvidence{Source: tc.source, Quote: tc.quote}
			_, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`})
			var citation *rcCitationError
			if !errors.As(err, &citation) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "check 1, citation 1") {
				t.Fatalf("imprecise error: %v", err)
			}
		})
	}
	e := (&rcCitationError{Quote: strings.Repeat("x", 10000) + "\nsecret-tail"}).Error()
	if len(e) > 400 || strings.Contains(e, "secret-tail") || strings.Contains(e, "\n") {
		t.Fatal("diagnostic quote is unbounded or not escaped")
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
	a.Checks[0].Evidence[0].Quote = "cd /tmp && pwd; pwd"
	raw := rcTestAnswer(t, a)
	failed := provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "original", Name: "submit_review", Input: raw}}}
	msgs := []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "Check these two issues:\n" + rcFalseCDIssue + "\n" + rcEscapeIssue + "\nSOURCE 1:\n" + sources[0] + "\nSOURCE 2:\n" + sources[1]}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	got, err := rcValidateOrRepairCitation(ctx, prov, model, msgs, failed, raw, "original", []string{rcFalseCDIssue, rcEscapeIssue}, sources)
	if err != nil {
		t.Fatal(err)
	}
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
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
						a.Checks[0].Evidence[0].Quote = "paraphrased evidence"
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
						if !strings.Contains(feedback, "check 1, citation 1, source 1") || !strings.Contains(feedback, "paraphrased evidence") {
							t.Fatal("missing precise feedback")
						}
						switch outcome {
						case "mismatch":
							a.Checks[0].Evidence[0].Quote = "still wrong"
						case "unverified":
							a.Checks[0].Verdict = "unverified"
						case "missing-check":
							a.Checks = a.Checks[:1]
						case "new-issue":
							a.Review = rcTestReview("invented new defect")
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
				got, err := rcChallengeReview(ctx, p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{rcCDSource, `cd "$HOME"`}, nil)
				if outcome == "corrected" {
					if err != nil || !strings.Contains(got, rcEscapeIssue) || strings.Contains(got, rcFalseCDIssue) {
						t.Fatalf("correction rejected: %q %v", got, err)
					}
				} else if err == nil || got != "" {
					t.Fatalf("unchecked result escaped: %q %v", got, err)
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
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{rcCDSource, `cd "$HOME"`}, nil)
	if calls != 1 || err == nil || got != "" {
		t.Fatalf("uncertainty retried or published: calls=%d %q %v", calls, got, err)
	}
}
