package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"kai/internal/config"
	"kai/internal/freeplan"

	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/remote"
)

// fakeFreeDefaults has the shape of the server's per-role defaults, with
// placeholder ids.
var fakeFreeDefaults = map[string]string{
	"chat": "free/model-fast", "robot": "free/model-strong",
	"planner": "free/model-strong", "coder": "free/model-strong",
	"agent": "free/model-strong", "jeff": "free/model-fast",
	"images": "free/model-fast", "titles": "free/model-small",
}

// newFakeKaiServer serves the model list: with free and defaults only when
// free, as the server sends them to a caller on the Free plan.
func newFakeKaiServer(t *testing.T, free bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"data": []map[string]string{{"id": "anthropic/claude-opus-5"}}}
		if free {
			out = map[string]any{"data": []map[string]string{{"id": "free/model-fast"}, {"id": "free/model-strong"}},
				"free": true, "defaults": fakeFreeDefaults}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signInTo stores credentials for srv in a fresh HOME and answers the plan
// from it, as the real cliFreePlan would. Every KAI_*_MODEL(S) variable and
// role flag is cleared, so a developer's or CI's shell can't change the
// outcome.
func signInTo(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "KAI_") && (strings.HasSuffix(name, "_MODEL") || strings.HasSuffix(name, "_MODELS")) {
			t.Setenv(name, "")
		}
	}
	for _, k := range []string{"KAI_PROVIDER", "KAI_AGENT_MODEL", "KAI_REVIEW_SET_VERSION"} {
		t.Setenv(k, "")
	}
	prevPlanner, prevAgent := PlannerModelFlag, AgentModelFlag
	PlannerModelFlag, AgentModelFlag = "", ""
	if err := remote.SaveCredentials(&remote.Credentials{AccessToken: "tok", ServerURL: srv.URL,
		ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	prev := cliFreePlan
	cliFreePlan = func(kind provider.Kind) freeplan.Plan {
		if kind != provider.KindKailab {
			return freeplan.Plan{}
		}
		return freeplan.Fetch(context.Background(), srv.URL, "tok")
	}
	t.Cleanup(func() {
		cliFreePlan = prev
		activeFreePlan = freeplan.Plan{}
		PlannerModelFlag, AgentModelFlag = prevPlanner, prevAgent
	})
}

// No free key in the list: no role changes, a model the person set runs as
// before, and the commit review asks for the same models as before.
func TestFreePlan_NoFreeKeyChangesNothing(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, false))
	t.Setenv("KAI_AGENT_MODEL", "anthropic/claude-opus-5")
	cfg := config.Default()
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err != nil || !reflect.DeepEqual(cfg, config.Default()) {
		t.Fatalf("changed without a free key: %v %+v", err, cfg)
	}
	t.Setenv("KAI_AGENT_MODEL", "")
	prov, review, fix, err := buildGateProvider(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if d := config.Default(); review != d.Review.Model || fix != d.Agent.Model {
		t.Fatalf("review %s, fix %s", review, fix)
	}
	if _, ok := prov.(*provider.Kailab); !ok {
		t.Fatalf("provider is %T", prov)
	}
	if m := consultModelFromEnv(); m != "claude-opus-4-7" {
		t.Errorf("consult: %s", m)
	}
	if m := rcFastModel("z-ai/glm-5.2", provider.KindKailab); m != rcFastDefaultModel {
		t.Errorf("fast pass: %s", m)
	}
	if m := rcEnsembleModel(rcStageRank); m != "anthropic/claude-opus-5.5" {
		t.Errorf("rank: %s", m)
	}
}

// Free: every role nobody set takes the server's model, silently.
func TestFreePlan_UnsetRolesTakeServerDefaults(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	cfg := config.Default()
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, role string }{
		{"executor", cfg.Agent.Model, "coder"}, {"planner", cfg.Planner.Model, "planner"}, {"chat", cfg.Chat.Model, "chat"},
		{"classifier", cfg.Classifier.Model, freeplan.CatchAll}, {"review", cfg.Review.Model, freeplan.CatchAll},
	} {
		if c.got != fakeFreeDefaults[c.role] {
			t.Errorf("%s runs %s, want the server's %s model", c.name, c.got, c.role)
		}
	}
}

// Free with a model the person set that the plan doesn't include
// (KAI_AGENT_MODEL=anthropic/claude-opus-5): the command stops with one
// sentence, and the model is neither run nor replaced.
func TestFreePlan_SetModelNotOnThePlanStops(t *testing.T) {
	srv := newFakeKaiServer(t, true)
	signInTo(t, srv)
	t.Setenv("KAI_AGENT_MODEL", "anthropic/claude-opus-5")
	cfg := config.Default()
	err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab))
	want := "anthropic/claude-opus-5 isn't available on the Free plan. Choose another model, or upgrade: " + srv.URL + "/billing."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if !reflect.DeepEqual(cfg, config.Default()) {
		t.Errorf("roles changed on a stop: %+v", cfg)
	}
	if _, _, _, err := buildGateProvider(config.Default()); err == nil || err.Error() != want {
		t.Errorf("commit review didn't stop: %v", err)
	}

	// A flag works the same way.
	t.Setenv("KAI_AGENT_MODEL", "")
	PlannerModelFlag = "deepseek/deepseek-v4-flash"
	cfg = config.Default()
	cfg.Planner.Model = PlannerModelFlag // as buildPlannerServices applies it
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err == nil || !strings.HasPrefix(err.Error(), "deepseek/deepseek-v4-flash isn't available on the Free plan.") {
		t.Errorf("flag: %v", err)
	}
}

