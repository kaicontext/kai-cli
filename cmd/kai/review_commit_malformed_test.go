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

// rcChecksAsStrings is the observed wrong shape: valid JSON, right field names,
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

// A repair that decodes but still cites a location that does not exist is a
// state the structural path could not reach before: the old code returned
// (nil, err) on a decode failure and never retried, so "malformed, then
// decoded, then bad citation" had no representation.
//
// It publishes, degraded. That is not a new policy invented here — it is the
// policy rcValidateChallenge already applies to any answer that decodes: the
// item whose citation points nowhere becomes unresolved, the items that stand
// on their own are published, and the review is marked incomplete. What is
// new is only that a structurally malformed first attempt can now reach it.
// Withholding instead would discard verdicts that validated, which is the
// behaviour this whole change exists to stop.
func TestReviewMalformedRepairWithBadCitationPublishesDegraded(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcMalformedProse}}}, nil
		}
		if calls > 2 {
			t.Fatal("unbounded retry: a structural repair must not chain into a citation repair")
		}
		a := rcCDChecks()
		a.Checks[0].Evidence[0].LineEnd = 99 // decodes, but points nowhere
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "fixed", Name: "submit_review", Input: rcTestAnswer(t, a)}}}, nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || got == nil {
		t.Fatalf("a repaired answer with one bad citation withheld the review: %+v %v", got, err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly one correction, got %d calls", calls)
	}
	if s := got.Allegations[0].Status; s != rcStatusUnresolved {
		t.Errorf("item with the unresolvable citation not degraded: %v", s)
	}
	if s := got.Allegations[1].Status; s != rcStatusSupported {
		t.Errorf("the independently supported finding was lost: %v", s)
	}
	if !got.Incomplete {
		t.Error("a review with an unresolved item must be marked incomplete")
	}
	if !strings.Contains(got.Review, rcEscapeIssue) {
		t.Error("the supported finding is missing from the published review")
	}
}

// The repair is keyed off rcValidateChallenge returning an error at all, not
// off JSON decoding specifically — so a submission that decodes cleanly but
// breaks the PROTOCOL gets the same single correction. That family is live:
// kai-server#266 and kai-cli#122's own review both died on "challenge omitted
// reasoning, duplicated a check, or checked an unknown issue", with a complete
// draft in hand.
func TestReviewProtocolRejectionRepaired(t *testing.T) {
	for _, tc := range []struct {
		name, wantCause string
		break_          func(*rcChallengeAnswer)
	}{
		{"no-reasoning", "omitted reasoning", func(a *rcChallengeAnswer) { a.Checks[0].Reason = "" }},
		{"duplicated-check", "duplicated", func(a *rcChallengeAnswer) { a.Checks[1].Issue = a.Checks[0].Issue }},
		{"unknown-issue", "unknown issue", func(a *rcChallengeAnswer) { a.Checks[0].Issue = "an issue nobody raised" }},
		{"unknown-verdict", "unknown verdict", func(a *rcChallengeAnswer) { a.Checks[0].Verdict = "probably-fine" }},
		{"bad-merge-ready", "merge_ready", func(a *rcChallengeAnswer) { a.MergeReady = 99 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
				calls++
				a := rcCDChecks()
				if calls == 1 {
					tc.break_(&a)
				} else if calls > 2 {
					t.Fatal("unbounded retry")
				} else {
					feedback := req.Messages[len(req.Messages)-1].Parts[0].(message.ToolResult).Content
					if !strings.Contains(feedback, tc.wantCause) {
						t.Errorf("feedback did not carry the rejection reason %q: %s", tc.wantCause, feedback)
					}
					// A protocol rejection needs the CONTENT rules, not just
					// the shape rules — "checks are arrays of objects" does
					// not tell a model it dropped a required reason.
					if !strings.Contains(feedback, "EXACTLY ONE check per bullet") || !strings.Contains(feedback, "non-empty \"reason\"") {
						t.Errorf("feedback omitted the content rules: %s", feedback)
					}
				}
				return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "submission", Name: "submit_review", Input: rcTestAnswer(t, a)}}}, nil
			}}
			got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
			if err != nil || got == nil {
				t.Fatalf("a protocol slip withheld the review: %+v %v", got, err)
			}
			if calls != 2 {
				t.Fatalf("expected exactly one correction, got %d calls", calls)
			}
		})
	}
}

// Fail-closed covers the RETURN for both families; this pins the CAUSE for the
// protocol one. When a protocol rejection's correction also fails, the error
// that propagates must still be the protocol error — not a decode error and
// not a generic "challenge rejected". The specific cause is what the job log
// shows and what a diagnosis starts from, and a refactor that collapsed the
// two families into one message would pass every other test in this file
// while silently losing it.
func TestReviewProtocolRejectionFailsClosedWithItsOwnCause(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(c context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls > 2 {
			t.Fatal("unbounded retry")
		}
		a := rcCDChecks()
		a.Checks[0].Reason = "" // a protocol break, not a decode failure
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "submission", Name: "submit_review", Input: rcTestAnswer(t, a)}}}, nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err == nil || got != nil {
		t.Fatalf("an unchecked draft was published: %+v %v", got, err)
	}
	if !strings.Contains(err.Error(), "omitted reasoning") {
		t.Fatalf("the protocol cause did not survive the failed correction: %v", err)
	}
	if strings.Contains(err.Error(), "invalid challenge JSON") {
		t.Fatalf("a protocol rejection was reported as a decode failure: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected exactly one correction attempt, got %d calls", calls)
	}
}
