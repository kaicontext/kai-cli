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

// rcTestReview builds a DRAFT (the challenge's input) with the given ISSUES.
func rcTestReview(issues ...string) string { return rcTestReviewWith(issues, nil) }

// rcTestReviewWith builds a draft carrying both ISSUES and DECISIONS.
func rcTestReviewWith(issues, decisions []string) string {
	readiness := "5"
	match := "verified"
	if len(issues) > 0 {
		readiness, match = "3", "partial"
	} else if len(decisions) > 0 {
		readiness = "4"
	}
	s := "Review within the supplied scope.\n" + rcReviewDataMarker + "\nINTENT_MATCH: " + match +
		"\nMERGE_READY: " + readiness + "\nSUMMARY: Findings checked.\n"
	if len(decisions) > 0 {
		s += "DECISIONS:\n" + rcTestBullets(decisions)
	}
	return s + "ISSUES:\n" + rcTestBullets(issues)
}

func rcTestBullets(items []string) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString("- " + item + "\n")
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

func rcBool(b bool) *bool { return &b }

func rcCDChecks() rcChallengeAnswer {
	return rcChallengeAnswer{
		Scope:       []string{"frontend/dist/panel-terminal.js command construction"},
		IntentMatch: "partial",
		MergeReady:  3,
		Checks: []rcIssueCheck{
			{Issue: rcFalseCDIssue, Verdict: "refuted", RequiresRuntime: rcBool(false), Reason: "A successful cd changes shell state for both lines.", Evidence: []rcCheckEvidence{{Source: 1, LineStart: 1, LineEnd: 2}}},
			{Issue: rcEscapeIssue, Verdict: "supported", RequiresRuntime: rcBool(false), Reason: "Double quotes still allow parameter expansion.", Finding: "The path is interpolated inside double quotes, so $ expands.", Remedy: "Escape the path before interpolating it into the double-quoted cd.", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}},
		},
	}
}

var rcCDIssues = []string{rcFalseCDIssue, rcEscapeIssue}
var rcCDSources = []string{rcCDSource, `cd "$HOME"`}

// This tests publication mechanics, not the model's shell knowledge. The live
// evaluations below separately exercise the actual model on the #418 and #429
// specimens.
func TestReviewChallengeDropsRefutedIssueAndKeepsSupportedIssue(t *testing.T) {
	res, err := rcValidateChallenge(rcTestAnswer(t, rcCDChecks()), rcCDIssues, nil, rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Review, rcFalseCDIssue) || !strings.Contains(res.Review, rcEscapeIssue) {
		t.Fatalf("wrong published allegations: %s", res.Review)
	}
	if res.Incomplete || len(res.Unresolved) != 0 || strings.Contains(res.Review, "This review is incomplete") {
		t.Fatalf("a fully resolved review was marked incomplete: %+v", res)
	}
	// The structured record carries the same verdicts, by id.
	if len(res.Allegations) != 2 || res.Allegations[0].Status != "refuted" || res.Allegations[1].Status != "supported" || res.Allegations[1].ID != 2 {
		t.Fatalf("structured record disagrees with the published review: %+v", res.Allegations)
	}
	if res.Allegations[1].Remedy == "" || !strings.Contains(res.Review, "**Remedy:** Escape the path") {
		t.Fatalf("supported finding's remedy not published as actionable: %+v", res.Allegations[1])
	}
}

func TestReviewChallengeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rcChallengeAnswer)
	}{
		{"missing check", func(a *rcChallengeAnswer) { a.Checks = a.Checks[:1] }},
		{"duplicate check", func(a *rcChallengeAnswer) { a.Checks[1] = a.Checks[0] }},
		{"unknown issue", func(a *rcChallengeAnswer) { a.Checks[0].Issue = "phantom.go:1 — never alleged" }},
		{"empty reason", func(a *rcChallengeAnswer) { a.Checks[0].Reason = "" }},
		{"unknown verdict", func(a *rcChallengeAnswer) { a.Checks[0].Verdict = "maybe" }},
		{"missing runtime classification", func(a *rcChallengeAnswer) { a.Checks[0].RequiresRuntime = nil }},
		{"supported without finding", func(a *rcChallengeAnswer) { a.Checks[1].Finding = "" }},
		{"empty scope", func(a *rcChallengeAnswer) { a.Scope = nil }},
		{"blank-only scope", func(a *rcChallengeAnswer) { a.Scope = []string{"  "} }},
		{"invalid intent", func(a *rcChallengeAnswer) { a.IntentMatch = "maybe" }},
		{"invalid merge_ready", func(a *rcChallengeAnswer) { a.MergeReady = 9 }},
		{"contradictory readiness", func(a *rcChallengeAnswer) { a.MergeReady = 5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := rcCDChecks()
			tc.mutate(&a)
			res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, nil, rcCDSources, nil)
			if err == nil || res != nil {
				t.Fatalf("unchecked review escaped: %+v, %v", res, err)
			}
		})
	}
	if res, err := rcValidateChallenge("not valid json", rcCDIssues, nil, rcCDSources, nil); err == nil || res != nil {
		t.Fatalf("accepted malformed challenge JSON: %+v %v", res, err)
	}
	// A draft decision left unassessed is a structural failure, like an
	// unchecked allegation.
	a := rcCDChecks()
	if res, err := rcValidateChallenge(rcTestAnswer(t, a), rcCDIssues, []string{"Keep the getter public."}, rcCDSources, nil); err == nil || res != nil {
		t.Fatalf("accepted a challenge that skipped a draft decision: %+v %v", res, err)
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
		// The source is presented with numbered lines, so the raw blob no longer
		// appears verbatim; the end of a large file must still reach the challenge.
		if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Parts[0].(message.TextContent).Text, "critical source at the end") {
			t.Fatal("challenge did not get full evidence in a fresh conversation")
		}
		return provider.Response{}, errors.New("provider failed")
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), sources, nil)
	if err == nil || res != nil {
		t.Fatalf("failed challenge returned draft: %+v %v", res, err)
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
	got, res, err := rcRunFastReview(context.Background(), p, "test", "", "", "test", "", rcCDSource, nil)
	if err == nil || got != "" || res != nil || calls != 2 {
		t.Fatalf("unchecked fast draft escaped: calls=%d result=%q err=%v", calls, got, err)
	}
}

// The fast path must surface the structured result to its caller so the bundle
// is marked incomplete and the run exits non-zero — a published-but-partial fast
// review must not read as a completed one.
func TestFastReviewReportsUnresolved(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestReview(rcFalseCDIssue)}}}, nil
		}
		a := rcChallengeAnswer{
			Scope:       []string{"the diff"},
			IntentMatch: "partial",
			MergeReady:  4,
			Checks:      []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "needs a shell"}},
		}
		return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: rcTestAnswer(t, a)}}}, nil
	}}
	got, res, err := rcRunFastReview(context.Background(), p, "test", "", "", "test", "", rcCDSource, nil)
	if err != nil {
		t.Fatalf("fast review withheld a publishable-but-incomplete result: %v", err)
	}
	if res == nil || !res.Incomplete || len(res.Unresolved) != 1 || res.Unresolved[0] != rcFalseCDIssue {
		t.Fatalf("fast review did not report the unresolved allegation: %+v", res)
	}
	if !strings.Contains(got, "This review is incomplete") {
		t.Fatalf("fast review body did not mark itself incomplete: %s", got)
	}
}

