// Package config loads kai's per-repo configuration from
// <kaiDir>/config.yaml. Currently covers the planner (LLM model, agent
// cap) and the agent runner (command template, timeout) — the bits
// Phase 3 needs. The safety gate has its own loader at
// internal/safetygate/config.go for the same reason: focused configs,
// minimal blast radius when one slice changes.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configFileName lives next to db.sqlite inside the kai data directory.
const configFileName = "config.yaml"

// Config is the merged result of defaults + on-disk overrides.
//
// Model selection is role-based: kai routes each kind of LLM call to
// a model suited to the work. Classifier decides chat-vs-code, the
// planner and chat agent reason/converse, and the agent writes code.
// See Default() for the built-in assignments.
type Config struct {
	Agent      AgentConfig      `yaml:"agent"`
	Planner    PlannerConfig    `yaml:"planner"`
	Chat       ChatConfig       `yaml:"chat"`
	Classifier ClassifierConfig `yaml:"classifier"`
	Review     ReviewConfig     `yaml:"review"`

	// Triage is retained from the triage line. The classifier is the
	// live request router (see internal/tui/views/classify.go); the
	// triage package is kept compiling but unwired pending a decision
	// to remove it.
	Triage TriageConfig `yaml:"triage"`

	// Autonomy selects how much of a `kai code` run proceeds without
	// human input. "hands_off" auto-confirms the plan, auto-accepts
	// file/bash actions, and runs the review→fix loop unattended —
	// the user only does the final gate approve. "yolo" does everything
	// hands_off does AND auto-resolves the gate (approve, or reject when
	// the reviewer says REJECT) — it ships changes the gate held,
	// unresolved judgment calls included, so it's opt-in and deliberately
	// risky. Empty / "off" (default) keeps every confirmation prompt. The
	// `--auto` flag and KAI_AUTO env var force hands_off per run; KAI_YOLO
	// forces yolo.
	Autonomy string `yaml:"autonomy"`
}

// TriageConfig controls the request-triage step that classifies an
// incoming request before planning.
type TriageConfig struct {
	// Enabled gates the triage step. Default true via Default(); set
	// `triage.enabled: false` in .kai/config.yaml to fall back to the
	// always-plan behavior. A rollout safety knob.
	Enabled bool `yaml:"enabled"`
}

// HandsOff reports whether the config selects AT LEAST the hands-off
// autonomy level. "yolo" is a superset (everything hands_off does plus
// gate auto-approve), so it returns true for yolo too. Flag/env
// overrides are applied by the caller (cmd/kai), not here.
func (c Config) HandsOff() bool {
	return c.Autonomy == "hands_off" || c.Autonomy == "yolo"
}

// Yolo reports whether the config selects the fully-autonomous "yolo"
// level: hands_off PLUS auto-resolving the gate (the one decision
// hands_off leaves to the human). It publishes changes the gate held,
// so it's opt-in only. Flag/env overrides applied by the caller.
func (c Config) Yolo() bool {
	return c.Autonomy == "yolo"
}

// AgentConfig controls how kai's in-process agent runner behaves.
//
// Note: post-Slice 6, kai owns the full agent loop in-process —
// there's no external `agent.command` to configure. The yaml field
// is gone; pre-Slice 6 configs that set it will have the value
// silently ignored at load time (yaml.v3 tolerates unknown fields).
//
// Model specifies the AI model to use for agent operations.
type AgentConfig struct {
	// Model is the model id used for agent operations (e.g.
	// "zai-org/GLM-5.1"). Any model exposed by the configured
	// provider is acceptable.
	Model string `yaml:"model"`

	// TimeoutSeconds is the outer wall-clock bound on a single
	// agent run. 0 means no outer cap. With IdleTimeoutSeconds
	// active (the primary kill switch), this is a safety net —
	// it only fires on a genuinely stuck loop that produces no
	// progress for the full window.
	TimeoutSeconds int `yaml:"timeout"`

	// IdleTimeoutSeconds is the inactivity cap. The watchdog
	// cancels the agent run if no progress signal (tool call or
	// streaming text-delta) fires for this long. 0 disables the
	// idle watcher; the legacy wall-clock-only behavior takes
	// over (not recommended — kills productive agents).
	IdleTimeoutSeconds int `yaml:"idle_timeout"`

	// BashAllow is the in-process bash tool's allowlist. When
	// non-empty, the first token of any `bash` tool call must match
	// one of these (e.g. ["npm", "go", "git", "make"]). Empty list
	// allows everything.
	BashAllow []string `yaml:"bash_allow"`

	// AutoTest enables the auto-test pass after a coding agent
	// applies edits. The harness runs the project's test command
	// and reports pass/fail, regardless of what the test agent
	// claims. Default true via Default(); set false in
	// .kai/config.yaml to opt out.
	AutoTest bool `yaml:"auto_test"`
}

