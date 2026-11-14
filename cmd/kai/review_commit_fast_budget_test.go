package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// kaicontext/kai-vcs#1 (2026-10-15): the draft model thought through the
// whole 2000-token budget and returned no text, or a draft cut off mid-word.
// The draft call has room for default thinking, and a draft stopped by the
// token limit is an error naming the cause — never published, never "no
// content", and never sent on to the fact-check.
func TestFastDraftCutOffByTheTokenLimitFails(t *testing.T) {
	calls := 0
	var draftReq provider.Request
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		calls++
		draftReq = req
		return provider.Response{
			Parts:        []message.ContentPart{message.ReasoningContent{Thinking: "long thinking"}, message.TextContent{Text: "The diff was tru"}},
			FinishReason: message.FinishReasonMaxTokens,
			OutputTokens: rcFastMaxTokens,
		}, nil
	}}
	got, res, err := rcRunFastReview(context.Background(), p, "m", "c", "", "", "subject", "", "diff --git a/x b/x", nil)
	if err == nil || !strings.Contains(err.Error(), "cut off at the token limit") || got != "" || res != nil {
		t.Fatalf("truncated draft: got %q, %v, %v", got, res, err)
	}
	if calls != 1 {
		t.Errorf("a truncated draft went on to the fact-check (%d calls)", calls)
	}
	if draftReq.MaxTokens < 8000 {
		t.Errorf("draft budget %d leaves no room for default thinking", draftReq.MaxTokens)
	}
}
