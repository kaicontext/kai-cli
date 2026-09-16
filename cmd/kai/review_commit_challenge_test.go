package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const rcFalseCDIssue = `frontend/dist/panel-terminal.js:148 — later lines run outside the workspace after a successful cd`
const rcEscapeIssue = `frontend/dist/panel-terminal.js:148 — workspace expansion is possible inside double quotes`
const rcCDSource = "cd /tmp && pwd\npwd\n"

func rcTestReview(issues ...string) string {
	readiness := "5"
	match := "verified"
	if len(issues) > 0 {
		readiness, match = "3", "partial"
	}
	return "Review within the supplied scope.\n" + rcReviewDataMarker + "\nINTENT_MATCH: " + match +
		"\nMERGE_READY: " + readiness + "\nSUMMARY: Findings checked.\nISSUES:\n" + rcTestBullets(issues)
}

func rcTestBullets(issues []string) string {
	var b strings.Builder
	for _, issue := range issues {
		b.WriteString("- " + issue + "\n")
	}
	return b.String()
}

func rcTestAnswer(t *testing.T, answer rcChallengeAnswer) string {
	t.Helper()
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rcCDChecks() rcChallengeAnswer {
	return rcChallengeAnswer{
		Review: rcTestReview(rcEscapeIssue),
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "refuted", Reason: "A successful cd changes shell state for both lines.", Evidence: []rcCheckEvidence{{Source: 1, Quote: rcCDSource}}},
			{Issue: rcEscapeIssue, Verdict: "supported", Reason: "Double quotes still allow parameter expansion.", Evidence: []rcCheckEvidence{{Source: 2, Quote: `cd "$HOME"`}}},
		},
	}
}

// This tests publication mechanics, not the model's shell knowledge. The live
// evaluation below separately exercises the actual model on the #418 specimen.
func TestReviewChallengeDropsRefutedIssueAndKeepsSupportedIssue(t *testing.T) {
	got, err := rcValidateChallenge(rcTestAnswer(t, rcCDChecks()), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, rcFalseCDIssue) || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("wrong published allegations: %s", got)
	}
}

func TestReviewChallengeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rcChallengeAnswer)
	}{
		{"missing check", func(a *rcChallengeAnswer) { a.Checks = a.Checks[:1] }},
		{"duplicate check", func(a *rcChallengeAnswer) { a.Checks[1] = a.Checks[0] }},
		{"unverified is not refuted", func(a *rcChallengeAnswer) { a.Checks[0].Verdict = "unverified" }},
		{"invented quote", func(a *rcChallengeAnswer) { a.Checks[0].Evidence[0].Quote = "made up output" }},
		{"invented source", func(a *rcChallengeAnswer) { a.Checks[0].Evidence[0].Source = 99 }},
		{"no evidence", func(a *rcChallengeAnswer) { a.Checks[0].Evidence = nil }},
		{"unchecked new issue", func(a *rcChallengeAnswer) { a.Review = rcTestReview("new.go:1 — new allegation") }},
		{"rejected issue survives", func(a *rcChallengeAnswer) { a.Review = rcTestReview(rcFalseCDIssue, rcEscapeIssue) }},
		{"supported issue lost", func(a *rcChallengeAnswer) { a.Review = rcTestReview() }},
		{"contradictory readiness", func(a *rcChallengeAnswer) {
			a.Review = strings.Replace(a.Review, "MERGE_READY: 3", "MERGE_READY: 5", 1)
		}},
		{"missing verdict", func(a *rcChallengeAnswer) { a.Review = "unfinished\n" + rcReviewDataMarker }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			tc.mutate(&a)
			got, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, rcEscapeIssue}, []string{rcCDSource, `cd "$HOME"`})
			if err == nil || got != "" {
				t.Fatalf("unchecked review escaped: %q, %v", got, err)
			}
		})
	}
}

type rcChallengeProvider struct {
	send func(context.Context, provider.Request) (provider.Response, error)
}

func (p rcChallengeProvider) Send(ctx context.Context, req provider.Request) (provider.Response, error) {
	return p.send(ctx, req)
}

func TestReviewChallengeReceivesFullEvidenceAndFreshConversation(t *testing.T) {
	file := strings.Repeat("preamble\n", 500) + "critical source at the end"
	tr := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "original diff"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.ToolCall{ID: "read", Name: "kai_view", Input: `{"file_path":"a.go"}`}, message.TextContent{Text: "unsupported model assertion"}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "read", Content: file}}},
	}
	sources := rcChallengeSources(tr)
	if len(sources) != 2 || !strings.Contains(sources[1], file) || strings.Contains(strings.Join(sources, ""), "unsupported model assertion") {
		t.Fatalf("sources lost evidence or included speculation: %v", sources)
	}
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Parts[0].(message.TextContent).Text, file) {
			t.Fatal("challenge did not get full evidence in a fresh conversation")
		}
		return provider.Response{}, errors.New("provider failed")
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), sources, nil)
	if err == nil || got != "" {
		t.Fatalf("failed challenge returned draft: %q %v", got, err)
	}
}