// Live eval caught this: with no sandbox, the model asks for review_shell
// anyway, and treating that as fatal withheld every finding — supported ones
// included. It must be answered with an error tool result and the challenge
// must continue; the model then submits with the runtime claim unverified.
func TestReviewChallengeUnavailableSandboxIsNotFatal(t *testing.T) {
	calls := 0
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		calls++
		if calls == 1 {
			if !strings.Contains(req.Messages[0].Parts[0].(message.TextContent).Text, "no review_shell sandbox is available") {
				t.Fatal("initial message did not tell the model no sandbox is available")
			}
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "exp", Name: "review_shell", Input: `{"script":"pwd"}`}}}, nil
		}
		// The model must receive an ERROR tool result, not a fatal end.
		last := req.Messages[len(req.Messages)-1].Parts[0].(message.ToolResult)
		if !last.IsError || last.ToolCallID != "exp" || !strings.Contains(last.Content, "not available") {
			t.Fatalf("model was not told the experiment is unavailable: %+v", last)
		}
		a := rcChallengeAnswer{
			Scope:       []string{"the cd behavior"},
			IntentMatch: "partial",
			MergeReady:  4,
			Checks:      []rcIssueCheck{{Issue: rcFalseCDIssue, Verdict: "unverified", RequiresRuntime: rcBool(true), Reason: "no sandbox to run it"}},
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "final", Name: "submit_review", Input: rcTestAnswer(t, a)}}}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil)
	if err != nil {
		t.Fatalf("unavailable sandbox withheld the review: %v", err)
	}
	if calls != 2 || !res.Incomplete || res.Allegations[0].Status != "unresolved" {
		t.Fatalf("challenge did not continue to an unresolved, incomplete result: calls=%d %+v", calls, res)
	}
	// A genuinely unknown tool, and exceeding the cap, are still fatal.
	p2 := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "bad", Name: "bash", Input: `{}`}}}, nil
	}}
	if res, err := rcChallengeReview(context.Background(), p2, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil); err == nil || res != nil {
		t.Fatalf("unknown tool accepted: %+v %v", res, err)
	}
}

func TestReviewChallengeRejectsTruncatedAnswerAndUnexpectedTool(t *testing.T) {
	for _, resp := range []provider.Response{
		{FinishReason: message.FinishReasonMaxTokens, Parts: []message.ContentPart{message.TextContent{Text: `{}`}}},
		{Parts: []message.ContentPart{message.ToolCall{ID: "bad", Name: "bash", Input: `{"command":"pwd"}`}}},
	} {
		p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) { return resp, nil }}
		if res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), []string{rcCDSource}, nil); err == nil || res != nil {
			t.Fatalf("accepted invalid challenge: %+v %v", res, err)
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

func TestReviewChallengeSkipsDraftWithoutIssuesOrDecisions(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		t.Fatal("a draft without allegations or decisions does not need this pass")
		return provider.Response{}, nil
	}}
	res, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(), nil, nil)
	if err != nil || res == nil || res.Review != rcTestReview() || !res.rcEmpty() {
		t.Fatalf("changed issue-free draft: %+v %v", res, err)
	}
}

// ---- Live evaluations (opt-in; use the configured Kai provider, incur usage) ----
//
// Unit tests above cannot establish that an LLM knows shell semantics. These
// run the real model on the two observed failures. Each runs in two
// configurations: with the sandbox (runtime claims can be settled) and without
// (runtime claims must be left unresolved and the review must be incomplete).
// A mocked answer is never presented as evidence of model quality.

func rcLiveEvalSetup(t *testing.T) (provider.Provider, string) {
	t.Helper()
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
	return prov, model
}

// rcLiveSandbox returns the configured sandbox, or nil for the without-sandbox
// configuration. The with-sandbox configuration skips when none is configured
// rather than pretending.
func rcLiveSandbox(t *testing.T, want bool) *rcShellSandbox {
	t.Helper()
	if !want {
		return nil
	}
	sb := rcConfiguredSandbox()
	if sb == nil {
		t.Skip("with-sandbox configuration requires KAI_REVIEW_SANDBOX_IMAGE")
	}
	return sb
}

const rcLive418Source = `// frontend/dist/panel-terminal.js: the PTY uses a persistent shell.
// Supports runnable multiline shell fences.
const ws = this.ctx.workspace;
const full = ws ? 'cd "' + ws + '" && ' + String(command) : String(command);
const input = full.replace(/\r?\n/g, "\r") + "\r";
// Sample workspace path: a literal /tmp/$HOME directory, not a variable.
`

