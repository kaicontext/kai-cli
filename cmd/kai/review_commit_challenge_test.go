package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
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
var rcCDSources = []rcSource{rcRowSource(rcCDSource), rcRowSource(`cd "$HOME"`)}

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
	res, _, err := rcValidateChallenge(rcTestAnswer(t, rcCDChecks()), rcCDIssues, nil, rcCDSources)
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
			got, _, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources)
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
	var joined strings.Builder
	for _, src := range sources {
		joined.WriteString(src.Text)
	}
	// This kai_view result carries no "N: " file rows, so its file mapping
	// cannot be established: it is UNMAPPED — shown in full for context, not
	// citable — never silently re-addressed by rows.
	if len(sources) != 2 || !strings.Contains(sources[1].Text, file) || sources[1].Coord != rcCoordNone || strings.Contains(joined.String(), "unsupported model assertion") {
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
		if !strings.Contains(text, "SOURCE 2 (kai_view result whose file line mapping could not be established") || !strings.Contains(text, "\ncritical source at the end") {
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
	if _, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []rcSource{rcRowSource(strings.Repeat("x", rcEvidenceLimit))}, nil); err == nil {
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

// System-extracted citations. The model names a source and a range; the system
// copies exactly those lines. No quotation is requested, sent, or compared.
// Each source is shown in ONE coordinate system, declared in its header: rows
// for the prompt, diffs, grep output and experiments; the file's own line
// numbers for a kai_view result, which is shown verbatim.
func TestReviewCitationIsExtractedBySystem(t *testing.T) {
	sources := rcCDSources
	if got := rcRenderSource(1, sources[0]); got != "SOURCE 1 (2 rows; cite the ROW numbers printed at the left):\n    1| cd /tmp && pwd\n    2| pwd\n" {
		t.Fatalf("row source: %q", got)
	}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 1, LineStart: 2, LineEnd: 2}); !ok || got != "pwd" {
		t.Fatalf("extract: %q %v", got, ok)
	}
	if got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 1, LineStart: 1, LineEnd: 2}); !ok || got != "cd /tmp && pwd\npwd" {
		t.Fatalf("extract range: %q %v", got, ok)
	}
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
	for _, want := range []string{"SOURCE 1 (2 rows; cite the ROW numbers printed at the left):\n    1| cd /tmp && pwd\n    2| pwd\n", "SOURCE 2 (1 row; cite the ROW numbers printed at the left):\n    1| cd \"$HOME\"\n"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks numbered source %q:\n%s", want, prompt)
		}
	}
	for _, ref := range res.Allegations[1].Evidence {
		if ref.Coord != rcCoordRows {
			t.Fatalf("row citation recorded as %q", ref.Coord)
		}
	}
	schema := rcSubmitReviewToolInfo()
	if b, _ := json.Marshal(schema.Parameters); strings.Contains(string(b), `"quote"`) || !strings.Contains(string(b), `"line_start"`) {
		t.Fatalf("schema still asks for quotes: %s", b)
	}
	for _, shell := range []bool{false, true} {
		if system := rcChallengeSystemPrompt(shell); strings.Contains(system, "verbatim excerpt") || !strings.Contains(system, "BY LOCATION") {
			t.Fatal("prompt still asks the model to copy excerpts")
		}
	}
}

// A challenger that is ALWAYS truncated, or keeps calling a tool it was not
// offered, still fails closed — after its bounded retries, not on the first try.
func TestReviewChallengeRejectsTruncatedAnswerAndUnexpectedTool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resp      provider.Response
		wantCalls int
		wantErr   string
	}{
		// The first answer and exactly one retry, then fail closed.
		{"always truncated", provider.Response{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{}`}}}, 1 + rcMaxTruncations, "truncated"},
		// Each refused call is answered; the one past the limit fails closed.
		{"always an unoffered tool", provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "bad", Name: "bash", Input: `{"command":"pwd"}`}}}, rcMaxRefusedCalls + 1, `unavailable tool "bash"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) { calls++; return tc.resp, nil }}
			got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []rcSource{rcRowSource(rcCDSource)}, nil)
			if err == nil || got != nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("accepted invalid challenge: %+v %v", got, err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("made %d calls, want exactly %d", calls, tc.wantCalls)
			}
		})
	}
}

