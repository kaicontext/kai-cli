package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcModelsIn is every model the report names: each stage's configured and
// answering model, both ends of every failover, and the summarising model.
func rcModelsIn(r *rcRunReport) map[string]bool {
	in := map[string]bool{}
	for _, st := range r.Stages {
		in[st.Model], in[st.Ran] = true, true
	}
	for _, f := range r.Failovers {
		in[f.From], in[f.To] = true, true
	}
	in[r.SummaryModel] = true
	for _, m := range r.SummaryFailures {
		in[m] = true
	}
	delete(in, "")
	return in
}

// A whole review, run the way CI runs it (--format json), against a fake
// model server: the bundle carries a "run" object naming the depth, the set
// version, each stage the review used and the model that answered it, and
// every model the server was asked for appears in it.
func TestReviewRunReport_ReviewAgainstFakeModelServer(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		asked[body.Model]++
		mu.Unlock()
		text := "The change renames the greeting.\n\n" + rcReviewDataMarker + "\nINTENT_MATCH: yes\nMERGE_READY: 4\nSUMMARY: renames the greeting\nISSUES:\nDECISIONS:\n"
		if strings.Contains(string(raw), "COMMITS AND THE AUTHOR'S DESCRIPTION") {
			text = "Renames the greeting."
		}
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greet.go"), []byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "greet.go"), []byte("package greet\n\nfunc Hello() string { return \"hello\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, dir, "Say hello in full")
	t.Chdir(dir)

	t.Setenv("KAI_PROVIDER", "openai")
	t.Setenv("KAI_OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("KAI_OPENAI_MODEL", "vendor/review-model")
	t.Setenv("KAI_REVIEW_TIER", "standard")
	t.Setenv("KAI_REVIEW_SET_VERSION", "3")
	t.Setenv("KAI_REVIEW_PROFILE", "")
	t.Setenv("KAI_REVIEW_PROFILE_LOW_RISK", "")
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
	oldKaiDir, oldFormat := kaiDir, reviewCommitFormat
	kaiDir, reviewCommitFormat = t.TempDir(), "json"
	t.Cleanup(func() { kaiDir, reviewCommitFormat = oldKaiDir, oldFormat })
	var stdout bytes.Buffer
	reviewCommitCmd.SetOut(&stdout)
	t.Cleanup(func() { reviewCommitCmd.SetOut(nil) })

	if err := runReviewCommit(reviewCommitCmd, []string{"HEAD"}); err != nil {
		t.Fatalf("review: %v", err)
	}
	var bundle struct {
		Run *rcRunReport `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle: %v\n%s", err, stdout.String())
	}
	run := bundle.Run
	if run == nil {
		t.Fatalf("bundle has no run object:\n%s", stdout.String())
	}
	if run.Tier != "standard" || run.SetVersion != 3 || run.Profile != rcProfileNone {
		t.Errorf("tier=%q set_version=%d profile=%q, want standard, 3, none", run.Tier, run.SetVersion, run.Profile)
	}
	for _, stage := range []string{rcStageQuickDraft, rcCostStageDescription} {
		st := run.Stages[stage]
		if st == nil || st.Status != rcRunOK || st.Model != "vendor/review-model" || st.Ran != "vendor/review-model" || st.Calls != 1 {
			t.Errorf("stage %s = %+v, want one ok call answered by vendor/review-model", stage, st)
		}
	}
	// A draft with no issues has nothing to fact-check, and the grounded
	// stages belong to --deep.
	for _, stage := range []string{rcStageQuickFactcheck, rcStageMain, rcStageSweep, rcStageConclusion} {
		if st := run.Stages[stage]; st == nil || st.Status != rcRunSkipped {
			t.Errorf("stage %s = %+v, want skipped", stage, st)
		}
	}
	if st := run.Stages[rcStageUnlabelled]; st != nil {
		t.Errorf("a model call carried no stage: %+v", st)
	}
	named := rcModelsIn(run)
	if len(asked) == 0 {
		t.Fatal("test bug: the fake model server was never called")
	}
	for m := range asked {
		if !named[m] {
			t.Errorf("the server was asked for %q, which the run object never names", m)
		}
	}
}

// fakeModels is an in-process model server for the agent stages: it records
// every model it is asked for and answers from a script.
type fakeModels struct {
	mu    sync.Mutex
	asked []string
	reply func(req provider.Request, n int) (provider.Response, error)
	n     int
}

func (f *fakeModels) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req.Model)
	f.n++
	n := f.n
	f.mu.Unlock()
	return f.reply(req, n)
}

const rcTestSummarizerSystem = "You are a terse, factual summarizer of agent sessions."

func rcTextReply(s string) provider.Response {
	return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: s}}, FinishReason: message.FinishReasonEndTurn}
}

// An agent stage through the real engine: its model comes back empty, the
// engine rescues the turn on another model, and the conversation grows long
// enough to be summarised. The report names the swap, the model that
// answered, and the summarising model, and every model asked for appears.
func TestReviewRunReport_AgentStageFailoverAndSummary(t *testing.T) {
	const reviewModel = "z-ai/glm-5.2"
	if !provider.IsReasoningModel(reviewModel) {
		t.Skipf("%s is no longer a reasoning model; the engine's rescue path would not run", reviewModel)
	}
	rcRunStart("M", rcProfileStandard, map[string]string{rcStageMain: reviewModel, rcStageFinder2: ""})

	emptied := false
	tools := 0
	f := &fakeModels{reply: func(req provider.Request, _ int) (provider.Response, error) {
		if req.System == rcTestSummarizerSystem {
			return rcTextReply("summary"), nil
		}
		if req.Model == reviewModel && !emptied {
			emptied = true
			return provider.Response{FinishReason: message.FinishReasonEndTurn}, nil // empty once
		}
		tools++
		if tools <= 10 { // tool turns reporting a nearly full context window
			return provider.Response{
				Parts: []message.ContentPart{message.ToolCall{ID: fmt.Sprintf("c%d", tools), Name: "kai_files",
					Input: `{}`, Type: "function", Finished: true}},
				FinishReason: message.FinishReasonToolUse, InputTokens: 170_000, OutputTokens: 10,
			}, nil
		}
		return rcTextReply("done"), nil
	}}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawSwap bool
	res, err := rcRunAgent(context.Background(), rcStageMain, agent.Options{
		Workspace: ws, Prompt: "review", System: "s", Model: reviewModel,
		CompactionModel: "vendor/summarizer", Provider: f,
		// A hook the caller set keeps firing.
		Hooks: agent.Hooks{OnModelSwap: func(string, string, string) { sawSwap = true }},
	})
	if err != nil || res.FinishReason != message.FinishReasonEndTurn {
		t.Fatalf("run: %v %+v", err, res)
	}
	if !sawSwap {
		t.Error("the caller's own OnModelSwap stopped firing")
	}

	run := rcRunSnapshot()
	main := run.Stages[rcStageMain]
	if main == nil || main.Status != rcRunOK || main.Model != reviewModel || main.Failovers == 0 {
		t.Fatalf("main = %+v, want ok on %s with a failover", main, reviewModel)
	}
	if len(run.Failovers) == 0 {
		t.Fatal("no failover recorded")
	}
	first := run.Failovers[0]
	if first.Stage != rcStageMain || first.From != reviewModel || first.To == "" || first.Reason != agent.ModelSwapReasoningEmpty {
		t.Errorf("failover = %+v, want main %s → rescuer, reasoning_empty", first, reviewModel)
	}
	// The engine remembers the rescue, so the later turns ran on it too.
	if main.Ran != first.To {
		t.Errorf("main ran %q, want the rescuer %q", main.Ran, first.To)
	}
	if run.SummaryModel != "vendor/summarizer" {
		t.Errorf("summary_model = %q, want vendor/summarizer", run.SummaryModel)
	}
	if st := run.Stages[rcStageFinder2]; st == nil || st.Status != rcRunOff {
		t.Errorf("finder2 = %+v, want off (no model)", st)
	}
	if run.Risk != "M" || run.Profile != rcProfileStandard {
		t.Errorf("risk=%q profile=%q", run.Risk, run.Profile)
	}
	named := rcModelsIn(run)
	for _, m := range f.asked {
		if !named[m] {
			t.Errorf("the model server was asked for %q, which the run object never names", m)
		}
	}
}

// A failed call fails its stage, and a stage still running when the review
// is written did not finish in time for it.
func TestReviewRunReport_FailedAndUnfinishedStages(t *testing.T) {
	rcRunStart("", rcProfileNone, map[string]string{rcStageSweep: "m/sweep", rcStageRank: "m/rank", rcStageHumanize: "m/h"})
	f := &fakeModels{reply: func(req provider.Request, _ int) (provider.Response, error) {
		if req.Model == "m/rank" {
			return provider.Response{}, errors.New("upstream 500")
		}
		return rcTextReply("ok"), nil
	}}
	sweepCtx := rcCostStage(context.Background(), rcStageSweep)
	if _, err := rcSend(sweepCtx, f, provider.Request{Model: "m/sweep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rcSend(rcCostStage(context.Background(), rcStageRank), f, provider.Request{Model: "m/rank"}); err == nil {
		t.Fatal("want the rank call to fail")
	}
	rcRun.begin(rcStageHumanize, "m/h") // still running

	run := rcRunSnapshot()
	want := map[string]string{rcStageSweep: rcRunOK, rcStageRank: rcRunFailed, rcStageHumanize: rcRunFailed}
	var got []string
	for stage, status := range want {
		if st := run.Stages[stage]; st == nil || st.Status != status {
			got = append(got, fmt.Sprintf("%s=%+v", stage, st))
		}
	}
	sort.Strings(got)
	if len(got) > 0 {
		t.Errorf("want %v, got %v", want, got)
	}
	if r := run.Stages[rcStageRank]; r.Ran != "" || r.Errors != 1 {
		t.Errorf("rank = %+v: a failed call answered nothing", r)
	}
	rcRun.end(rcStageHumanize, "m/h", nil)
}

// A remembered failover repeats on every turn; the report keeps one entry
// per swap with a count, and the stage counts every call.
func TestReviewRunReport_RepeatedSwapIsOneEntry(t *testing.T) {
	rcRunStart("", rcProfileNone, nil)
	h := rcRunHooks(rcStageFinder2, agent.Hooks{})
	h.OnModelSwap("a", "b", agent.ModelSwapEmpty)
	for i := 0; i < 3; i++ {
		h.OnModelSwap("a", "b", agent.ModelSwapRemembered)
	}
	h.OnTurnTiming(agent.TurnTiming{Model: "b", FinishReason: "end_turn"})
	h.OnTurnTiming(agent.TurnTiming{Model: "a"}) // no response: not an answer
	run := rcRunSnapshot()
	if len(run.Failovers) != 2 || run.Failovers[1].Count != 3 {
		t.Errorf("failovers = %+v, want empty once and remembered ×3", run.Failovers)
	}
	if st := run.Stages[rcStageFinder2]; st.Failovers != 4 || st.Ran != "b" {
		t.Errorf("finder2 = %+v, want 4 failovers, ran b", st)
	}
}

// The bundle field is additive: an older reader that knows none of it still
// decodes the rest.
func TestReviewRunReport_JSONShape(t *testing.T) {
	rcRunStart("S", rcProfileLowRisk, map[string]string{rcStageMain: "m"})
	b, err := json.Marshal(rcRunSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"tier":`, `"set_version":`, `"risk":"S"`, `"profile":"low_risk"`, `"stages":`, `"failovers":[]`, `"summary_model":""`, `"main":{"model":"m","ran":"","status":"skipped","failovers":0}`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("run JSON lacks %s:\n%s", key, b)
		}
	}
}

// A finder's own conclusion is labelled and reported apart from the main
// finder's: it runs on that finder's model, so one shared label would mix
// models in both the cost rows and the report.
func TestReviewRunReport_FinderConclusionHasItsOwnStage(t *testing.T) {
	rcRunStart("", rcProfileNone, map[string]string{rcStageConclusion: "m/main", rcStageFinder2 + rcConclusionSuffix: "m/f2"})
	f := &fakeModels{reply: func(provider.Request, int) (provider.Response, error) { return rcTextReply("review"), nil }}
	transcript := []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "diff"}}}}
	// A stage set by the caller is replaced by the one the call names.
	ctx := rcCostStage(context.Background(), rcStageMain)
	if out, _ := rcConcludeWithModel(ctx, f, rcStageFinder2+rcConclusionSuffix, "m/f2", transcript); out == "" {
		t.Fatal("no conclusion")
	}
	run := rcRunSnapshot()
	if st := run.Stages[rcStageFinder2+rcConclusionSuffix]; st == nil || st.Status != rcRunOK || st.Ran != "m/f2" {
		t.Errorf("finder2_conclusion = %+v, want ok on m/f2", st)
	}
	if st := run.Stages[rcStageConclusion]; st == nil || st.Status != rcRunSkipped {
		t.Errorf("conclusion = %+v, want skipped", st)
	}
	if st := run.Stages[rcStageMain]; st != nil {
		t.Errorf("the caller's stage was reported: %+v", st)
	}
}

