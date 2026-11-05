package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const (
	rcTestGLM  = "z-ai/glm-5.2"
	rcTestKimi = "moonshotai/kimi-k2.6"
)

// Failing over must stay at GLM's price point. Anything without a listed peer,
// the Claude models the engine would reach for included, gets none.
func TestFailoverPeerStaysAtSimilarCost(t *testing.T) {
	for model, want := range map[string]string{
		rcTestGLM:                rcTestKimi,
		rcTestKimi:               rcTestGLM,
		rcTestGLM + ":reasoning": rcTestKimi,
		"claude-opus-4-8":        "",
		"claude-sonnet-5-5":      "",
		"openai/gpt-5.5":         "",
		"":                       "",
	} {
		if got := rcFailoverPeer(model); got != want {
			t.Errorf("rcFailoverPeer(%q) = %q, want %q", model, got, want)
		}
	}
}

// rcScriptedRun plays one outcome per run and records the options each got.
type rcScriptedRun struct {
	outcomes []func() (*agent.Result, error)
	got      []agent.Options
}

func (r *rcScriptedRun) run(_ context.Context, o agent.Options) (*agent.Result, error) {
	r.got = append(r.got, o)
	if len(r.got) > len(r.outcomes) {
		return nil, errors.New("more runs than the test scripted")
	}
	return r.outcomes[len(r.got)-1]()
}

func (r *rcScriptedRun) models() string {
	var m []string
	for _, o := range r.got {
		m = append(m, o.Model)
	}
	return strings.Join(m, ",")
}

func rcDry() (*agent.Result, error) {
	return &agent.Result{SessionID: "s1", FinishReason: message.FinishReasonEndTurn}, nil
}

func rcAnswered() (*agent.Result, error) {
	return &agent.Result{SessionID: "s1", FinalText: "the review", FinishReason: message.FinishReasonEndTurn}, nil
}

func rcTransientFail() (*agent.Result, error) {
	return &agent.Result{SessionID: "s1"}, provider.NewTransientError(errors.New("kailab provider: 503: upstream overloaded"), 0)
}

func TestRunWithRetriesLadder(t *testing.T) {
	type outcome = func() (*agent.Result, error)
	for _, tc := range []struct {
		name       string
		model      string
		outcomes   []outcome
		wantModels string
		wantActive string
		wantText   string
		wantErr    bool
	}{
		{name: "an answer needs no retry", model: rcTestGLM, outcomes: []outcome{rcAnswered},
			wantModels: rcTestGLM, wantActive: rcTestGLM, wantText: "the review"},
		{name: "one dry run retries the same model", model: rcTestGLM, outcomes: []outcome{rcDry, rcAnswered},
			wantModels: rcTestGLM + "," + rcTestGLM, wantActive: rcTestGLM, wantText: "the review"},
		{name: "a second dry run fails over to the peer", model: rcTestGLM, outcomes: []outcome{rcDry, rcDry, rcAnswered},
			wantModels: rcTestGLM + "," + rcTestGLM + "," + rcTestKimi, wantActive: rcTestKimi, wantText: "the review"},
		{name: "every model dry ends dry, for the conclusion step", model: rcTestGLM, outcomes: []outcome{rcDry, rcDry, rcDry},
			wantModels: rcTestGLM + "," + rcTestGLM + "," + rcTestKimi, wantActive: rcTestKimi},
		{name: "a failing peer leaves the dry result standing", model: rcTestGLM, outcomes: []outcome{rcDry, rcDry, rcTransientFail},
			wantModels: rcTestGLM + "," + rcTestGLM + "," + rcTestKimi, wantActive: rcTestGLM},
		{name: "transient exhaustion skips the same-model step", model: rcTestGLM, outcomes: []outcome{rcTransientFail, rcAnswered},
			wantModels: rcTestGLM + "," + rcTestKimi, wantActive: rcTestKimi, wantText: "the review"},
		{name: "transient exhaustion on both models reports the first error", model: rcTestGLM, outcomes: []outcome{rcTransientFail, rcTransientFail},
			wantModels: rcTestGLM + "," + rcTestKimi, wantActive: rcTestGLM, wantErr: true},
		{name: "a model with no peer retries itself and stops", model: "claude-sonnet-5-5", outcomes: []outcome{rcDry, rcDry},
			wantModels: "claude-sonnet-5-5,claude-sonnet-5-5", wantActive: "claude-sonnet-5-5"},
		{name: "a hard failure gets no retry", model: rcTestGLM,
			outcomes:   []outcome{func() (*agent.Result, error) { return nil, errors.New("kailab provider: 401: bad token") }},
			wantModels: rcTestGLM, wantActive: rcTestGLM, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rcScriptedRun{outcomes: tc.outcomes}
			res, active, err := rcRunWithRetries(context.Background(), agent.Options{Model: tc.model, Prompt: "review", MaxTurns: 20}, time.Now(), r.run)
			if got := r.models(); got != tc.wantModels {
				t.Errorf("models tried = %s, want %s", got, tc.wantModels)
			}
			if active != tc.wantActive {
				t.Errorf("active model = %s, want %s", active, tc.wantActive)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && res.FinalText != tc.wantText {
				t.Errorf("final text = %q, want %q", res.FinalText, tc.wantText)
			}
			for i, o := range r.got {
				if !o.NoModelFailover {
					t.Errorf("run %d left the engine's failover open", i)
				}
			}
		})
	}
}

