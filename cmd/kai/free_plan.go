package main

// Free plan models. When the server's model list says the caller is on the
// Free plan, every role the person didn't set runs the server's model for
// it, and a model they did set (env, flag or .kai/config.yaml) that the
// Free plan doesn't include stops the command with one sentence saying so.
// Without a free key in the list nothing here changes a model.

import (
	"os"
	"sort"
	"strings"

	"kai/internal/config"
	"kai/internal/freeplan"

	"github.com/kaicontext/kai-engine/modelplan"
	"github.com/kaicontext/kai-engine/provider"
)

// cliFreePlan is the caller's plan for a run on provider kind, for Kai's
// own provider only: a provider the person brought isn't on a Kai plan.
// It also hands that plan to the engine (modelplan.Set), so the models the
// engine picks on its own during the run follow it too; with the person's
// own provider the engine gets the zero plan and keeps its own models. A
// variable so tests can answer for the server.
var cliFreePlan = func(kind provider.Kind) freeplan.Plan {
	if kind != provider.KindKailab {
		modelplan.Set(modelplan.Plan{})
		return freeplan.Plan{}
	}
	p := cliHostedPlan()
	modelplan.Set(p.Engine())
	return p
}

// cliHostedPlan is the caller's plan on Kai's own provider, asked of the
// server once per process. A variable so tests can answer for the server.
var cliHostedPlan = func() freeplan.Plan {
	base, token := kailabCreds()
	return freeplan.Current(base, token)
}

// freeRole is one of the config's role models: its config section, its
// env override and the server role whose default it takes on Free.
type freeRole struct {
	key        string // the role's section in .kai/config.yaml
	builtin    string // its built-in default model
	model      *string
	env        string
	serverRole string
}

func freeRoles(cfg *config.Config) []freeRole {
	d := config.Default()
	return []freeRole{
		{"agent", d.Agent.Model, &cfg.Agent.Model, "KAI_AGENT_MODEL", "coder"},
		{"planner", d.Planner.Model, &cfg.Planner.Model, "KAI_PLANNER_MODEL", "planner"},
		{"chat", d.Chat.Model, &cfg.Chat.Model, "KAI_CHAT_MODEL", "chat"},
		{"classifier", d.Classifier.Model, &cfg.Classifier.Model, "KAI_CLASSIFIER_MODEL", freeplan.CatchAll},
		{"review", d.Review.Model, &cfg.Review.Model, "KAI_REVIEW_MODEL", freeplan.CatchAll},
	}
}

// activeFreePlan is the plan applyFreePlan last applied on Free, for the
// models chosen outside the config's roles (freeDefault). Zero off Free.
var activeFreePlan freeplan.Plan

// freeDefault is the server's model for role when this command runs on
// Free, else "": for the models a command chooses outside the config's
// roles (a review stage's own default, the critic, the consult tool).
func freeDefault(role string) string {
	return activeFreePlan.Default(role)
}

// applyFreePlan sets every role model in cfg the person didn't set to the
// plan's model for it. A model they did set that the Free plan doesn't
// include is not run and not replaced: the error says so, and the command
// stops. Role flags must already be in cfg. Off Free, cfg is left as it is.
func applyFreePlan(cfg *config.Config, plan freeplan.Plan) error {
	if !plan.Free {
		return nil
	}
	if err := plan.Check(explicitModels(cfg)); err != nil {
		return freeStopError{err}
	}
	activeFreePlan = plan
	for _, r := range freeRoles(cfg) {
		if v := strings.TrimSpace(os.Getenv(r.env)); v != "" {
			// The person's own choice, written into cfg so a reader of
			// cfg (the planner's single-shot call) runs it too.
			*r.model = v
			continue
		}
		if roleSetInConfig(cfg, r) {
			continue // the person's own choice
		}
		*r.model = plan.Resolve(r.serverRole, "", *r.model)
	}
	return nil
}

// freeStopError is applyFreePlan's stop, marked so a caller that reports
// its other provider errors in its own words (rcReviewProvider) can show
// this one as it is.
type freeStopError struct{ error }

func (e freeStopError) Unwrap() error { return e.error }

// notKaiModelVars are KAI_*_MODEL variables that don't name a model Kai's
// server is asked for: only a provider the person brought reads them.
var notKaiModelVars = map[string]bool{"KAI_OPENAI_MODEL": true, "KAI_ANTHROPIC_MODEL": true}

// explicitModels is every model the person named, sorted: a KAI_*_MODEL(S)
// variable (comma lists split) and a role model set by a flag or
// .kai/config.yaml (roleSetInConfig).
func explicitModels(cfg *config.Config) []string {
	set := map[string]bool{}
	for _, kv := range os.Environ() {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || notKaiModelVars[name] || !strings.HasPrefix(name, "KAI_") || !(strings.HasSuffix(name, "_MODEL") || strings.HasSuffix(name, "_MODELS")) {
			continue
		}
		for _, m := range strings.Split(val, ",") {
			if m = strings.TrimSpace(m); m != "" && !offModel[m] {
				set[m] = true
			}
		}
	}
	for _, r := range freeRoles(cfg) {
		if m := strings.TrimSpace(*r.model); m != "" && roleSetInConfig(cfg, r) {
			set[m] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// offModel are the values that turn a role off rather than name a model
// (KAI_CONSULT_MODEL=disabled, a review stage's "off").
var offModel = map[string]bool{"off": true, "none": true, "disabled": true}

// roleSetInConfig reports whether cfg's model for r is the person's: set
// in .kai/config.yaml to something other than the built-in default (a
// config value equal to the default counts as unset; plan session,
// 2026-11-16), or by its flag, where the command applied the flag to cfg.
func roleSetInConfig(cfg *config.Config, r freeRole) bool {
	if cfg.ModelsSet[r.key] && strings.TrimSpace(*r.model) != r.builtin {
		return true
	}
	// A flag counts where the command applied it to cfg (the TUI does;
	// the headless paths don't read these flags at all).
	flag := ""
	switch r.key {
	case "planner":
		flag = PlannerModelFlag
	case "agent":
		flag = AgentModelFlag
	}
	flag = strings.TrimSpace(flag)
	return flag != "" && strings.TrimSpace(*r.model) == flag
}