// A model the person set that the plan includes runs; variables that name
// no Kai model, and values that turn a role off, don't count.
func TestFreePlan_SetModelOnThePlanRuns(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	t.Setenv("KAI_AGENT_MODEL", "free/model-fast")
	t.Setenv("KAI_OPENAI_MODEL", "local/model")
	t.Setenv("KAI_CONSULT_MODEL", "disabled")
	cfg := config.Default()
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err != nil {
		t.Fatal(err)
	}
	if got := modelFromEnv("KAI_AGENT_MODEL", cfg.Agent.Model); got != "free/model-fast" {
		t.Errorf("the person's model lost: %s", got)
	}
	// cfg carries it too, for readers of cfg alone.
	if cfg.Agent.Model != "free/model-fast" {
		t.Errorf("cfg.Agent.Model = %s, want the person's model", cfg.Agent.Model)
	}
	if cfg.Chat.Model != fakeFreeDefaults["chat"] {
		t.Errorf("an unset role kept %s", cfg.Chat.Model)
	}
}

// A model set in .kai/config.yaml is the person's: one the plan doesn't
// include stops the command. A config value equal to the built-in default
// counts as unset and takes the server's model (plan session, 2026-11-16).
func TestFreePlan_ConfigModels(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	load := func(body string) config.Config {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/config.yaml", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := load("autonomy: hands_off\nreview:\n  model: anthropic/claude-opus-5.5\n")
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err == nil || !strings.HasPrefix(err.Error(), "anthropic/claude-opus-5.5 isn't available on the Free plan.") {
		t.Errorf("a config model the plan lacks: %v", err)
	}
	cfg = load("review:\n  model: " + config.Default().Review.Model + "\n")
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err != nil {
		t.Fatalf("a config value equal to the default stopped the command: %v", err)
	}
	if cfg.Review.Model != fakeFreeDefaults[freeplan.CatchAll] {
		t.Errorf("a config value equal to the default kept %s", cfg.Review.Model)
	}
}

// Reviewing a commit on Free: the review's model is the server's, and so is
// every stage on its own default (the fast pass doesn't swap in another
// model, the ensemble stages don't run their built-in ones); the consult
// tool does the same. The provider stays Kai's own, so the run's cost
// upload still finds it. A review whose models were chosen for it (the
// server sent a set) keeps them and isn't stopped.
func TestFreePlan_CommitReview(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	prov, review, fix, err := buildGateProvider(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	strong := fakeFreeDefaults[freeplan.CatchAll]
	if review != strong || fix != fakeFreeDefaults["coder"] {
		t.Fatalf("review %s, fix %s", review, fix)
	}
	if _, ok := prov.(*provider.Kailab); !ok {
		t.Fatalf("provider is %T, so the engine's cost upload would skip it", prov)
	}
	if m := rcFastModel("z-ai/glm-5.2", provider.KindKailab); m != "z-ai/glm-5.2" {
		t.Errorf("fast pass swapped in %s", m)
	}
	for _, st := range []string{rcStageFinder2, rcStageSweep2, rcStageRank, rcStageHumanize} {
		if m := rcEnsembleModel(st); m != strong {
			t.Errorf("%s runs %s", st, m)
		}
	}
	if m := rcEnsembleModel(rcStageSpecLogic); m != "" {
		t.Errorf("an off stage came on: %s", m)
	}
	if m := consultModelFromEnv(); m != strong {
		t.Errorf("consult: %s", m)
	}
}

// The commit review's provider shows the Free stop as it is, not as a
// missing provider ("run `kai login`").
func TestFreePlan_CommitReviewStopShown(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	prev := kaiDir
	kaiDir = t.TempDir()
	t.Cleanup(func() { kaiDir = prev })
	t.Setenv("KAI_REVIEW_MODEL", "anthropic/claude-opus-5")
	prov, _, _, err := rcReviewProvider()
	if prov != nil || err == nil || !strings.HasPrefix(err.Error(), "anthropic/claude-opus-5 isn't available on the Free plan.") {
		t.Fatalf("provider %v, error %v", prov, err)
	}
}

// A review whose models the server chose (KAI_REVIEW_SET_VERSION) keeps
// them on Free: the plan isn't applied, so the review model and a stage
// on its own default stay the built-in ones, not the plan's.
func TestFreePlan_ServerChosenReviewKeepsItsSet(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	t.Setenv("KAI_REVIEW_SET_VERSION", "3")
	builtinRank := rcEnsembleDefaults[rcStageRank].model
	if builtinRank == fakeFreeDefaults[freeplan.CatchAll] {
		t.Fatal("the fake plan's model equals the built-in rank model; the test can't tell them apart")
	}
	_, review, _, err := buildGateProvider(config.Default())
	if err != nil {
		t.Fatalf("a server-chosen review was stopped: %v", err)
	}
	if review != config.Default().Review.Model {
		t.Errorf("a server-chosen review lost its model: %s", review)
	}
	if m := rcEnsembleModel(rcStageRank); m != builtinRank {
		t.Errorf("a server-chosen review's rank stage changed to %s", m)
	}
}

// A flag counts only where the command applied it to cfg: the headless
// agent doesn't read --planner-model, so there the built-in default isn't
// the person's and takes the server's model.
func TestFreePlan_FlagNotAppliedDoesntCount(t *testing.T) {
	signInTo(t, newFakeKaiServer(t, true))
	PlannerModelFlag = "free/model-fast"
	cfg := config.Default() // the flag was not applied
	if err := applyFreePlan(&cfg, cliFreePlan(provider.KindKailab)); err != nil {
		t.Fatalf("stopped for a model nobody named: %v", err)
	}
	if cfg.Planner.Model != fakeFreeDefaults["planner"] {
		t.Errorf("planner runs %s", cfg.Planner.Model)
	}
}
