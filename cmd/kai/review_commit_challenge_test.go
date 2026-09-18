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
const rcFalseCDRemedy = "wrap every line in braces so the cd applies to all of them"
const rcEscapeFinding = "A workspace path containing $ is expanded by the shell inside double quotes."
const rcEscapeRemedy = "single-quote the workspace path"

var rcCDIssues = []string{rcFalseCDIssue, rcEscapeIssue}
var rcCDSources = []string{rcCDSource, `cd "$HOME"`}

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
		Scope:       []string{"the two supplied sources"},
		IntentMatch: "partial",
		MergeReady:  3,
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "refuted", Reason: "A successful cd changes shell state for both lines.", Remedy: rcFalseCDRemedy, Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", Reason: "Double quotes still allow parameter expansion.", Finding: rcEscapeFinding, Remedy: rcEscapeRemedy, Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},
		},
	}
}

// This tests publication mechanics, not the model's shell knowledge. The live
// evaluation below separately exercises the actual model on the #418 specimen.
func TestReviewChallengeDropsRefutedIssueAndKeepsSupportedIssue(t *testing.T) {
	res, err := rcValidateChallenge(rcTestAnswer(t, rcCDChecks()), rcCDIssues, nil, rcCDSources)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Review; strings.Contains(got, rcFalseCDIssue) || !strings.Contains(got, rcEscapeIssue) {
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
		{"line range past the end", func(a *rcChallengeAnswer) { a.Checks[0].Evidence[0].LineEnd = 99 }},
		{"line range starting at zero", func(a *rcChallengeAnswer) { a.Checks[0].Evidence[0].LineStart = 0 }},
		{"reversed line range", func(a *rcChallengeAnswer) {
			a.Checks[0].Evidence[0] = rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 1}
		}},
		{"invented source", func(a *rcChallengeAnswer) { a.Checks[0].Evidence[0].Source = 99 }},
		{"no evidence", func(a *rcChallengeAnswer) { a.Checks[0].Evidence = nil }},
		{"check for an issue the draft never raised", func(a *rcChallengeAnswer) {
			a.Checks = append(a.Checks, rcIssueCheck{Issue: "new.go:1 — new allegation", Verdict: "supported", Reason: "r", Finding: "f", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}})
		}},
		{"check without reasoning", func(a *rcChallengeAnswer) { a.Checks[0].Reason = " " }},
		{"unknown verdict", func(a *rcChallengeAnswer) { a.Checks[0].Verdict = "probably" }},
		{"unknown intent verdict", func(a *rcChallengeAnswer) { a.IntentMatch = "" }},
		{"invalid readiness", func(a *rcChallengeAnswer) { a.MergeReady = 9 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			tc.mutate(&a)
			got, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
			if err == nil || got != nil {
				t.Fatalf("unchecked review escaped: %+v, %v", got, err)
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
		// Sources reach the challenger rendered with numbered lines, so the
		// evidence check is that the LAST line of the file is present as its
		// own numbered line — the 2,000-character cut this guards against would
		// have dropped it.
		text := ""
		if len(req.Messages) == 1 {
			text = req.Messages[0].Parts[0].(message.TextContent).Text
		}
		// Source 2 = the tool-call header line + 500 preamble lines + the last line.
		if !strings.Contains(text, "SOURCE 2 (502 lines):") || !strings.Contains(text, "  502| critical source at the end") {
			t.Fatal("challenge did not get full evidence in a fresh conversation")
		}
		return provider.Response{}, errors.New("provider failed")
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), sources, nil)
	if err == nil || got != nil {
		t.Fatalf("failed challenge returned draft: %+v %v", got, err)
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
	got, _, err := rcRunFastReview(context.Background(), p, "test", "test", "", "", "test", "", rcCDSource, nil)
	if err == nil || got != "" || calls != 2 {
		t.Fatalf("unchecked fast draft escaped: calls=%d result=%q err=%v", calls, got, err)
	}
}

// Configured challenger routing. The fast pass may substitute a non-reasoning
// model for the DRAFT; the publication challenge must be sent to the
// configured review model. Before this change the substitute was handed to the
// challenge too, so every fast-path challenge ran on the draft substitute.
func TestFastDraftDoesNotSubstituteChallenger(t *testing.T) {
	var requested []string
	// The fast pass has ONE source, its own prompt; both checks cite its first
	// line, so the answer validates and only routing is under test.
	answer := rcCDChecks()
	for i := range answer.Checks {
		answer.Checks[i].Evidence = []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 1}}
	}
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		requested = append(requested, req.Model)
		if len(requested) == 1 { // the draft
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview(rcFalseCDIssue, rcEscapeIssue)}}}, nil
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, answer)}}}, nil
	}}
	got, _, err := rcRunFastReview(context.Background(), p, "fast-draft-substitute", "configured-review-model", "", "", "test", "", rcCDSource, nil)
	if err != nil || !strings.Contains(got, rcEscapeIssue) {
		t.Fatalf("fast review did not publish the checked draft: %q %v", got, err)
	}
	if len(requested) != 2 || requested[0] != "fast-draft-substitute" {
		t.Fatalf("draft was not requested from the fast model: %v", requested)
	}
	for _, m := range requested[1:] {
		if m != "configured-review-model" {
			t.Fatalf("a challenge request was sent to the draft substitute instead of the review model: %v", requested)
		}
	}
}

