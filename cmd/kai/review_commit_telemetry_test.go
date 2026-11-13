package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// The rest of the review bundle's telemetry: each finding's origin_stage and
// the run object's version, source, routing, per-stage timing and the stage
// an unfinished review stopped in.

// rcSpecialistDone is a finished specialist run handing back issues.
func rcSpecialistDone(stage string, issues ...string) rcSpecialistRun {
	sf := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: func() {}}
	sf.ch <- rcFinderResult{Issues: issues}
	return rcSpecialistRun{stage: stage, sf: sf}
}

// A specialist's own bullet comes from that specialist; a specialist bullet
// repeating the main finder's credits the specialists on the main bullet, but
// the main finder came first. Neither changes Proposers.
func TestOriginStage_SpecialistsAreOriginsNotProposers(t *testing.T) {
	const repeat = "a.go:10 — dereferences the session user after expiry without a nil check | trigger: an expired session | evidence: a.go:10 | consequence: a panic"
	const own = "c.go:4 — the retry loop never stops | trigger: a server that always fails | evidence: c.go:4 | consequence: a hung job"
	issues, by := rcAwaitSpecialists([]rcSpecialistRun{
		rcSpecialistDone(rcStageSpecLogic, repeat, own),
		rcSpecialistDone(rcStageSpecSecurity, repeat, "d.go:1 — no trigger or evidence: dropped by the contract"),
	})
	if len(issues) != 3 {
		t.Fatalf("issues = %q, want the three contract bullets", issues)
	}
	if got := by[rcFinderKeyOnly(repeat)]; !reflect.DeepEqual(got, []string{rcStageSpecLogic, rcStageSpecSecurity}) {
		t.Errorf("repeated bullet's specialists = %q", got)
	}

	l := newRcFinderLedger()
	l.addAll(rcIssuesOf(findersDraft), rcFinderMain)
	merged := rcWithSpecialists(findersDraft, issues, by, l)
	bullets := rcIssuesOf(merged)
	if len(bullets) != 2 {
		t.Fatalf("draft issues = %q, want the main bullet and the specialists' new one", bullets)
	}
	if got := l.of(bullets[0]); !reflect.DeepEqual(got, []string{rcFinderMain}) {
		t.Errorf("main bullet's proposers = %q, want main only", got)
	}
	if got := l.stagesOf(bullets[0]); !reflect.DeepEqual(got, []string{rcFinderMain, rcStageSpecLogic, rcStageSpecSecurity}) {
		t.Errorf("main bullet's stages = %q, want main, spec_logic, spec_security", got)
	}
	if got := l.of(bullets[1]); got != nil {
		t.Errorf("specialist bullet's proposers = %q, want none", got)
	}
	if got := l.stagesOf(bullets[1]); !reflect.DeepEqual(got, []string{rcStageSpecLogic}) {
		t.Errorf("specialist bullet's stages = %q", got)
	}

	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: bullets[0], Status: rcStatusSupported},
		{ID: 2, Issue: bullets[1], Status: rcStatusSupported},
		{ID: 3, Issue: "z.go:9 — a wording no finder proposed", Status: rcStatusSupported},
	}}
	rcRecordProposers(res, l)
	rcCountVotes(res)
	want := map[int]string{1: rcFinderMain, 2: rcStageSpecLogic, 3: ""}
	for _, a := range res.Allegations {
		if a.OriginStage != want[a.ID] {
			t.Errorf("allegation %d origin_stage = %q, want %q", a.ID, a.OriginStage, want[a.ID])
		}
	}
	b, _ := json.Marshal(res.Allegations[2])
	if strings.Contains(string(b), "origin_stage") {
		t.Errorf("an unknown origin was written: %s", b)
	}
}

