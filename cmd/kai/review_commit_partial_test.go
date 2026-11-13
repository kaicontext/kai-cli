package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

func TestPartialMode(t *testing.T) {
	for v, want := range map[string]string{
		"": rcPartialOff, "off": rcPartialOff, "OFF": rcPartialOff, "record": rcPartialRecord,
		" show ": rcPartialShow, "on": rcPartialOff, "yes": rcPartialOff,
	} {
		t.Setenv("KAI_REVIEW_PARTIAL", v)
		if got := rcPartialMode(); got != want {
			t.Errorf("KAI_REVIEW_PARTIAL=%q → %q, want %q", v, got, want)
		}
	}
}

func TestSetBackups(t *testing.T) {
	for v, want := range map[string][]string{
		"":                       nil,
		"  ":                     nil,
		"a/x":                    {"a/x"},
		" a/x , b/y,,A/X , c/z ": {"a/x", "b/y", "c/z"},
		// Separators with no model name are not a list: sending them must
		// not switch failover off.
		",":      nil,
		" , ,, ": nil,
	} {
		t.Setenv("KAI_REVIEW_FALLBACK_MODELS", v)
		got, sent := rcSetBackups()
		if sent != (want != nil) || !slices.Equal(got, want) {
			t.Errorf("%q → %v (sent %v), want %v", v, got, sent, want)
		}
	}
}

// Off, the bundle carries no partial fields at all. On, both are always
// there, and only reasons that count make the review partial.
func TestPartialFields(t *testing.T) {
	rcRunStart(rcRisk{}, rcProfileNone, nil)
	rcNotePartial(rcPartialRankFailed)
	t.Setenv("KAI_REVIEW_PARTIAL", "off")
	if p, r := rcPartialFields(rcRunSnapshot()); p != nil || r != nil {
		t.Fatalf("off: partial=%v reasons=%v, want neither", p, r)
	}

	t.Setenv("KAI_REVIEW_PARTIAL", "record")
	rcRunStart(rcRisk{}, rcProfileNone, nil)
	p, r := rcPartialFields(rcRunSnapshot())
	if p == nil || *p || r == nil || len(*r) != 0 {
		t.Fatalf("clean: partial=%v reasons=%v, want false and []", p, r)
	}
	b, _ := json.Marshal(struct {
		Partial        *bool     `json:"partial,omitempty"`
		PartialReasons *[]string `json:"partialReasons,omitempty"`
	}{p, r})
	if string(b) != `{"partial":false,"partialReasons":[]}` {
		t.Errorf("clean bundle fields = %s", b)
	}

	rcNotePartial(rcPartialHumanizeFailed)
	rcRun.swapped(rcStageSweep, "a", "b", rcSwapEmpty)
	p, r = rcPartialFields(rcRunSnapshot())
	if *p || !slices.Equal(*r, []string{rcPartialHumanizeFailed, rcPartialFailover}) {
		t.Errorf("recorded-only reasons: partial=%v reasons=%v, want false with both listed", *p, *r)
	}
	rcNotePartial(rcFinderFailed(rcStageFinder2))
	rcNotePartial(rcFinderFailed(rcStageFinder2))
	p, r = rcPartialFields(rcRunSnapshot())
	if !*p || len(*r) != 3 {
		t.Errorf("a failed finder: partial=%v reasons=%v, want true, three reasons", *p, *r)
	}
}

// rcScriptedSend answers each model from a script, in order per model.
type rcScriptedSend struct {
	mu     sync.Mutex
	asked  []string
	script map[string][]func() (provider.Response, error)
}

func (s *rcScriptedSend) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req.Model)
	q := s.script[req.Model]
	if len(q) == 0 {
		return rcTextReply("answer from " + req.Model), nil
	}
	s.script[req.Model] = q[1:]
	return q[0]()
}

func rcEmptyReply() (provider.Response, error) {
	return provider.Response{FinishReason: message.FinishReasonEndTurn}, nil
}