// #418: the false multiline-cd allegation must be refuted and the real escaping
// defect retained — but only with an experiment. Without a sandbox both are
// runtime claims and must come back unresolved, with the review incomplete.
func TestReviewChallengeLiveDesktop418(t *testing.T) {
	for _, withSandbox := range []bool{true, false} {
		name := map[bool]string{true: "with-sandbox", false: "without-sandbox"}[withSandbox]
		t.Run(name, func(t *testing.T) {
			prov, model := rcLiveEvalSetup(t)
			sandbox := rcLiveSandbox(t, withSandbox)
			res, err := rcChallengeReview(context.Background(), prov, model, rcTestReview(rcFalseCDIssue, rcEscapeIssue), []string{rcLive418Source}, sandbox)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("model=%s\n%s\nrecord=%+v", model, res.Review, res.Allegations)
			_, issues, _, _, _, _ := rcParseReviewOutput(res.Review)
			if withSandbox {
				if len(issues) != 1 || issues[0] != rcEscapeIssue {
					t.Fatalf("expected only the genuine escaping defect: %v", issues)
				}
				if res.Allegations[0].Status != "refuted" {
					t.Fatalf("false cd allegation not refuted: %+v", res.Allegations[0])
				}
				return
			}
			if len(issues) != 0 || !res.Incomplete {
				t.Fatalf("without a sandbox runtime claims must be unresolved and the review incomplete: issues=%v record=%+v", issues, res)
			}
			for _, a := range res.Allegations {
				if a.Status != "unresolved" || a.Remedy != "" {
					t.Fatalf("runtime claim settled or remedied without an experiment: %+v", a)
				}
			}
		})
	}
}

const rcLive429Source = `// frontend/dist/app.js: play-button (data-run-command) click handler.
const command = commandFromFence(code.textContent);
// Prefix a cd into the workspace so the command always runs in the
// correct directory. JSON.stringify quotes the path "safely".
const wsPath = (window.Panels && typeof window.Panels.workspace === "function") ? window.Panels.workspace() : "";
const full = wsPath ? 'cd ' + JSON.stringify(wsPath) + ' && ' + command : command;
Panels.setOpen(true);
Panels.select("terminal", { command: full });
// The terminal sends the string to a POSIX sh. JSON.stringify emits a
// double-quoted JS string literal; it escapes \ and " but not $ or backtick.
`

const rcStringifyIssue = `frontend/dist/app.js:6 — JSON.stringify does not shell-escape $ or backticks, so a workspace path containing them is expanded by the shell and the cd targets the wrong directory`

// #429: the JSON.stringify path-handling defect is real and must be supported —
// but only with an experiment showing the expansion. Without a sandbox it is a
// runtime claim and must be left unresolved, with its remedy withheld.
func TestReviewChallengeLiveDesktop429(t *testing.T) {
	for _, withSandbox := range []bool{true, false} {
		name := map[bool]string{true: "with-sandbox", false: "without-sandbox"}[withSandbox]
		t.Run(name, func(t *testing.T) {
			prov, model := rcLiveEvalSetup(t)
			sandbox := rcLiveSandbox(t, withSandbox)
			res, err := rcChallengeReview(context.Background(), prov, model, rcTestReview(rcStringifyIssue), []string{rcLive429Source}, sandbox)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("model=%s\n%s\nrecord=%+v", model, res.Review, res.Allegations)
			a := res.Allegations[0]
			if withSandbox {
				if a.Status != "supported" {
					t.Fatalf("real JSON.stringify defect not supported with an experiment available: %+v", a)
				}
				return
			}
			if a.Status != "unresolved" || a.Remedy != "" || !res.Incomplete {
				t.Fatalf("without a sandbox the runtime claim must be unresolved, remedy withheld, review incomplete: %+v incomplete=%v", a, res.Incomplete)
			}
		})
	}
}