// The origin is the first stage behind the whole vote group, and recording
// it changes neither the gate's decisions nor Proposers.
func TestOriginStage_OverTheVoteGroup(t *testing.T) {
	plain := gateFixture()
	pl := newRcFinderLedger()
	pl.add("a.go:11 — raised three times", rcFinderSweep2)
	pl.add("a.go:11 — the same, reworded", rcFinderSecond)
	pl.add("a.go:12 — raised once", rcFinderSweep)
	rcRecordProposers(plain, pl)
	rcPublishGate(plain, gateRepo(), true)

	res := gateFixture()
	l := newRcFinderLedger()
	l.add("a.go:11 — raised three times", rcFinderSweep2)
	l.add("a.go:11 — the same, reworded", rcFinderSecond)
	l.add("a.go:12 — raised once", rcFinderSweep)
	l.specialists().add("a.go:12 — raised once", rcStageSpecContracts)
	rcRecordProposers(res, l)
	rcPublishGate(res, gateRepo(), true)
	want := map[int]string{1: rcFinderSecond, 2: rcFinderSecond, 3: rcFinderSecond, 4: rcStageSpecContracts, 5: "", 6: ""}
	for i, a := range res.Allegations {
		p := plain.Allegations[i]
		if a.Status != p.Status || a.Reason != p.Reason || a.Votes != p.Votes || !reflect.DeepEqual(a.Proposers, p.Proposers) {
			t.Errorf("allegation %d changed with specialists recorded: %+v vs %+v", a.ID, a, p)
		}
		if a.OriginStage != want[a.ID] {
			t.Errorf("allegation %d origin_stage = %q, want %q", a.ID, a.OriginStage, want[a.ID])
		}
	}
}

// Stages sort in the order the review merges their bullets.
func TestOriginStage_Order(t *testing.T) {
	set := map[string]bool{}
	for _, s := range []string{rcFinderSweep2, rcStageSpecContracts, rcFinderSweep, rcStageSpecLogic, rcFinderSecond, rcStageSpecSecurity, rcFinderMain, "zzz"} {
		set[s] = true
	}
	want := []string{rcFinderMain, rcFinderSecond, rcStageSpecLogic, rcStageSpecSecurity, rcStageSpecContracts, rcFinderSweep, rcFinderSweep2, "zzz"}
	if got := rcOriginSorted(set); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %q, want %q", got, want)
	}
}

// rcRunFakeClock makes the recorder's clock advance only when told.
func rcRunFakeClock(t *testing.T) func(time.Duration) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	old := rcRunNow
	rcRunNow = func() time.Time { return now }
	t.Cleanup(func() { rcRunNow = old })
	return func(d time.Duration) { now = now.Add(d) }
}

// Seconds counts the time a stage had a call in flight, overlapping calls
// once and the gap between calls not at all; a call still running counts to
// the snapshot.
func TestRunTelemetry_Seconds(t *testing.T) {
	tick := rcRunFakeClock(t)
	rcRunStart(rcRisk{}, rcProfileNone, nil)
	rcRun.begin(rcStageSweep, "m", "")
	tick(2 * time.Second)
	rcRun.begin(rcStageSweep, "m", "") // a second chunk, overlapping
	tick(3 * time.Second)
	rcRun.end(rcStageSweep, "m", nil)
	tick(1 * time.Second)
	rcRun.end(rcStageSweep, "m", nil) // 6s busy so far
	tick(10 * time.Second)            // idle
	rcRun.begin(rcStageSweep, "m", "")
	tick(1500 * time.Millisecond)
	rcRun.end(rcStageSweep, "m", nil)

	rcRun.begin(rcStageRank, "m", "")
	tick(250 * time.Millisecond)

	run := rcRunSnapshot()
	if got := run.Stages[rcStageSweep].Seconds; got != 7.5 {
		t.Errorf("sweep seconds = %v, want 7.5", got)
	}
	if got := run.Stages[rcStageRank].Seconds; got != 0.3 {
		t.Errorf("rank seconds = %v, want 0.3 (still running, rounded)", got)
	}
	rcRun.end(rcStageRank, "m", nil)
}

