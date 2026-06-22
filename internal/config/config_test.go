package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("expected defaults, got %+v", cfg)
	}
}

func TestLoad_FullOverride(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`
agent:
  timeout: 1200
  bash_allow: [npm, go]
planner:
  model: claude-opus-4-7
  max_agents: 8
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		Agent: AgentConfig{
			Model:              Default().Agent.Model, // yaml didn't set it → default applies
			TimeoutSeconds:     1200,
			IdleTimeoutSeconds: Default().Agent.IdleTimeoutSeconds, // yaml didn't set it → default applies
			BashAllow:          []string{"npm", "go"},
			AutoTest:           true, // default-on; FullOverride yaml doesn't disable it
		},
		Planner: PlannerConfig{
			Model:     "claude-opus-4-7",
			MaxAgents: 8,
		},
		// yaml didn't set these → role-model defaults apply.
		Chat:       Default().Chat,
		Classifier: Default().Classifier,
		Review:     Default().Review,
		Triage: TriageConfig{
			Enabled: true, // yaml didn't set it → default applies
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("unexpected config:\n got: %+v\nwant: %+v", cfg, want)
	}
}

// TestLoad_PartialOverrideKeepsDefaults: the user only specifies the
// model, so everything else should fall back to Default(). Critical
// for forward-compat — we add a new config field, existing yamls
// shouldn't break.
func TestLoad_PartialOverrideKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("planner:\n  model: claude-haiku-4-5\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Planner.Model != "claude-haiku-4-5" {
		t.Errorf("model override lost: %s", cfg.Planner.Model)
	}
	if cfg.Planner.MaxAgents != Default().Planner.MaxAgents {
		t.Errorf("max_agents should default, got %d", cfg.Planner.MaxAgents)
	}
	if !reflect.DeepEqual(cfg.Agent, Default().Agent) {
		t.Errorf("agent block should default: %+v", cfg.Agent)
	}
}

// TestLoad_LegacyCommandFieldIgnored: pre-Slice 6 configs may have
// `agent.command: [...]` set. yaml.v3 silently ignores unknown fields,
// so existing configs load without error. The non-deprecated fields
// still parse normally.
func TestLoad_LegacyCommandFieldIgnored(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`agent:
  command: ["claude", "-p", "{prompt}"]
  timeout: 60
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.TimeoutSeconds != 60 {
		t.Errorf("timeout override lost: %d", cfg.Agent.TimeoutSeconds)
	}
}

