package main

// What a review really ran on, for the bundle's "run" object.
//
// Before this, a review said which models it was configured with (and only on
// stderr) but not which ones answered. kai-engine swaps models when one fails
// (an empty answer, a run of transient errors), and the transcript stamps the
// model that was asked for, so a review that ran on a fallback looked exactly
// like one that did not. The server needs the truth to show users their review
// depth, measure cost per model set, prove Free stayed on its models, and know
// when a set is failing.
//
// Every model call a review makes goes through one of two doors, and both
// record here:
//   - rcSend, for the one-shot calls (intent, sweep, the fact-check, rank,
//     humanize, the description, grouping, conclusions, the fast draft);
//   - rcRunAgent, for the agent runs (main, the coverage gate, finder2, the
//     specialists), through the engine's OnModelSwap, OnTurnTiming and
//     OnSummarize hooks.
//
// Both take the stage from the context, set by the cost label each call site
// already carries (rcCostStage, review_commit_cost_stage.go), so the stage a
// call is billed to and the stage it is reported under are one name. The
// provider is never wrapped: the engine uploads a run's cost only when
// Options.Provider is the kailab provider itself.

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/provider"
)

// rcStageUnlabelled catches a model call made without a stage, so it is still
// reported rather than lost. A test fails if any review call lands here.
const rcStageUnlabelled = "unlabelled"

// Stage statuses.
const (
	rcRunOK      = "ok"      // every call answered
	rcRunFailed  = "failed"  // a call failed, or was still running when the review was written
	rcRunOff     = "off"     // not configured for this review
	rcRunSkipped = "skipped" // configured, but the review did not need it
)

// rcRunStage is one stage's line in the report.
type rcRunStage struct {
	// Model is the model the stage was configured with.
	Model string `json:"model"`
	// Ran is the model whose answer the stage last used: Model, unless the
	// engine failed over. Empty when nothing answered.
	Ran    string `json:"ran"`
	Status string `json:"status"`
	// Failovers counts the calls sent to a substitute model.
	Failovers int `json:"failovers"`
	// Calls and Errors count the stage's model calls (agent turns are one
	// call per run). Omitted when the stage made none.
	Calls  int `json:"calls,omitempty"`
	Errors int `json:"errors,omitempty"`

	inflight int
}