// A resume continues the session; only a run with no session starts over.
func TestRunWithRetriesResumesTheSession(t *testing.T) {
	r := &rcScriptedRun{outcomes: []func() (*agent.Result, error){rcDry, rcAnswered}}
	opts := agent.Options{Model: rcTestGLM, Prompt: "review", InjectedContext: "ctx", MaxTurns: 20}
	if _, _, err := rcRunWithRetries(context.Background(), opts, time.Now(), r.run); err != nil {
		t.Fatal(err)
	}
	second := r.got[1]
	if second.SessionID != "s1" || second.Prompt != rcResumePrompt || second.InjectedContext != "" {
		t.Errorf("resume options = session %q, prompt %q, injected %q", second.SessionID, second.Prompt, second.InjectedContext)
	}

	r = &rcScriptedRun{outcomes: []func() (*agent.Result, error){
		func() (*agent.Result, error) {
			return nil, provider.NewTransientError(errors.New("kailab provider: 503: overloaded"), 0)
		},
		rcAnswered,
	}}
	if _, _, err := rcRunWithRetries(context.Background(), opts, time.Now(), r.run); err != nil {
		t.Fatal(err)
	}
	if fresh := r.got[1]; fresh.SessionID != "" || fresh.Prompt != "review" || fresh.InjectedContext != "ctx" {
		t.Errorf("a run with no session should start over, got session %q, prompt %q", fresh.SessionID, fresh.Prompt)
	}
}

// With the hard deadline nearly spent there is no retry: the conclusion step
// needs what is left.
func TestRunWithRetriesRespectsTheClock(t *testing.T) {
	r := &rcScriptedRun{outcomes: []func() (*agent.Result, error){rcDry}}
	old := time.Now().Add(-rcReviewHardDeadline + time.Second)
	if _, _, err := rcRunWithRetries(context.Background(), agent.Options{Model: rcTestGLM, Prompt: "x"}, old, r.run); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 1 {
		t.Errorf("resumed %d time(s) with no clock left", len(r.got)-1)
	}
}

// A run stopped by its budget or turn cap is the conclusion step's job.
func TestRanDryIgnoresRunsThatStoppedOnALimit(t *testing.T) {
	for _, fr := range []message.FinishReason{message.FinishReasonTimeBudget, message.FinishReasonTurnCap, message.FinishReasonCanceled} {
		if rcRanDry(&agent.Result{FinishReason: fr}) {
			t.Errorf("%s counted as the model running dry", fr)
		}
	}
	if !rcRanDry(&agent.Result{FinishReason: message.FinishReasonEndTurn, FinalText: "  \n"}) {
		t.Error("a blank answer should count as running dry")
	}
	if rcRanDry(&agent.Result{FinishReason: message.FinishReasonEndTurn, FinalText: "prose"}) {
		t.Error("an answer is not a dry run")
	}
}

// rcModelLog is a provider that records which models it was asked for.
type rcModelLog struct {
	mu     sync.Mutex
	models []string
	reply  func(model string) provider.Response
}

func (p *rcModelLog) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.models = append(p.models, req.Model)
	p.mu.Unlock()
	return p.reply(req.Model), nil
}

func (p *rcModelLog) count(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.models {
		if m == model {
			n++
		}
	}
	return n
}