func rc503() (provider.Response, error) {
	return provider.Response{}, provider.NewTransientError(errors.New("503: overloaded"), 0)
}

func TestSendOnSet(t *testing.T) {
	ctx := rcCostStage(context.Background(), rcStageRank)
	req := provider.Request{Model: "set/primary"}
	for _, tc := range []struct {
		name     string
		backups  string
		script   map[string][]func() (provider.Response, error)
		wantAsk  string
		wantFrom string
		wantErr  bool
		wantNone bool // nothing answered: the step is recorded failed
		wantCap  bool // the error returned is the spent budget
	}{
		{name: "no backups sent: one plain call", backups: "",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rcEmptyReply}},
			wantAsk: "set/primary", wantFrom: ""},
		{name: "an empty reply is asked again", backups: "set/b1",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rcEmptyReply}},
			wantAsk: "set/primary,set/primary", wantFrom: "set/primary"},
		{name: "twice empty walks the backups in order", backups: "set/primary,set/b1,set/b2",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rcEmptyReply, rcEmptyReply}, "set/b1": {rcEmptyReply}},
			wantAsk: "set/primary,set/primary,set/b1,set/b2", wantFrom: "set/b2"},
		{name: "a transient failure goes straight to the backups", backups: "set/b1",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rc503}},
			wantAsk: "set/primary,set/b1", wantFrom: "set/b1"},
		{name: "a hard failure is returned as it is", backups: "set/b1",
			script: map[string][]func() (provider.Response, error){"set/primary": {func() (provider.Response, error) {
				return provider.Response{}, errors.New("401: bad token")
			}}},
			wantAsk: "set/primary", wantErr: true},
		{name: "nothing answering is a failed step", backups: "set/b1",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rcEmptyReply, rcEmptyReply}, "set/b1": {rcEmptyReply}},
			wantAsk: "set/primary,set/primary,set/b1", wantNone: true},
		{name: "a spent budget on a backup stops the walk", backups: "set/b1,set/b2",
			script: map[string][]func() (provider.Response, error){"set/primary": {rc503}, "set/b1": {func() (provider.Response, error) {
				return provider.Response{}, &provider.Error{Code: provider.CodeCapExceeded, Message: "monthly cap reached"}
			}}},
			wantAsk: "set/primary,set/b1", wantErr: true, wantCap: true},
		{name: "every backup failing returns the first failure", backups: "set/b1",
			script:  map[string][]func() (provider.Response, error){"set/primary": {rc503}, "set/b1": {rc503}},
			wantAsk: "set/primary,set/b1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAI_REVIEW_FALLBACK_MODELS", tc.backups)
			rcRunStart(rcRisk{}, rcProfileNone, map[string]string{rcStageRank: "set/primary"})
			p := &rcScriptedSend{script: tc.script}
			resp, err := rcSend(ctx, p, req)
			if got := strings.Join(p.asked, ","); got != tc.wantAsk {
				t.Errorf("asked %s, want %s", got, tc.wantAsk)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantCap != provider.IsCapExceeded(err) {
				t.Errorf("err = %v, want the spent budget %v", err, tc.wantCap)
			}
			st := rcRunSnapshot().Stages[rcStageRank]
			if tc.wantNone && (err != nil || st.Status != rcRunFailed || st.Ran != "") {
				t.Errorf("nothing answered: err=%v stage=%+v, want no error to the caller and a failed stage with no model", err, st)
			}
			if !tc.wantNone && !tc.wantErr && st.Status != rcRunOK {
				t.Errorf("stage = %+v, want ok", st)
			}
			if tc.wantFrom != "" {
				if rcResponseText(resp) != "answer from "+tc.wantFrom || st.Ran != tc.wantFrom {
					t.Errorf("answer %q, ran %q; want %s", rcResponseText(resp), st.Ran, tc.wantFrom)
				}
			}
			if want := len(strings.Split(tc.wantAsk, ",")) - 1; st.Retries != want {
				t.Errorf("retries = %d, want %d: every call after the first", st.Retries, want)
			}
			var to []string
			for _, f := range rcRunSnapshot().Failovers {
				to = append(to, f.To)
			}
			wantTo := []string{}
			for _, m := range strings.Split(tc.wantAsk, ",")[1:] {
				if m != "set/primary" {
					wantTo = append(wantTo, m)
				}
			}
			if strings.Join(to, ",") != strings.Join(wantTo, ",") {
				t.Errorf("failovers to %v, want %v", to, wantTo)
			}
		})
	}
}