// PlannerConfig controls the natural-language planner.
type PlannerConfig struct {
	// Model is the model id used for plan generation (e.g.
	// "Qwen/Qwen3.5-397B-A17B"). Any model exposed by the configured
	// provider is acceptable.
	Model string `yaml:"model"`

	// MaxAgents caps how many agents a single plan may spawn. The
	// planner's LLM is told this number so it doesn't propose more.
	MaxAgents int `yaml:"max_agents"`
}

// ChatConfig controls the conversational chat agent — questions,
// discussion, "talking" turns that don't write code.
type ChatConfig struct {
	// Model is the model id used for conversation turns (e.g.
	// "Qwen/Qwen3.5-397B-A17B").
	Model string `yaml:"model"`
}

// ClassifierConfig controls the request classifier — the first step
// of every substantive turn, which decides whether the user wants a
// conversation or a code change.
type ClassifierConfig struct {
	// Model is the model id used to classify chat-vs-code. A strong
	// model is used here on purpose: misrouting a turn is expensive
	// (a code request sent to the read-only chat agent loops, a
	// question sent to the planner burns a planning cycle).
	Model string `yaml:"model"`
}

// ReviewConfig controls the gate-review model — the reviewer that
// audits a held change and flags issues for the fix agent.
type ReviewConfig struct {
	// Model is the model id used to review code changes. A strong
	// model is used here on purpose: the reviewer is the quality
	// gate over the cheaper agent's output (it catches incorrect or
	// incomplete work the build/test pass can't judge), so it's
	// worth spending Opus tokens on a single diff-review pass.
	Model string `yaml:"model"`

	// DeployedModels names the model ids the reviewed workspace
	// actually serves to its users (e.g. the product's configured LLM).
	// They are runtime keys the reviewer can't see in a diff: a deployed
	// model need not appear in the changed code, yet must still exist in
	// the price/config tables. Supplying them lets the gate review's
	// config cross-reference flag an unpriced *deployed* model. Empty by
	// default; extendable at runtime via KAI_DEPLOYED_MODELS (comma-sep).
	DeployedModels []string `yaml:"deployed_models"`

	// Invariants are declared co-occurrence rules enforced by
	// `kai analyze invariants`: a function that calls Trigger must also
	// call Require (same body). They make the reviewer's
	// forward-consequence-trace finding deterministic — once you've
	// discovered "this mutation must also recompute X", promote it to a
	// static check instead of re-finding it with a model. Empty by default.
	Invariants []InvariantRule `yaml:"invariants"`
}

// InvariantRule is one declared co-occurrence invariant for
// `kai analyze invariants`. Trigger and Require match the FINAL
// identifier of a call expression (so the receiver doesn't matter:
// `h.db.AddMember(...)` matches Trigger "AddMember"). Message, when set,
// replaces the default violation text.
type InvariantRule struct {
	Trigger string `yaml:"trigger"`
	Require string `yaml:"require"`
	Message string `yaml:"message"`
}