// TestLoad_BashAllowParses verifies the bash_allow allowlist round-trips
// from yaml so the in-process agent's bash tool can pick it up.
func TestLoad_BashAllowParses(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`agent:
  bash_allow: [npm, go, git, make]
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(cfg.Agent.BashAllow, []string{"npm", "go", "git", "make"}) {
		t.Errorf("bash_allow: %v", cfg.Agent.BashAllow)
	}
}

// TestDefault_RoleModels pins the role-based routing: the classifier
// runs on a strong model (Opus), planner and chat on QWEN, and code
// agents on GLM. A regression here silently sends the wrong kind of
// work to the wrong model.
func TestDefault_RoleModels(t *testing.T) {
	d := Default()
	cases := []struct{ role, got, want string }{
		{"classifier", d.Classifier.Model, "claude-opus-4-6"},
		{"review", d.Review.Model, "claude-opus-4-6"},
		{"planner", d.Planner.Model, "deepseek/deepseek-v4-pro"},
		// Chat is a coding-mode agent loop → GLM-5.1, same as the
		// executor (DeepSeek silently dies mid-loop). OpenRouter slugs.
		{"chat", d.Chat.Model, "z-ai/glm-5.1"},
		{"agent (code)", d.Agent.Model, "z-ai/glm-5.1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s model: got %q, want %q", c.role, c.got, c.want)
		}
	}
}

// TestLoad_ChatClassifierOverride confirms the new role blocks parse
// from yaml, and that an omitted block falls back to its default.
func TestLoad_ChatClassifierOverride(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("chat:\n  model: my-chat-model\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Chat.Model != "my-chat-model" {
		t.Errorf("Chat.Model override lost: got %q", cfg.Chat.Model)
	}
	if cfg.Classifier.Model != Default().Classifier.Model {
		t.Errorf("Classifier.Model should default when yaml omits it: got %q", cfg.Classifier.Model)
	}
}

// TestHandsOff covers the autonomy field → HandsOff() mapping. Only
// the explicit "hands_off" value enables it; everything else (empty,
// "off", anything unrecognized) stays interactive.
func TestHandsOff(t *testing.T) {
	cases := []struct {
		autonomy string
		want     bool
	}{
		{"hands_off", true},
		{"", false},
		{"off", false},
		{"on", false}, // unrecognized → safe default (interactive)
	}
	for _, c := range cases {
		if got := (Config{Autonomy: c.autonomy}).HandsOff(); got != c.want {
			t.Errorf("Autonomy %q: HandsOff() = %v, want %v", c.autonomy, got, c.want)
		}
	}
	if Default().HandsOff() {
		t.Error("Default() should not be hands-off")
	}
}

// TestLoad_AutonomyParses confirms the autonomy field round-trips from
// yaml so a project can opt into hands-off mode persistently.
func TestLoad_AutonomyParses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("autonomy: hands_off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.HandsOff() {
		t.Errorf("autonomy: hands_off should load as hands-off, got %q", cfg.Autonomy)
	}
}

func TestDefaultIfEmpty(t *testing.T) {
	cases := []struct {
		val, fallback, want string
	}{
		{"my-model", "fallback", "my-model"},
		{"", "fallback", "fallback"},
		{"", "", ""},
		{"value", "", "value"},
	}
	for _, c := range cases {
		got := defaultIfEmpty(c.val, c.fallback)
		if got != c.want {
			t.Errorf("defaultIfEmpty(%q, %q) = %q, want %q", c.val, c.fallback, got, c.want)
		}
	}
}

func TestLoad_EmptyPlannerModelFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("planner:\n  model: \"\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Planner.Model != Default().Planner.Model {
		t.Errorf("Planner.Model should default when yaml sets empty: got %q", cfg.Planner.Model)
	}
}

func TestLoad_EmptyChatModelFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("chat:\n  model: \"\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Chat.Model != Default().Chat.Model {
		t.Errorf("Chat.Model should default when yaml sets empty: got %q", cfg.Chat.Model)
	}
}

func TestLoad_EmptyClassifierModelFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("classifier:\n  model: \"\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Classifier.Model != Default().Classifier.Model {
		t.Errorf("Classifier.Model should default when yaml sets empty: got %q", cfg.Classifier.Model)
	}
}

func TestLoad_EmptyReviewModelFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("review:\n  model: \"\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Review.Model != Default().Review.Model {
		t.Errorf("Review.Model should default when yaml sets empty: got %q", cfg.Review.Model)
	}
}

func TestLoad_MalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("not: : valid:"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestLoad_AgentModelOverride pins the agent.model field's
// load-time path. The wider end-to-end wiring (Load → OrchestratorCfg
// → agent.Options → runner) is exercised by cmd/kai tests; here we
// just confirm the yaml field reads back and the empty-string
// fallback restores the default.
func TestLoad_AgentModelOverride(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("agent:\n  model: my-private-model\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.Model != "my-private-model" {
		t.Errorf("Agent.Model override lost: got %q", cfg.Agent.Model)
	}
}

func TestLoad_EmptyAgentModelFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	// Specify an agent block but no model — should pick up the default.
	yaml := []byte("agent:\n  timeout: 60\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.Model != Default().Agent.Model {
		t.Errorf("Agent.Model should default when yaml omits it: got %q", cfg.Agent.Model)
	}
}

// TestModelOverrideFromFlags verifies the CLI flag override logic that
// runCodeTUI applies after config.Load(). When the override strings are
// empty, config values are untouched; when non-empty, they replace
// whatever config.Load set.
func TestModelOverrideFromFlags(t *testing.T) {
	t.Run("empty_flags_preserve_config_values", func(t *testing.T) {
		dir := t.TempDir()
		yaml := []byte("planner:\n  model: claude-opus-4-7\nagent:\n  model: my-private-model\n")
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		plannerFlag := ""
		agentFlag := ""
		if plannerFlag != "" {
			cfg.Planner.Model = plannerFlag
		}
		if agentFlag != "" {
			cfg.Agent.Model = agentFlag
		}
		if cfg.Planner.Model != "claude-opus-4-7" {
			t.Errorf("Planner.Model should retain config value: got %q", cfg.Planner.Model)
		}
		if cfg.Agent.Model != "my-private-model" {
			t.Errorf("Agent.Model should retain config value: got %q", cfg.Agent.Model)
		}
	})

	t.Run("nonempty_flags_replace_config_values", func(t *testing.T) {
		dir := t.TempDir()
		yaml := []byte("planner:\n  model: claude-opus-4-7\nagent:\n  model: my-private-model\n")
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		plannerFlag := "claude-sonnet-4-6"
		agentFlag := "zai-org/GLM-5.1"
		if plannerFlag != "" {
			cfg.Planner.Model = plannerFlag
		}
		if agentFlag != "" {
			cfg.Agent.Model = agentFlag
		}
		if cfg.Planner.Model != "claude-sonnet-4-6" {
			t.Errorf("Planner.Model should be overridden: got %q", cfg.Planner.Model)
		}
		if cfg.Agent.Model != "zai-org/GLM-5.1" {
			t.Errorf("Agent.Model should be overridden: got %q", cfg.Agent.Model)
		}
	})
}

// TestLoad_TriageCanBeDisabled locks the kill-switch: triage is on by
// default but `triage.enabled: false` in config.yaml turns it off.
func TestLoad_TriageDefaultsOn(t *testing.T) {
	if !Default().Triage.Enabled {
		t.Error("Default() should have triage enabled")
	}
}

func TestLoad_TriageCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte("triage:\n  enabled: false\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Triage.Enabled {
		t.Error("triage.enabled: false in yaml should disable triage")
	}
}