// A chunk counts as lost only when every sweep that read it lost it.
func TestSweepLostChunks(t *testing.T) {
	a := rcSweepResult{Chunks: 3, Lost: []bool{true, true, false}, Unswept: 2}
	b := rcSweepResult{Chunks: 3, Lost: []bool{false, true, true}, Unswept: 2}
	if n := rcMergeSweeps(a, b).LostChunks(); n != 1 {
		t.Errorf("lost by both = %d, want 1", n)
	}
	if n := rcMergeSweeps(a, rcSweepResult{}).LostChunks(); n != 2 {
		t.Errorf("a second sweep that never ran: lost = %d, want 2", n)
	}
	if u := rcMergeSweeps(a, b).Unswept; u != 2 {
		t.Errorf("unswept = %d, want 2", u)
	}
	// A second sweep that never ran says nothing about the files past the cap.
	if u := rcMergeSweeps(a, rcSweepResult{}).Unswept; u != 2 {
		t.Errorf("merged with a sweep that never ran: unswept = %d, want 2", u)
	}
	if u := rcMergeSweeps(rcSweepResult{}, b).Unswept; u != 2 {
		t.Errorf("first sweep never ran: unswept = %d, want 2", u)
	}

	rcRunStart(rcRisk{}, rcProfileNone, nil)
	rcNoteSweepGaps(rcMergeSweeps(a, b))
	if got := rcPartialReasons(); !slices.Equal(got, []string{rcPartialSweepFailed, rcPartialFilesUnswept}) {
		t.Errorf("reasons = %v", got)
	}
}

// A sweep whose every chunk came back blank read nothing: with partial
// reviews on it is no pass, so the publish gate does not ask it to agree.
// Off, it counts as before.
func TestBlankSweepIsNoPass(t *testing.T) {
	patches := map[string]string{"a.go": "@@ -1 +1 @@\n-a\n+b\n"}
	blank := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return rcTextReply(""), nil
	}}
	for mode, want := range map[string]int{"": 1, "record": 0} {
		t.Setenv("KAI_REVIEW_PARTIAL", mode)
		t.Setenv("KAI_REVIEW_FALLBACK_MODELS", "")
		if got := rcRunSweep(context.Background(), blank, "m", "", []string{"a.go"}, patches).Passes; got != want {
			t.Errorf("KAI_REVIEW_PARTIAL=%q: passes = %d, want %d", mode, got, want)
		}
	}
}