func TestReviewConclusionPreservesFullToolResults(t *testing.T) {
	file := strings.Repeat("header\n", 500) + rcCDSource
	tr := []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "read", Content: file}}}}
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		if got := req.Messages[0].Parts[0].(message.ToolResult).Content; got != file {
			t.Fatal("conclusion lost the end of the source file")
		}
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview()}}}, nil
	}}
	if got := rcConcludeFromTranscript(context.Background(), p, "test", tr); got == "" {
		t.Fatal("missing conclusion")
	}
	if tr[0].Parts[0].(message.ToolResult).Content != file {
		t.Fatal("mutated original transcript")
	}
}

func TestReviewConclusionFailureDoesNotRetryWithMissingEvidence(t *testing.T) {
	tr := make([]message.Message, 50)
	for i := range tr {
		tr[i] = message.Message{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "source"}}}
	}
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		return provider.Response{}, errors.New("too much context")
	}}
	if got := rcConcludeFromTranscript(context.Background(), p, "test", tr); got != "" || calls != 1 {
		t.Fatalf("retried on a partial transcript: calls=%d, output=%s", calls, got)
	}
}

func TestReviewChallengeEvidenceLimitAndIncompleteReport(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("oversized evidence must not be silently shortened and sent")
		return provider.Response{}, nil
	}}
	if _, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{strings.Repeat("x", rcEvidenceLimit)}, nil); err == nil {
		t.Fatal("accepted oversized evidence")
	}
	prose := rcIncompleteProse(&rcIncomplete{ChallengeFailure: "unverified allegation"})
	if !strings.Contains(prose, "unchecked draft has been withheld") || !strings.Contains(prose, "not an approval") {
		t.Fatalf("misleading incomplete report: %s", prose)
	}
}

func TestFastReviewDoesNotPublishDraftWhenChallengeFails(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview(rcFalseCDIssue)}}}, nil
		}
		return provider.Response{}, errors.New("challenge unavailable")
	}}
	got, err := rcRunFastReview(context.Background(), p, "test", "", "", "test", "", rcCDSource, nil)
	if err == nil || got != "" || calls != 2 {
		t.Fatalf("unchecked fast draft escaped: calls=%d result=%q err=%v", calls, got, err)
	}
}

func TestReviewChallengeRejectsTruncatedAnswerAndUnexpectedTool(t *testing.T) {
	for _, resp := range []provider.Response{
		{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{}`}}},
		{Parts: []message.ContentPart{message.ToolCall{ID: "bad", Name: "bash", Input: `{"command":"pwd"}`}}},
	} {
		p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) { return resp, nil }}
		if got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil); err == nil || got != "" {
			t.Fatalf("accepted invalid challenge: %q %v", got, err)
		}
	}
}

func TestReviewChallengeAcceptsStructuredSubmissionWithCommentary(t *testing.T) {
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		if len(req.Tools) != 1 || req.Tools[0].Name != "submit_review" {
			t.Fatal("missing structured submission tool")
		}
		return provider.Response{Parts: []message.ContentPart{
			message.TextContent{Text: "Now I can submit the checked review."},
			message.ToolCall{ID: "final", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())},
		}}, nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{rcCDSource, `cd "$HOME"`}, nil)
	if err != nil || strings.Contains(got, rcFalseCDIssue) || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("bad structured submission: %q %v", got, err)
	}
}

func TestReviewChallengeSkipsDraftWithoutIssues(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("a draft without allegations does not need this pass")
		return provider.Response{}, nil
	}}
	if got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(), nil, nil); err != nil || got != rcTestReview() {
		t.Fatalf("changed issue-free draft: %q %v", got, err)
	}
}

// Opt-in model evaluation: uses the configured Kai provider and incurs usage.
// Unit tests above cannot establish that an LLM knows shell semantics.
func TestReviewChallengeLiveDesktop418(t *testing.T) {
	if os.Getenv("KAI_REVIEW_LIVE_EVAL") != "1" {
		t.Skip("set KAI_REVIEW_LIVE_EVAL=1 to evaluate the configured model")
	}
	if dir := os.Getenv("KAI_REVIEW_EVAL_CONFIG_DIR"); dir != "" {
		old := kaiDir
		kaiDir = dir
		t.Cleanup(func() { kaiDir = old })
	}
	prov, model, _ := rcReviewProvider()
	if prov == nil {
		t.Fatal("no configured Kai provider")
	}
	sandbox := rcConfiguredSandbox()
	if sandbox == nil {
		t.Fatal("live evaluation requires KAI_REVIEW_SANDBOX_IMAGE")
	}
	const source = `// frontend/dist/panel-terminal.js: the PTY uses a persistent shell.
// Supports runnable multiline shell fences.
const ws = this.ctx.workspace;
const full = ws ? 'cd "' + ws + '" && ' + String(command) : String(command);
const input = full.replace(/\r?\n/g, "\r") + "\r";
// Sample workspace path: a literal /tmp/$HOME directory, not a variable.
`
	got, err := rcChallengeReview(context.Background(), prov, model, rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{source}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s\n%s", model, got)
	_, issues, _, _, _, _ := rcParseReviewOutput(got)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("expected only the genuine escaping defect: %v", issues)
	}
}