// Built-in role models. Routing rationale: Opus decides intent,
// GLM-5.1 reasons, converses, and writes code.
//
// Planner/chat were originally split onto Qwen models, but dogfood
// testing found both Together-routed Qwen variants unreliable:
// Qwen3.5-397B was slow (~60s/turn) and intermittently returned empty
// responses / malformed plan JSON; Qwen3-Coder-Next returned empty
// responses near-instantly. zai-org/GLM-5.1 ran reliably in the same
// tests and is the model kai's own error-classifier recommends as a
// dependable default — so all three work roles use it. The classifier
// stays on Opus (a first-class Anthropic model) since misrouting a
// turn is costly.
const (
	defaultClassifierModel = "claude-opus-4-6"
	defaultReviewModel     = "claude-opus-4-6"
	// Open models now route through OpenRouter (switched from Together
	// 2026-06-05) — ids are OpenRouter slugs. Classifier/review stay on
	// native Anthropic this phase (Opus caching). DeepSeek/GLM behavior
	// notes below still apply — same underlying models, new slugs.
	defaultPlannerModel = "deepseek/deepseek-v4-pro"
	// Chat runs the coding agent loop (chat mode is code mode) but on
	// ChatModel, so it hits the SAME silent-death failure the executor
	// did on DeepSeek-V4-Pro: 0/0 tokens → "Model returned no text"
	// (observed 2026-05-31 on a critic-retry turn — heavy context +
	// the hidden <think> step starved the completion, and the run
	// errored instead of recovering). Default it to GLM-5.1, the
	// reliable agent-loop model, same as defaultAgentModel below. The
	// planner keeps DeepSeek-V4-Pro for its reasoning-heavy exploration
	// and reroutes only its constrained finalize turn (tui.go:514).
	// A user who wants a reasoning chat model can set KAI_CHAT_MODEL.
	defaultChatModel = "z-ai/glm-5.1"
	// Executor stays on GLM-5.1. DeepSeek-V4-Pro can silently
	// die mid-task on multi-file edits — observed 2026-05-25:
	// model emitted a text-only turn with finish_reason=tool_use
	// but no tool_call block (likely the DSML-delimiter leak the
	// provider filter sometimes mangles), then returned 0/0
	// tokens on the next turn and the runner terminated with
	// zero edits and a misleading "Done: 0/0/0" summary. Until
	// we either improve the runner's empty-response handling or
	// fix the DSML leak upstream, executors run on GLM-5.1 where
	// this failure mode hasn't been observed.
	defaultAgentModel = "z-ai/glm-5.1"
)

// Default returns the config used when no file is present.
func Default() Config {
	return Config{
		Agent: AgentConfig{
			Model:              defaultAgentModel,
			TimeoutSeconds:     1800, // 30 minutes — outer safety net only
			IdleTimeoutSeconds: 300,  // 5 minutes — primary kill on inactivity
			AutoTest:           true,
		},
		Planner: PlannerConfig{
			Model:     defaultPlannerModel,
			MaxAgents: 5,
		},
		Chat: ChatConfig{
			Model: defaultChatModel,
		},
		Classifier: ClassifierConfig{
			Model: defaultClassifierModel,
		},
		Review: ReviewConfig{
			Model: defaultReviewModel,
		},
		Triage: TriageConfig{
			Enabled: true,
		},
	}
}

func defaultIfEmpty(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// Load reads <kaiDir>/config.yaml. Missing file → Default. Malformed
// file is an error: silent fallback would mask config drift the user
// expects to take effect.
//
// Partial yaml is tolerated — any field not specified gets the
// default value. We achieve this by unmarshaling onto a Default()
// copy rather than a zero value.
func Load(kaiDir string) (Config, error) {
	cfg := Default()
	p := filepath.Join(kaiDir, configFileName)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", p, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", p, err)
	}
	cfg.Agent.Model = defaultIfEmpty(cfg.Agent.Model, Default().Agent.Model)
	cfg.Planner.Model = defaultIfEmpty(cfg.Planner.Model, Default().Planner.Model)
	cfg.Chat.Model = defaultIfEmpty(cfg.Chat.Model, Default().Chat.Model)
	cfg.Classifier.Model = defaultIfEmpty(cfg.Classifier.Model, Default().Classifier.Model)
	cfg.Review.Model = defaultIfEmpty(cfg.Review.Model, Default().Review.Model)
	if cfg.Planner.MaxAgents <= 0 {
		cfg.Planner.MaxAgents = Default().Planner.MaxAgents
	}
	return cfg, nil
}