// A summary that failed summarised nothing: it leaves summary_model empty,
// and the model asked is reported as a failure instead, so the call is still
// accounted for. A later summary that works sets summary_model.
func TestReviewRunReport_FailedSummaryIsNotTheSummaryModel(t *testing.T) {
	rcRunStart("", rcProfileNone, nil)
	var callerSaw []error
	h := rcRunHooks(rcStageMain, agent.Hooks{OnSummarize: func(_ string, err error) { callerSaw = append(callerSaw, err) }})
	h.OnSummarize("vendor/summarizer", errors.New("upstream 500"))
	h.OnSummarize("vendor/summarizer", errors.New("upstream 500"))
	run := rcRunSnapshot()
	if run.SummaryModel != "" {
		t.Errorf("summary_model = %q after only failed summaries, want empty", run.SummaryModel)
	}
	if len(run.SummaryFailures) != 1 || run.SummaryFailures[0] != "vendor/summarizer" {
		t.Errorf("summary_failures = %v, want [vendor/summarizer] once", run.SummaryFailures)
	}
	if !rcModelsIn(run)["vendor/summarizer"] {
		t.Error("a summarizer that was asked is missing from the run object")
	}
	h.OnSummarize("vendor/summarizer", nil)
	if got := rcRunSnapshot().SummaryModel; got != "vendor/summarizer" {
		t.Errorf("summary_model = %q after a summary worked, want vendor/summarizer", got)
	}
	if len(callerSaw) != 3 || callerSaw[0] == nil || callerSaw[2] != nil {
		t.Errorf("the caller's OnSummarize saw %v, want both failures then success", callerSaw)
	}
}