func rcTextReply(text string) provider.Response {
	return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: text}}, FinishReason: message.FinishReasonEndTurn}
}

// Through the real engine loop: an empty GLM reply must never reach Opus. The
// control run shows what the flag closes, so a change in the engine's own
// fallback shows up here rather than on a customer's bill.
func TestEmptyReplyNeverReachesOpus(t *testing.T) {
	const opus = "claude-opus-4-8"
	glmEmpty := func(model string) provider.Response {
		if model == rcTestGLM {
			return provider.Response{FinishReason: message.FinishReasonEndTurn}
		}
		return rcTextReply("answered by " + model)
	}
	base := agent.Options{Model: rcTestGLM, Prompt: "review this", Workspace: t.TempDir(), ReadOnly: true, MaxTurns: 3}

	control := &rcModelLog{reply: glmEmpty}
	o := base
	o.Provider = control
	if _, err := agent.Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if control.count(opus) == 0 {
		t.Fatalf("the engine no longer fails over to %s (models: %v); revisit review_commit_retry.go", opus, control.models)
	}

	p := &rcModelLog{reply: glmEmpty}
	o = base
	o.Provider = p
	res, active, err := rcRunWithRetries(context.Background(), o, time.Now(), agent.Run)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.count(opus); n != 0 {
		t.Errorf("%d call(s) reached %s: %v", n, opus, p.models)
	}
	if active != rcTestKimi || !strings.Contains(res.FinalText, rcTestKimi) {
		t.Errorf("active = %s, final = %q; want an answer from %s", active, res.FinalText, rcTestKimi)
	}
	if p.count(rcTestGLM) < 2 {
		t.Errorf("failed over before retrying the same model: %v", p.models)
	}
}

// rcConclusionTranscript is a transcript with something in it to conclude from.
var rcConclusionTranscript = []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "source"}}}}

const rcStrayConclusion = "<tool_call>kai_code_read add.go</arg_value></tool_call>"

// The reply from the 2026-11-05 probe. It has text, so it used to pass as a
// review and post "Nothing jumped out".
func TestConclusionRejectsStrayToolCallText(t *testing.T) {
	p := &rcModelLog{reply: func(string) provider.Response { return rcTextReply(rcStrayConclusion) }}
	got, category := rcConcludeFromTranscript(context.Background(), p, rcTestGLM, rcConclusionTranscript)
	if got != "" {
		t.Errorf("adopted %q as a conclusion", got)
	}
	if category != rcConclusionCategoryNoCoda {
		t.Errorf("category = %q, want %q", category, rcConclusionCategoryNoCoda)
	}
	if p.count(rcTestGLM) != 2 || p.count(rcTestKimi) != 1 {
		t.Errorf("attempts = %v, want GLM twice then Kimi once", p.models)
	}
}

func TestConclusionFailsOverToThePeerOnly(t *testing.T) {
	p := &rcModelLog{reply: func(model string) provider.Response {
		if model == rcTestKimi {
			return rcTextReply(rcTestReview())
		}
		return provider.Response{}
	}}
	got, category := rcConcludeFromTranscript(context.Background(), p, rcTestGLM, rcConclusionTranscript)
	if !rcUsableCoda(got) || category != "" {
		t.Errorf("got %q (%q), want the peer's review", got, category)
	}
	if p.count("claude-opus-4-8") != 0 {
		t.Errorf("reached Opus: %v", p.models)
	}
}

func TestConclusionWithNoPeerRetriesItselfOnly(t *testing.T) {
	p := &rcModelLog{reply: func(string) provider.Response { return provider.Response{} }}
	got, category := rcConcludeFromTranscript(context.Background(), p, "claude-sonnet-5-5", rcConclusionTranscript)
	if got != "" || category != "empty" {
		t.Errorf("got %q (%q)", got, category)
	}
	if len(p.models) != 2 {
		t.Errorf("attempts = %v, want two on the same model", p.models)
	}
}

// A provider failure has already been retried inside the provider, so it goes
// straight to the peer.
func TestConclusionProviderFailureSkipsSameModelRetry(t *testing.T) {
	var models []string
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		models = append(models, req.Model)
		if req.Model == rcTestGLM {
			return provider.Response{}, errors.New("kailab provider: 503: overloaded")
		}
		return rcTextReply(rcTestReview()), nil
	}}
	got, _ := rcConcludeFromTranscript(context.Background(), p, rcTestGLM, rcConclusionTranscript)
	if !rcUsableCoda(got) {
		t.Errorf("got %q", got)
	}
	if strings.Join(models, ",") != rcTestGLM+","+rcTestKimi {
		t.Errorf("attempts = %v", models)
	}
}