// rcRunFailover is one kind of swap the engine made. A swap the engine
// remembers is repeated on every later turn; those repeats are one entry with
// a count.
type rcRunFailover struct {
	Stage  string `json:"stage"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// rcRunReport is the bundle's "run" object.
type rcRunReport struct {
	// Tier is the depth the server chose (KAI_REVIEW_TIER), "" when unset.
	Tier string `json:"tier"`
	// SetVersion is the model set's version (KAI_REVIEW_SET_VERSION), 0 when
	// unset.
	SetVersion int `json:"set_version"`
	// Risk is the pre-review risk level (rcPreReviewRisk), "" when unrated.
	Risk string `json:"risk"`
	// Profile is which review profile the stages were resolved from:
	// low_risk, standard (any other profile), or none.
	Profile   string                 `json:"profile"`
	Stages    map[string]*rcRunStage `json:"stages"`
	Failovers []rcRunFailover        `json:"failovers"`
	// SummaryModel is the model that summarised a conversation that grew too
	// long, "" when none was summarised.
	SummaryModel string `json:"summary_model"`
	// SummaryFailures are the models asked to summarise whose call failed,
	// each once. A failed summary summarised nothing, so it never sets
	// SummaryModel, but the call was still made and is still reported.
	SummaryFailures []string `json:"summary_failures,omitempty"`
}

const (
	rcProfileLowRisk  = "low_risk"
	rcProfileStandard = "standard"
	rcProfileNone     = "none"
)

// rcRunRecorder collects the report. Stages run concurrently (the sweep's
// chunks, finder2, the specialists, the description), so every method locks.
type rcRunRecorder struct {
	mu     sync.Mutex
	report rcRunReport
}

// rcRun is this process's recorder. review-commit reviews one commit per
// process; rcRunStart resets it.
var rcRun = &rcRunRecorder{}

// rcRunStart resets the report for a new review. stageModels is each stage's
// configured model, as resolved before the review starts; a stage with no
// model is off, the rest are skipped until they make a call.
func rcRunStart(risk, profile string, stageModels map[string]string) {
	r := rcRunReport{
		Tier:      strings.TrimSpace(os.Getenv("KAI_REVIEW_TIER")),
		Risk:      risk,
		Profile:   profile,
		Stages:    map[string]*rcRunStage{},
		Failovers: []rcRunFailover{},
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("KAI_REVIEW_SET_VERSION"))); err == nil {
		r.SetVersion = v
	}
	for stage, model := range stageModels {
		status := rcRunSkipped
		if model == "" {
			status = rcRunOff
		}
		r.Stages[stage] = &rcRunStage{Model: model, Status: status}
	}
	rcRun.mu.Lock()
	rcRun.report = r
	rcRun.mu.Unlock()
}

// rcRunStageModels is every stage a review may call, with its configured
// model: the profile's stages plus the ones that borrow another's model.
func rcRunStageModels(stageModels map[string]string, jsonOut bool) map[string]string {
	out := make(map[string]string, len(stageModels)+8)
	for k, v := range stageModels {
		out[k] = v
	}
	out[rcCostStageCoverageGate] = stageModels[rcStageMain]
	out[rcCostStageGroup] = stageModels[rcStageFactcheck]
	if jsonOut {
		out[rcCostStageDescription] = stageModels[rcStageIntent]
	} else {
		out[rcCostStageDescription] = ""
	}
	out[rcStageFinder2+rcConclusionSuffix] = stageModels[rcStageFinder2]
	for _, s := range rcSpecialistStages {
		out[s+rcConclusionSuffix] = stageModels[s]
	}
	return out
}

// rcRunProfile names the profile the stages resolved from.
func rcRunProfile(lowRiskLoaded bool) string {
	switch {
	case lowRiskLoaded:
		return rcProfileLowRisk
	case rcActiveProfile != nil:
		return rcProfileStandard
	}
	return rcProfileNone
}

// rcRunMark sets a stage's status outright: off when a stage turns out not to
// apply (finder2 on the main finder's own model).
func rcRunMark(stage, model, status string) {
	rcRun.mu.Lock()
	defer rcRun.mu.Unlock()
	st := rcRun.stage(stage, model)
	if st.Calls == 0 {
		st.Status = status
	}
}

// stage returns the stage's entry, creating it for a stage the start did not
// list. Caller holds mu.
func (r *rcRunRecorder) stage(name, model string) *rcRunStage {
	if r.report.Stages == nil {
		r.report.Stages = map[string]*rcRunStage{}
	}
	st, ok := r.report.Stages[name]
	if !ok {
		st = &rcRunStage{Model: model, Status: rcRunSkipped}
		r.report.Stages[name] = st
	}
	if st.Model == "" {
		st.Model = model
	}
	return st
}

func (r *rcRunRecorder) begin(stage, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stage(stage, model)
	st.Calls++
	st.inflight++
}

// end closes a call. answered is the model whose answer was used; "" leaves
// Ran as it was (a failed call answered nothing).
func (r *rcRunRecorder) end(stage, answered string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stage(stage, "")
	st.inflight--
	if err != nil {
		st.Errors++
	} else if answered != "" {
		st.Ran = answered
	}
}

// answered records the model that answered one agent turn.
func (r *rcRunRecorder) answered(stage, model string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stage(stage, "").Ran = model
}

func (r *rcRunRecorder) swapped(stage, from, to, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stage(stage, from).Failovers++
	for i := range r.report.Failovers {
		f := &r.report.Failovers[i]
		if f.Stage == stage && f.From == from && f.To == to && f.Reason == reason {
			f.Count++
			return
		}
	}
	r.report.Failovers = append(r.report.Failovers, rcRunFailover{Stage: stage, From: from, To: to, Reason: reason, Count: 1})
}

// summarized records one summarizer call: the model that summarised when
// it answered, a failure when it did not.
func (r *rcRunRecorder) summarized(model string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.report.SummaryModel = model
		return
	}
	for _, m := range r.report.SummaryFailures {
		if m == model {
			return
		}
	}
	r.report.SummaryFailures = append(r.report.SummaryFailures, model)
}

// rcRunSnapshot is the report as it stands, with each stage's status settled.
// A stage still running now did not finish in time for this review, and is
// reported failed.
func rcRunSnapshot() *rcRunReport {
	rcRun.mu.Lock()
	defer rcRun.mu.Unlock()
	src := rcRun.report
	out := src
	out.Stages = make(map[string]*rcRunStage, len(src.Stages))
	for name, st := range src.Stages {
		c := *st
		if c.Calls > 0 {
			c.Status = rcRunOK
			if c.Errors > 0 || c.inflight > 0 {
				c.Status = rcRunFailed
			}
		}
		out.Stages[name] = &c
	}
	out.Failovers = append([]rcRunFailover{}, src.Failovers...)
	out.SummaryFailures = append([]string(nil), src.SummaryFailures...)
	sort.SliceStable(out.Failovers, func(i, j int) bool { return out.Failovers[i].Stage < out.Failovers[j].Stage })
	return &out
}

// rcStageOf is the stage rcCostStage put on ctx.
func rcStageOf(ctx context.Context) string {
	if s, _ := ctx.Value(rcStageKey{}).(string); s != "" {
		return s
	}
	return rcStageUnlabelled
}

// rcSend is prov.Send, recorded under ctx's stage. These calls have no model
// failover, so the model asked is the model that answered.
func rcSend(ctx context.Context, prov provider.Provider, req provider.Request) (provider.Response, error) {
	stage := rcStageOf(ctx)
	rcRun.begin(stage, req.Model)
	resp, err := prov.Send(ctx, req)
	rcRun.end(stage, req.Model, err)
	return resp, err
}

// rcRunAgent is agent.Run for a review stage: the calls are labelled with the
// stage, and the engine's hooks report each swap, each turn's answering model
// and each summary. Hooks already on opts keep firing.
func rcRunAgent(ctx context.Context, stage string, opts agent.Options) (*agent.Result, error) {
	ctx = rcCostStage(ctx, stage)
	opts.Hooks = rcRunHooks(stage, opts.Hooks)
	rcRun.begin(stage, opts.Model)
	res, err := agent.Run(ctx, opts)
	rcRun.end(stage, "", err)
	return res, err
}

func rcRunHooks(stage string, h agent.Hooks) agent.Hooks {
	swap, timing, summarize := h.OnModelSwap, h.OnTurnTiming, h.OnSummarize
	h.OnModelSwap = func(from, to, reason string) {
		rcRun.swapped(stage, from, to, reason)
		if swap != nil {
			swap(from, to, reason)
		}
	}
	h.OnTurnTiming = func(t agent.TurnTiming) {
		// A turn with no finish reason got no response: its Model is the
		// one asked for, not one that answered.
		if t.FinishReason != "" {
			rcRun.answered(stage, t.Model)
		}
		if timing != nil {
			timing(t)
		}
	}
	h.OnSummarize = func(model string, err error) {
		rcRun.summarized(model, err)
		if summarize != nil {
			summarize(model, err)
		}
	}
	return h
}