// Every kind of retry used up in one challenge — the refused calls and the
// truncation — still leaves the turn for the final answer.
func TestReviewChallengeEveryRetryThenSubmits(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		switch {
		case calls <= rcMaxRefusedCalls:
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "sh" + strconv.Itoa(calls), Name: "review_shell", Input: `{"script":"true"}`}}}, nil
		case calls <= rcMaxRefusedCalls+rcMaxTruncations:
			return provider.Response{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{"scope":`}}}, nil
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || calls != rcMaxRefusedCalls+rcMaxTruncations+1 || !strings.Contains(res.Review, rcEscapeIssue) {
		t.Fatalf("final answer lost after every retry: calls=%d %+v %v", calls, res, err)
	}
	if max := rcMaxExperiments + rcMaxRefusedCalls + rcMaxTruncations + 1; rcChallengeMaxTurns <= max {
		t.Fatalf("turn backstop %d leaves no slack over the worst case %d", rcChallengeMaxTurns, max)
	}
}

func TestReviewChallengeAppendToEmptyTurn(t *testing.T) {
	got := rcAppendToLastTurn(nil, message.TextContent{Text: "note"})
	if len(got) != 1 || got[0].Role != message.RoleUser || len(got[0].Parts) != 1 {
		t.Fatalf("empty conversation not started as a user turn: %+v", got)
	}
}

// Without a sandbox (the default, and CI), the prompt must not invite a
// review_shell call — and if the model makes one anyway, it gets an error
// result and the review still finishes. This used to abort every challenge
// with `challenge requested unavailable tool "review_shell"`.
func TestReviewChallengeAnswersUnofferedToolAndFinishes(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if strings.Contains(req.System, "review_shell") {
			t.Fatal("prompt mentions review_shell although no sandbox is configured")
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "submit_review" {
			t.Fatalf("offered tools: %v", req.Tools)
		}
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "sh", Name: "review_shell", Input: `{"script":"cd /tmp && pwd"}`}}}, nil
		}
		last := req.Messages[len(req.Messages)-1]
		tr, ok := last.Parts[0].(message.ToolResult)
		if !ok || tr.ToolCallID != "sh" || !tr.IsError || !strings.Contains(tr.Content, "review_shell is not available") {
			t.Fatalf("unoffered call was not answered with an error result: %+v", last)
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || calls != 2 || !strings.Contains(res.Review, rcEscapeIssue) || strings.Contains(res.Review, rcFalseCDIssue) {
		t.Fatalf("review did not finish after an unoffered tool call: calls=%d %+v %v", calls, res, err)
	}
}

// The prompt names review_shell exactly when it is offered.
func TestReviewChallengePromptMatchesSandbox(t *testing.T) {
	if strings.Contains(rcChallengeSystemPrompt(false), "review_shell") {
		t.Fatal("no-sandbox prompt mentions review_shell")
	}
	if !strings.Contains(rcChallengeSystemPrompt(true), "review_shell") {
		t.Fatal("sandbox prompt does not mention review_shell")
	}
}

// An answer cut off at the output limit gets one concise retry instead of
// failing the review. The cut-off reply (here a half-written submit_review) is
// not replayed; the retry is told why it is being asked again.
func TestReviewChallengeRecoversFromTruncatedAnswer(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if req.MaxTokens < rcChallengeMaxTokens {
			t.Fatalf("challenge limit %d is below %d", req.MaxTokens, rcChallengeMaxTokens)
		}
		if calls == 1 {
			return provider.Response{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.ToolCall{ID: "cut", Name: "submit_review", Input: `{"scope":["the two`}}}, nil
		}
		if len(req.Messages) != 1 {
			t.Fatalf("truncated reply was replayed: %d messages", len(req.Messages))
		}
		parts := req.Messages[0].Parts
		if note, ok := parts[len(parts)-1].(message.TextContent); !ok || note.Text != rcTruncationNote {
			t.Fatalf("retry does not say the answer was cut off: %+v", parts)
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || calls != 2 || !strings.Contains(res.Review, rcEscapeIssue) {
		t.Fatalf("review did not finish after a truncated answer: calls=%d %+v %v", calls, res, err)
	}
}

// A truncation retry whose answer then needs a citation correction: the note
// stays in the history (the model is told why it was asked again), the one
// correction round still runs, and the corrected review publishes.
func TestReviewChallengeTruncationThenCitationCorrection(t *testing.T) {
	bad := rcCDChecks()
	bad.Checks[0].Evidence[0] = rcCheckEvidence{Source: 9, LineStart: 1, LineEnd: 1}
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		switch calls {
		case 1:
			return provider.Response{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{"scope":`}}}, nil
		case 2:
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, bad)}}}, nil
		}
		// The correction turn: prompt (with the truncation note), the
		// submission, and the citation feedback as its tool result.
		if len(req.Messages) != 3 {
			t.Fatalf("correction turn has %d messages, want 3", len(req.Messages))
		}
		first := req.Messages[0].Parts
		if note, ok := first[len(first)-1].(message.TextContent); !ok || note.Text != rcTruncationNote {
			t.Fatalf("truncation note missing from the correction history: %+v", first)
		}
		tr, ok := req.Messages[2].Parts[0].(message.ToolResult)
		if !ok || tr.ToolCallID != "s" || !strings.Contains(tr.Content, "challenge citation invalid") {
			t.Fatalf("correction feedback missing: %+v", req.Messages[2])
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s2", Name: "submit_review", Input: rcTestAnswer(t, rcCDChecks())}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue, rcEscapeIssue), rcCDSources, nil)
	if err != nil || calls != 3 || res.Incomplete || !strings.Contains(res.Review, rcEscapeIssue) || strings.Contains(res.Review, rcFalseCDIssue) {
		t.Fatalf("review did not finish after truncation and correction: calls=%d %+v %v", calls, res, err)
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
	res, err := rcChallengeReview(context.Background(), prov, model, rcTestReview(rcFalseCDIssue, rcEscapeIssue), []rcSource{rcRowSource(source)}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s\n%s", model, res.Review)
	_, issues, _, _, _, _ := rcParseReviewOutput(res.Review)
	if len(issues) != 1 || issues[0] != rcEscapeIssue {
		t.Fatalf("expected only the genuine escaping defect: %v", issues)
	}
}