// A real review with stray syntax above it keeps the review and loses the syntax.
func TestConclusionStripsStrayToolCallsFromARealReview(t *testing.T) {
	p := &rcModelLog{reply: func(string) provider.Response {
		return rcTextReply(rcStrayConclusion + "\n" + rcTestReview())
	}}
	got, _ := rcConcludeFromTranscript(context.Background(), p, rcTestGLM, rcConclusionTranscript)
	if !rcUsableCoda(got) || strings.Contains(got, "tool_call") || strings.Contains(got, "arg_value") {
		t.Errorf("got %q", got)
	}
	if len(p.models) != 1 {
		t.Errorf("attempts = %v, want one", p.models)
	}
}

func TestWithoutStrayToolCalls(t *testing.T) {
	for in, want := range map[string]string{
		rcStrayConclusion:                              "",
		"<tool_call>kai_view a.go</arg_value>":         "",
		"prose\n<tool_call>x\ny</tool_call>\nmore":     "prose\n\nmore",
		"prose about the tool_call field in parser.go": "prose about the tool_call field in parser.go",
	} {
		if got := rcWithoutStrayToolCalls(in); got != want {
			t.Errorf("rcWithoutStrayToolCalls(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDraftForCheck(t *testing.T) {
	const issue = "add.go:3 — Add subtracts instead of adding, so every total is wrong (severity: high)"
	real := rcTestReview()
	if draft, sweepOnly, ok := rcDraftForCheck(real, []string{issue}); draft != real || sweepOnly || !ok {
		t.Errorf("a real review should pass through: %q %v %v", draft, sweepOnly, ok)
	}
	for _, hollow := range []string{"", rcStrayConclusion, "Looks fine, nothing to add."} {
		if draft, sweepOnly, ok := rcDraftForCheck(hollow, nil); draft != "" || sweepOnly || ok {
			t.Errorf("rcDraftForCheck(%q, nil) = %q %v %v; want nothing to check", hollow, draft, sweepOnly, ok)
		}
		draft, sweepOnly, ok := rcDraftForCheck(hollow, []string{issue})
		if !sweepOnly || !ok || strings.Contains(draft, hollow+"\n") && hollow != "" {
			t.Fatalf("rcDraftForCheck(%q, issue) = %q %v %v", hollow, draft, sweepOnly, ok)
		}
		merged := rcDraftWithSweep(draft, []string{issue})
		if !rcUsableCoda(merged) {
			t.Errorf("sweep-only draft is not usable:\n%s", merged)
		}
		if got := rcIssuesOf(merged); len(got) != 1 || !strings.Contains(got[0], "add.go:3") {
			t.Errorf("issues = %q", got)
		}
		_, _, _, match, readiness, _ := rcParseReviewOutput(merged)
		if string(match) != "" && match != "unknown" {
			t.Errorf("a sweep-only draft must offer no verdict, got match %q", match)
		}
		if int(readiness) != 0 {
			t.Errorf("a sweep-only draft must offer no verdict, got readiness %d", readiness)
		}
	}
}

// The checked findings stand, and the report says the rest did not happen.
func TestIncompleteProseForSweepOnlyReview(t *testing.T) {
	got := rcIncompleteProse(&rcIncomplete{FinishReason: "end_turn", Elapsed: time.Minute, Turns: 2, ConclusionCategory: rcConclusionCategoryNoCoda, SweepOnly: true})
	for _, want := range []string{"This review did not finish", "ended without writing its review down", "held up when checked", `"not reviewed"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "no claim was checked") {
		t.Errorf("says nothing was checked, above findings that were:\n%s", got)
	}
}

func TestIncompleteReasonForRejectedConclusion(t *testing.T) {
	r := rcIncompleteReasonOf(&rcIncomplete{Model: rcTestGLM, ConclusionCategory: rcConclusionCategoryNoCoda, ConclusionModel: rcTestGLM})
	if r.Stage != rcStageNameConclusion || r.Category != rcConclusionCategoryNoCoda {
		t.Errorf("reason = %+v", r)
	}
}