// At the specialists' shared deadline, one still running must not cost the
// review another that already finished. With partial reviews on its answer
// is taken and only the running one is recorded as failed; off, both are
// dropped, as before.
func TestSpecialistDeadlineKeepsFinishedOnes(t *testing.T) {
	old := rcSecondFinderGrace
	rcSecondFinderGrace = 20 * time.Millisecond
	t.Cleanup(func() { rcSecondFinderGrace = old })
	const bullet = "a.go:1 — wrong sum | trigger: any call | evidence: a.go:1"
	runs := func() []rcSpecialistRun {
		slow := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: func() {}, model: "m"}
		done := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: func() {}, model: "m"}
		done.ch <- rcFinderResult{Model: "m", Raw: "review", Issues: []string{bullet}}
		return []rcSpecialistRun{{stage: rcStageSpecLogic, sf: slow}, {stage: rcStageSpecSecurity, sf: done}}
	}
	for mode, wantKept := range map[string]int{"": 0, "record": 1} {
		t.Setenv("KAI_REVIEW_PARTIAL", mode)
		rcRunStart(rcRisk{}, rcProfileNone, nil)
		kept, by := rcAwaitSpecialists(runs())
		if len(kept) != wantKept {
			t.Errorf("KAI_REVIEW_PARTIAL=%q: kept %v, want %d", mode, kept, wantKept)
		}
		if got := by[rcFinderKeyOnly(bullet)]; len(kept) > 0 && !slices.Equal(got, []string{rcStageSpecSecurity}) {
			t.Errorf("KAI_REVIEW_PARTIAL=%q: proposers = %v, want the specialist that finished", mode, got)
		}
		if mode == "record" {
			if got := rcPartialReasons(); !slices.Equal(got, []string{rcFinderFailed(rcStageSpecLogic)}) {
				t.Errorf("reasons = %v, want only the running specialist failed", got)
			}
		}
	}
}

// rcTestSet is a model set as the server sends it: each stage's model, the
// backups and the summarising model.
type rcTestSet struct {
	name    string
	stages  map[string]string
	backups []string
	summary string
}

func (s rcTestSet) allowed() map[string]bool {
	out := map[string]bool{s.summary: true}
	for _, m := range s.stages {
		out[m] = true
	}
	for _, m := range s.backups {
		out[m] = true
	}
	return out
}

