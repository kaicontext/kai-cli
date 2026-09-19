package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// A submission that does not decode used to sink the whole review. Measured
// across 44 live reviews on 2026-09-19, that was the largest single cause of a
// NEUTRAL "Review did not finish" check: 6 of the 13 failures whose job logs
// were read, in two shapes — prose where a submit_review call was required
// (kai-api#3, #4: "invalid character 'E'/'k' looking for beginning of value")
// and `checks` as an array of strings (kai-tui#138, #141, #142, kai-engine#106).
// In every one of them the review agent had already finished; only the
// verification gate's parse failed.
//
// One bounded resubmission now gets asked for, mirroring the citation repair.
// The fail-closed property is unchanged — a review whose claims were never
// checked is still withheld — it just no longer happens on the first slip.
const rcMalformedProse = "Excellent — I examined both issues. The first is refuted and the second holds."

// checksAsStrings is the observed wrong shape: valid JSON, right field names,
// array elements that are strings instead of objects.
const rcChecksAsStrings = `{"intent_match":"partial","merge_ready":3,"checks":["the cd issue is refuted","the escaping issue is supported"],"decisions":[]}`

func TestReviewMalformedSubmissionRepaired(t *testing.T) {
	for _, tc := range []struct {
		name, first, wantFeedback string
	}{
		{"prose", rcMalformedProse, "looking for beginning of value"},
		{"checks-as-strings", rcChecksAsStrings, "arrays of OBJECTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var firstCtx context.Context
			p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
				calls++
				if !req.RequireToolUse {
					t.Error("challenge must structurally require a tool call")
				}
				if calls == 1 {
					firstCtx = c
					return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: tc.first}}}, nil
				}
				if calls > 2 {
					t.Fatal("unbounded retry")
				}
				if c != firstCtx {
					t.Error("correction reset the deadline")
				}
				if len(req.Tools) != 1 || req.Tools[0].Name != "submit_review" {
					t.Error("correction gained tools")
				}
				feedback := req.Messages[len(req.Messages)-1].Parts[0].(message.TextContent).Text
				if !strings.Contains(feedback, tc.wantFeedback) {
					t.Errorf("feedback did not name the problem: %s", feedback)
				}
				if !strings.Contains(feedback, "Your findings do not change") {
					t.Error("feedback failed to hold the assessment fixed")
				}
				return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "fixed", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
			}}
			got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
			if err != nil || got == nil {
				t.Fatalf("a malformed submission withheld the review: %+v %v", got, err)
			}
			if calls != 2 {
				t.Fatalf("expected exactly one correction, got %d calls", calls)
			}
			// The repair re-expresses the SAME assessment: the refuted
			// allegation stays dropped and the supported one stays published.
			_, issues, _, _, _, _ := rcParseReviewOutput(got.Review)
			if len(issues) != 1 || issues[0] != rcEscapeIssue {
				t.Fatalf("repair changed the findings: %v", issues)
			}
		})
	}
}

// When the correction cannot be obtained or is itself malformed, the ORIGINAL
// decode error propagates: there is nothing to degrade to, since no verdict
// ever decoded. This is the property that keeps an unchecked draft unpublished.
func TestReviewMalformedSubmissionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second func() (provider.Response, error)
	}{
		{"still-malformed", func() (provider.Response, error) {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: "Still prose, sorry."}}}, nil
		}},
		{"provider-error", func() (provider.Response, error) {
			return provider.Response{}, errors.New("provider down")
		}},
		{"truncated", func() (provider.Response, error) {
			return provider.Response{FinishReason: message.FinishReasonMaxTokens}, nil
		}},
		{"wrong-tool", func() (provider.Response, error) {
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{Name: "review_shell", Input: `{"script":"pwd"}`}}}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
				calls++
				if calls == 1 {
					return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcMalformedProse}}}, nil
				}
				if calls > 2 {
					t.Fatal("unbounded retry")
				}
				return tc.second()
			}}
			got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
			if err == nil {
				t.Fatalf("an unchecked draft was published: %+v", got)
			}
			if got != nil {
				t.Fatalf("withholding must return no result, got %+v", got)
			}
			// The reported cause stays the decode failure, not whatever went
			// wrong during the repair — the repair is an implementation
			// detail of the gate, and the reason the review was withheld is
			// that the submission never decoded.
			if !strings.Contains(err.Error(), "invalid challenge JSON") {
				t.Fatalf("lost the original cause: %v", err)
			}
			if calls != 2 {
				t.Fatalf("expected exactly one correction attempt, got %d calls", calls)
			}
		})
	}
}

// A well-formed submission must not pay for any of this: no second call.
func TestReviewWellFormedSubmissionIsNotRetried(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
		calls++
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "ok", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	if _, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a clean submission cost %d calls", calls)
	}
}