// System-extracted citations. The model names a source and a line range; the
// prompt shows every source with one-based numbered lines; the system copies
// exactly those lines. No quotation is requested, sent, or compared. This
// removes the requirement that the model reproduce an excerpt byte-for-byte;
// it does not check that the extracted lines support the claim.
func TestReviewCitationIsExtractedBySystem(t *testing.T) {
	sources := []string{rcCDSource, `cd "$HOME"`}
	// Rendering and extraction share one coordinate system.
	if got := rcNumberedSource(1, rcCDSource); got != "SOURCE 1 (2 lines):\n    1| cd /tmp && pwd\n    2| pwd\n" {
		t.Fatalf("numbered source: %q", got)
	}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 2}); !ok || got != "pwd" {
		t.Fatalf("extract: %q %v", got, ok)
	}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 2}); !ok || got != "cd /tmp && pwd\npwd" {
		t.Fatalf("extract range: %q %v", got, ok)
	}
	// The prompt the challenger receives carries the numbered sources, and the
	// submission schema asks for locations, not quotes.
	var prompt string
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		prompt = req.Messages[0].Parts[0].(message.TextContent).Text
		if strings.Contains(rcTestAnswer(t, rcChallengeAnswer{}), "quote") {
			t.Fatal("answer shape still carries a quote field")
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), sources, nil)
	if err != nil || !strings.Contains(res.Review, rcEscapeIssue) {
		t.Fatalf("location citations rejected: %+v %v", res, err)
	}
	for _, want := range []string{"SOURCE 1 (2 lines):\n    1| cd /tmp && pwd\n    2| pwd\n", "SOURCE 2 (1 line):\n    1| cd \"$HOME\"\n"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks numbered source %q:\n%s", want, prompt)
		}
	}
	schema := rcSubmitReviewToolInfo()
	if b, _ := json.Marshal(schema.Parameters); strings.Contains(string(b), `"quote"`) || !strings.Contains(string(b), `"line_start"`) {
		t.Fatalf("schema still asks for quotes: %s", b)
	}
	if strings.Contains(rcChallengeSystem, "verbatim excerpt") || !strings.Contains(rcChallengeSystem, "BY LOCATION") {
		t.Fatal("prompt still asks the model to copy excerpts")
	}
}

func TestReviewChallengeRejectsTruncatedAnswerAndUnexpectedTool(t *testing.T) {
	for _, resp := range []provider.Response{
		{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{}`}}},
		{Parts: []message.ContentPart{message.ToolCall{ID: "bad", Name: "bash", Input: `{"command":"pwd"}`}}},
	} {
		p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) { return resp, nil }}
		if got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil); err == nil || got != nil {
			t.Fatalf("accepted invalid challenge: %+v %v", got, err)
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
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || strings.Contains(res.Review, rcFalseCDIssue) || !strings.Contains(res.Review, rcEscapeIssue) {
		t.Fatalf("bad structured submission: %+v %v", res, err)
	}
}

func TestReviewChallengeSkipsDraftWithoutIssues(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("a draft without allegations does not need this pass")
		return provider.Response{}, nil
	}}
	if got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(), nil, nil); err != nil || got.Review != rcTestReview() || got.Incomplete {
		t.Fatalf("changed issue-free draft: %+v %v", got, err)
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
	res, err := rcChallengeReview(context.Background(), prov, model, rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{source}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s\n%s", model, res.Review)
	_, issues, _, _, _, _ := rcParseReviewOutput(res.Review)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("expected only the genuine escaping defect: %v", issues)
	}
}