func rcSetStages(m string, over map[string]string) map[string]string {
	out := map[string]string{}
	for _, st := range []string{rcStageIntent, rcStageMain, rcStageSweep, rcStageFactcheck, rcStageConclusion,
		rcStageFinder2, rcStageSweep2, rcStageRank, rcStageHumanize} {
		out[st] = m
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// The sets the fault suite runs, after kai-server's review sets.
var (
	rcEngineBackups = []string{"z-ai/glm-5.2", "moonshotai/kimi-k2.6", "claude-opus-4-8"}
	rcTestSets      = []rcTestSet{
		{name: "baseline", stages: rcSetStages("z-ai/glm-5.2", nil), backups: rcEngineBackups, summary: "anthropic/claude-haiku-4.5"},
		{name: "free", stages: rcSetStages("google/gemini-3.5-flash", nil),
			backups: []string{"google/gemini-3.7-flash"}, summary: "google/gemini-3.1-flash-lite"},
		{name: "standard", stages: rcSetStages("anthropic/claude-sonnet-5.5", map[string]string{
			rcStageFactcheck: "deepseek-ai/DeepSeek-V4.1-Flash-1017", rcStageFinder2: "Qwen/Qwen3.8-27B-FP8",
			rcStageSweep2: "deepseek-ai/DeepSeek-V4.1-Flash-1017", rcStageRank: "anthropic/claude-opus-5.5",
		}), backups: rcEngineBackups, summary: "anthropic/claude-haiku-4.5"},
	}
)

// rcRunOnSet runs one grounded review on set, with partial reviews recorded,
// and returns the bundle (zero when none was written), the run's error and
// the fake server.
func rcRunOnSet(t *testing.T, set rcTestSet, sc rcFakeScenario) (rcFakeBundle, string, error, *rcFakeModelServer) {
	t.Helper()
	f := newRcFakeModelServer(t, sc)
	f.root = rcFakeRepo(t)
	rcFakeReviewEnv(t, f, set.stages[rcStageMain], set.stages)
	t.Setenv("KAI_REVIEW_FALLBACK_MODELS", strings.Join(set.backups, ","))
	t.Setenv("KAI_REVIEW_SUMMARY_MODEL", set.summary)
	t.Setenv("KAI_REVIEW_PARTIAL", "record")
	out, err := rcFakeRunReview(t)
	var b rcFakeBundle
	if strings.TrimSpace(out) != "" {
		if jerr := json.Unmarshal([]byte(out), &b); jerr != nil {
			t.Fatalf("bundle: %v\n%s", jerr, out)
		}
	}
	return b, out, err, f
}

func rcAssertInSet(t *testing.T, set rcTestSet, f *rcFakeModelServer) {
	t.Helper()
	allowed := set.allowed()
	for m := range f.Models() {
		if !allowed[m] {
			t.Errorf("the review asked for %q, outside the %s set", m, set.name)
		}
	}
}

func rcReasons(b rcFakeBundle) []string {
	if b.PartialReasons == nil {
		return nil
	}
	return *b.PartialReasons
}

// The Free set with its main model failing on every call only ever asks
// Google models, the summarising call included, and still finds the bugs.
func TestFreeSetWithAFailingGeminiStaysOnGoogle(t *testing.T) {
	set := rcTestSets[1]
	primary := set.stages[rcStageMain]
	b, out, err, f := rcRunOnSet(t, set, rcFakeScenario{Main: "review", Summarise: true,
		Fault: func(_, model string, _ *rand.Rand) string {
			if model == primary {
				return rcFaultEmpty
			}
			return rcFaultNone
		}})
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	for m := range f.Models() {
		if !strings.HasPrefix(m, "google/") {
			t.Errorf("asked for %q, not a Google model", m)
		}
	}
	rcAssertInSet(t, set, f)
	summarised := false
	for _, c := range f.Calls() {
		if c.Step == rcFakeSummary {
			summarised = true
			if c.Model != set.summary {
				t.Errorf("summarised on %q, want %q", c.Model, set.summary)
			}
		}
		if c.Step == rcFakeUnknown {
			t.Errorf("a request the fake could not place: %+v", c)
		}
	}
	if b.Run == nil {
		t.Fatalf("the bundle has no run report:\n%s", out)
	}
	if !summarised || b.Run.SummaryModel != set.summary {
		t.Errorf("no summarising call on %s (summary_model %q)", set.summary, b.Run.SummaryModel)
	}
	if !b.FindsTheBugs() {
		t.Errorf("the backup did not finish the review: %s", out)
	}
	if len(b.Run.Failovers) == 0 {
		t.Error("no failover recorded")
	}
	for _, fo := range b.Run.Failovers {
		if fo.From != primary || fo.To != set.backups[0] {
			t.Errorf("failover %+v, want %s → %s", fo, primary, set.backups[0])
		}
	}
	if b.Partial == nil || *b.Partial || !slices.Contains(rcReasons(b), rcPartialFailover) {
		t.Errorf("partial=%v reasons=%v; want not partial, with the failover recorded", b.Partial, rcReasons(b))
	}
}

// A review that read the change and ran out of turns before writing it down
// concludes from real evidence: not partial.
func TestReviewThatRanOutOfTurnsIsNotPartial(t *testing.T) {
	b, out, err, _ := rcRunOnSet(t, rcTestSets[0], rcFakeScenario{Main: "loop"})
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if b.Partial == nil || *b.Partial || slices.Contains(rcReasons(b), rcPartialThinConclusion) {
		t.Errorf("partial=%v reasons=%v; a run that read the code and ran out of turns is not partial", b.Partial, rcReasons(b))
	}
	if b.Incomplete || !b.FindsTheBugs() {
		t.Errorf("incomplete=%v finds=%v\n%s", b.Incomplete, b.FindsTheBugs(), out)
	}
}

// A finder that never answered leaves a conclusion written from nothing:
// partial, however clean the conclusion looks.
func TestConclusionFromAnEmptyRunIsPartial(t *testing.T) {
	b, out, _, _ := rcRunOnSet(t, rcTestSets[0], rcFakeScenario{Main: "dry"})
	if b.Partial == nil || (!*b.Partial && !b.Incomplete) || !slices.Contains(rcReasons(b), rcPartialThinConclusion) {
		t.Errorf("partial=%v incomplete=%v reasons=%v\n%s", b.Partial, b.Incomplete, rcReasons(b), out)
	}
}

// No finder wrote a verdict and the conclusion is stray tool-call text: the
// stray text is never published, the sweep's checked findings are, and the
// review says it did not finish.
func TestHollowReviewPublishesTheCheckedSweep(t *testing.T) {
	b, out, err, _ := rcRunOnSet(t, rcTestSets[0], rcFakeScenario{Main: "prose", Conclusion: "stray"})
	if !errors.Is(err, rcErrIncompleteReview) {
		t.Errorf("err = %v, want the incomplete-review exit", err)
	}
	if strings.Contains(b.Review, "tool_call") || strings.Contains(b.Review, "arg_value") {
		t.Errorf("stray tool-call text published:\n%s", b.Review)
	}
	if !b.Incomplete || !b.FindsTheBugs() {
		t.Errorf("incomplete=%v finds=%v; want the checked sweep findings under an incomplete review\n%s", b.Incomplete, b.FindsTheBugs(), out)
	}
	if b.Partial == nil || !slices.Contains(rcReasons(b), rcPartialNoCoda) {
		t.Errorf("reasons = %v, want %s", rcReasons(b), rcPartialNoCoda)
	}
}

// The second finder standing in for a main finder that wrote nothing is
// partial.
func TestSecondFinderStandingInIsPartial(t *testing.T) {
	set := rcTestSets[2]
	b, out, err, _ := rcRunOnSet(t, set, rcFakeScenario{Main: "review", Conclusion: "empty",
		AgentFor: map[string]string{set.stages[rcStageMain]: "prose"}})
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if b.Partial == nil || !*b.Partial || !slices.Contains(rcReasons(b), rcPartialFinder2StoodIn) {
		t.Errorf("partial=%v reasons=%v\n%s", b.Partial, rcReasons(b), out)
	}
}

// rcFindingLocations is the path:line of each published finding, sorted.
func rcFindingLocations(b rcFakeBundle) string {
	var locs []string
	for _, r := range b.Intent.Risks {
		if loc := rcSweepLocation(r); loc != "" {
			locs = append(locs, loc)
		}
	}
	sort.Strings(locs)
	return strings.Join(locs, ",")
}

// Fault injection: every step of every set, failing 10% or 30% of the time
// with an empty reply or a 5xx, and every time with an empty reply. Every
// bundle is either correct and complete — not partial, not incomplete, and
// publishing exactly what the same set publishes with no fault (which finds
// the seeded bugs) — or says it is partial or incomplete; a run that fails
// with no bundle says nothing clean either. Never clean and wrong. And no
// request ever leaves the set.
//
// "What the same set publishes" rather than "both bugs": a set with two
// sweeps and a selection step publishes only what more than one proposal
// raised (review_commit_publish.go), and the fake's finders word every
// proposal identically, so that set publishes one of the two.
func TestFaultInjectionEveryBundleIsCompleteOrMarked(t *testing.T) {
	steps := []string{rcFakeIntent, rcFakeAgent, rcFakeConclusion, rcFakeSweep, rcFakeFactcheck,
		rcFakeRank, rcFakeHumanize, rcFakeGroup, rcFakeDescription}
	type fault struct {
		kind string
		rate float64
	}
	// The whole matrix takes minutes, so by default every set and step gets
	// the two faults that reach furthest: a step that never answers, and a
	// 5xx on 30% of calls. KAI_FAULT_SUITE=full runs every rate on every set.
	faults := []fault{{rcFaultEmpty, 1}, {rcFault5xx, 0.3}}
	sets := rcTestSets
	full := os.Getenv("KAI_FAULT_SUITE") == "full"
	switch {
	case full:
		faults = []fault{{rcFaultEmpty, 0.1}, {rcFaultEmpty, 0.3}, {rcFaultEmpty, 1}, {rcFault5xx, 0.1}, {rcFault5xx, 0.3}}
	case testing.Short():
		sets, faults = rcTestSets[1:2], []fault{{rcFaultEmpty, 1}}
	}
	tally := map[string]int{}
	var mu sync.Mutex
	for _, set := range sets {
		var want string
		t.Run(set.name+"/no_fault", func(t *testing.T) {
			b, out, err, f := rcRunOnSet(t, set, rcFakeScenario{Main: "review"})
			rcAssertInSet(t, set, f)
			if err != nil || b.Incomplete || b.Partial == nil || *b.Partial || len(rcReasons(b)) != 0 {
				t.Fatalf("a review with no fault: err=%v incomplete=%v partial=%v reasons=%v\n%s", err, b.Incomplete, b.Partial, rcReasons(b), out)
			}
			if want = rcFindingLocations(b); !strings.Contains(want, "calc/calc.go:5") {
				t.Fatalf("a review with no fault published %q, not the seeded bug", want)
			}
		})
		if want == "" {
			continue
		}
		for _, step := range steps {
			for _, fl := range faults {
				name := fmt.Sprintf("%s/%s/%s@%.0f%%", set.name, step, fl.kind, fl.rate*100)
				// A finder or conclusion that never answers walks every
				// retry and backup the engine has, 15-20s a run; outside
				// the full suite one set covers it.
				if !full && fl.rate == 1 && (step == rcFakeAgent || step == rcFakeConclusion) && set.name != "free" {
					continue
				}
				t.Run(name, func(t *testing.T) {
					main := "review"
					if step == rcFakeConclusion {
						main = "loop" // only a run with no verdict asks for one
					}
					// Each case draws its own faults: one seed for all of
					// them would fail every step on the same calls.
					h := fnv.New64a()
					h.Write([]byte(name))
					sc := rcFakeScenario{Main: main, Seed: int64(h.Sum64()), Fault: func(s, _ string, rng *rand.Rand) string {
						if s == step && rng.Float64() < fl.rate {
							return fl.kind
						}
						return rcFaultNone
					}}
					b, out, err, f := rcRunOnSet(t, set, sc)
					rcAssertInSet(t, set, f)
					injected := 0
					for _, c := range f.Calls() {
						if c.Fault != "" {
							injected++
						}
					}
					clean := out != "" && !b.Incomplete && b.Partial != nil && !*b.Partial
					outcome := "complete"
					switch {
					case out == "":
						outcome = "failed, no bundle"
						if err == nil {
							t.Errorf("no bundle and no error")
						}
					case b.Incomplete:
						outcome = "incomplete"
					case b.Partial != nil && *b.Partial:
						outcome = "partial"
					}
					if clean && rcFindingLocations(b) != want {
						t.Errorf("clean and wrong: published %q where a run with no fault publishes %q; %d fault(s) injected, reasons %v\n%s",
							rcFindingLocations(b), want, injected, rcReasons(b), out)
					}
					if clean && err != nil {
						t.Errorf("a clean bundle with an error: %v", err)
					}
					mu.Lock()
					tally[outcome]++
					mu.Unlock()
					t.Logf("%d fault(s) injected → %s %v", injected, outcome, rcReasons(b))
				})
			}
		}
	}
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%s: %d", k, tally[k])
	}
}

func TestDescriptionFailed(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		asked, incomplete, fast bool
		description             string
		want                    bool
	}{
		{name: "grounded pass with none", asked: true, want: true},
		{name: "grounded pass with one", asked: true, description: "d"},
		{name: "fast pass not ready in its short wait", asked: true, fast: true},
		{name: "incomplete review never collects one", asked: true, incomplete: true},
		{name: "never asked", fast: false},
	} {
		if got := rcDescriptionFailed(tc.asked, tc.incomplete, tc.fast, tc.description); got != tc.want {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
}