// Effort is what the stage's calls asked for, "off" for none; truncations
// count answers cut at the token limit, through rcSend and agent turns.
func TestRunTelemetry_EffortAndTruncations(t *testing.T) {
	rcRunStart(rcRisk{}, rcProfileNone, map[string]string{rcStageSweep: "m/s", rcStageRank: "m/r", rcStageMain: "m/main"})
	f := &fakeModels{reply: func(req provider.Request, n int) (provider.Response, error) {
		if req.Model == "m/s" && n == 1 {
			return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: "cut"}}, FinishReason: message.FinishReasonMaxTokens}, nil
		}
		return rcTextReply("ok"), nil
	}}
	sweep := rcCostStage(context.Background(), rcStageSweep)
	req := provider.Request{Model: "m/s", ReasoningEffort: "high"}
	for i := 0; i < 2; i++ {
		if _, err := rcSend(sweep, f, req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rcSend(rcCostStage(context.Background(), rcStageRank), f, provider.Request{Model: "m/r"}); err != nil {
		t.Fatal(err)
	}
	h := rcRunHooks(rcStageMain, agent.Hooks{})
	h.OnTurnTiming(agent.TurnTiming{Model: "m/main", FinishReason: string(message.FinishReasonMaxTokens)})
	h.OnTurnTiming(agent.TurnTiming{Model: "m/main", FinishReason: string(message.FinishReasonEndTurn)})

	run := rcRunSnapshot()
	if st := run.Stages[rcStageSweep]; st.Effort != "high" || st.Truncations != 1 || st.Calls != 2 {
		t.Errorf("sweep = %+v, want effort high, 1 truncation, 2 calls", st)
	}
	if st := run.Stages[rcStageRank]; st.Effort != rcEffortOff || st.Truncations != 0 {
		t.Errorf("rank = %+v, want effort off, no truncation", st)
	}
	if st := run.Stages[rcStageMain]; st.Truncations != 1 || st.Effort != "" {
		t.Errorf("main = %+v, want 1 truncation and no effort (no call opened)", st)
	}
}

// Retries count the engine's re-sends inside an agent run, not the turn it
// forces past the token budget, and a caller's own hook keeps firing.
func TestRunTelemetry_Retries(t *testing.T) {
	rcRunStart(rcRisk{}, rcProfileNone, nil)
	var callerSaw int
	h := rcRunHooks(rcStageFinder2, agent.Hooks{OnRetryWait: func(int, time.Duration, error) { callerSaw++ }})
	h.OnRetryWait(1, time.Second, errors.New("openrouter provider: 429: slow down"))
	h.OnRetryWait(2, 0, errors.New("empty completion from m; retrying once"))
	h.OnRetryWait(7, 0, errors.New("budget exceeded (used 10 / cap 5) — forcing one final commit-now turn"))
	if st := rcRunSnapshot().Stages[rcStageFinder2]; st.Retries != 2 {
		t.Errorf("finder2 retries = %d, want 2", st.Retries)
	}
	if callerSaw != 3 {
		t.Errorf("the caller's OnRetryWait fired %d times, want 3", callerSaw)
	}
}

// Through the real engine: a transient error is retried, and the retry is
// counted on the agent's stage.
func TestRunTelemetry_RetriesThroughTheEngine(t *testing.T) {
	rcRunStart(rcRisk{}, rcProfileNone, map[string]string{rcStageMain: "m/main"})
	f := &fakeModels{reply: func(_ provider.Request, n int) (provider.Response, error) {
		if n == 1 {
			return provider.Response{}, provider.NewTransientError(errors.New("connection reset"), time.Millisecond)
		}
		return rcTextReply("done"), nil
	}}
	if _, err := rcRunAgent(context.Background(), rcStageMain, agent.Options{
		Workspace: t.TempDir(), Prompt: "review", System: "s", Model: "m/main", Provider: f, ReasoningEffort: "low",
	}); err != nil {
		t.Fatal(err)
	}
	st := rcRunSnapshot().Stages[rcStageMain]
	if st.Retries != 1 || st.Effort != "low" || st.Status != rcRunOK {
		t.Errorf("main = %+v, want one retry, effort low, ok", st)
	}
}

func TestRunTelemetry_Source(t *testing.T) {
	cases := []struct{ runSource, ci, gha, want string }{
		{"benchmark", "true", "true", "benchmark"},
		{"LOCAL", "true", "", "local"},
		{"", "true", "", "ci"},
		{"", "", "true", "ci"},
		{"something", "", "", "local"},
		{"", "", "", "local"},
	}
	for _, c := range cases {
		t.Setenv(provider.RunSourceEnv, c.runSource)
		t.Setenv("CI", c.ci)
		t.Setenv("GITHUB_ACTIONS", c.gha)
		if got := rcRunSource(); got != c.want {
			t.Errorf("KAI_RUN_SOURCE=%q CI=%q GITHUB_ACTIONS=%q: source %q, want %q", c.runSource, c.ci, c.gha, got, c.want)
		}
	}
}

// The profile hash follows the settings, not the file's layout or source.
func TestRunTelemetry_ProfileHash(t *testing.T) {
	old := rcActiveProfile
	t.Cleanup(func() { rcActiveProfile = old })
	rcActiveProfile = nil
	if h := rcProfileHash(); h != "" {
		t.Errorf("no profile hashed to %q", h)
	}
	a, err := rcParseProfile([]byte(`{"run":"x","stages":{"main":{"model":"a/b","effort":"high"},"sweep":{"model":"c/d"}}}`), "one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := rcParseProfile([]byte("{\n \"stages\": {\"sweep\": {\"model\": \"c/d\"},\n \"main\": {\"effort\": \"HIGH\", \"model\": \"a/b\"}}}"), "/some/other/path")
	if err != nil {
		t.Fatal(err)
	}
	c, err := rcParseProfile([]byte(`{"stages":{"main":{"model":"a/b","effort":"low"},"sweep":{"model":"c/d"}}}`), "one")
	if err != nil {
		t.Fatal(err)
	}
	rcActiveProfile = a
	ha := rcProfileHash()
	rcActiveProfile = b
	hb := rcProfileHash()
	rcActiveProfile = c
	hc := rcProfileHash()
	if len(ha) != 16 || ha != hb {
		t.Errorf("same settings hashed %q and %q", ha, hb)
	}
	if hc == ha {
		t.Error("a different effort kept the hash")
	}
}

func TestRunTelemetry_IncompleteStage(t *testing.T) {
	cases := []struct {
		stage string
		fast  bool
		want  string
	}{
		{rcStageNameReview, false, rcStageMain},
		{rcStageNameReview, true, rcStageQuickDraft},
		{rcStageNameChallenge, false, rcStageFactcheck},
		{rcStageNameChallenge, true, rcStageQuickFactcheck},
		{rcStageNameConclusion, false, rcStageConclusion},
	}
	for _, c := range cases {
		if got := rcRunIncompleteStage(&rcIncompleteReason{Stage: c.stage}, c.fast); got != c.want {
			t.Errorf("%s fast=%v: %q, want %q", c.stage, c.fast, got, c.want)
		}
	}
	if got := rcRunIncompleteStage(nil, false); got != "" {
		t.Errorf("a finished review stopped in %q", got)
	}
}

// The new fields are in the run object under their names, and carry only
// fixed vocabularies: no path, model text or prose.
func TestRunTelemetry_JSONShape(t *testing.T) {
	t.Setenv(provider.RunSourceEnv, "benchmark")
	old := Version
	Version = "9.9.9-test"
	t.Cleanup(func() { Version = old })
	rcRunStart(rcRisk{Level: "L", Reasons: []string{"touches auth", "changes dependencies"}}, rcProfileNone, map[string]string{rcStageMain: "m"})
	run := rcRunSnapshot()
	run.IncompleteStage = rcStageFactcheck
	b, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"kai_version":"9.9.9-test"`, `"source":"benchmark"`, `"risk":"L"`, `"risk_reasons":["touches auth","changes dependencies"]`, `"profile_hash":""`, `"incomplete_stage":"factcheck"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("run JSON lacks %s:\n%s", key, b)
		}
	}
	rcRunStart(rcRisk{Level: "S"}, rcProfileNone, nil)
	b, _ = json.Marshal(rcRunSnapshot())
	for _, key := range []string{`"risk_reasons":[]`, `"incomplete_stage":""`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("run JSON lacks %s:\n%s", key, b)
		}
	}
}

// With the publish gate off, rcCountVotes never runs: each finding keeps the
// origin of its own bullet, as it keeps its own Proposers.
func TestOriginStage_PublishGateOff(t *testing.T) {
	t.Setenv("KAI_REVIEW_PUBLISH_GATE", "0")
	res := gateFixture()
	l := newRcFinderLedger()
	l.add("a.go:11 — raised three times", rcFinderSweep2)
	l.add("a.go:11 — the same, reworded", rcFinderSecond)
	l.specialists().add("a.go:12 — raised once", rcStageSpecContracts)
	rcRecordProposers(res, l)
	rcPublishGate(res, gateRepo(), true)
	want := map[int]string{1: rcFinderSweep2, 2: rcFinderSecond, 3: "", 4: rcStageSpecContracts, 5: "", 6: ""}
	for _, a := range res.Allegations {
		if a.OriginStage != want[a.ID] {
			t.Errorf("allegation %d origin_stage = %q, want %q", a.ID, a.OriginStage, want[a.ID])
		}
	}
}
