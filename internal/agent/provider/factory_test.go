package provider

import (
	"strings"
	"testing"
)

// TestFromEnv_KailabDefault confirms unset KAI_PROVIDER routes to
// kailab using the supplied bearer + base URL — preserves existing
// behavior for users who haven't opted into BYOM.
func TestFromEnv_KailabDefault(t *testing.T) {
	t.Setenv("KAI_PROVIDER", "")
	cfg := FromEnv("https://kailab.test", "tok", "claude-sonnet-4-6")
	if cfg.Kind != KindKailab {
		t.Errorf("kind: %v", cfg.Kind)
	}
	if cfg.AuthToken != "tok" || cfg.BaseURL != "https://kailab.test" {
		t.Errorf("kailab fields: %+v", cfg)
	}
	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("model: %v", cfg.Model)
	}
}

func TestFromEnv_AnthropicSelected(t *testing.T) {
	t.Setenv("KAI_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-1")
	t.Setenv("KAI_ANTHROPIC_MODEL", "claude-opus-4-7")
	cfg := FromEnv("ignored-base", "ignored-token", "claude-sonnet-4-6")
	if cfg.Kind != KindAnthropic {
		t.Errorf("kind: %v", cfg.Kind)
	}
	if cfg.AuthToken != "sk-ant-1" {
		t.Errorf("auth: %v", cfg.AuthToken)
	}
	if cfg.Model != "claude-opus-4-7" {
		t.Errorf("model: %v", cfg.Model)
	}
}

func TestFromEnv_OpenAISelected(t *testing.T) {
	t.Setenv("KAI_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-oai-1")
	t.Setenv("KAI_OPENAI_BASE_URL", "https://api.together.xyz/v1")
	cfg := FromEnv("ignored", "ignored", "fallback-model")
	if cfg.Kind != KindOpenAI {
		t.Errorf("kind: %v", cfg.Kind)
	}
	if cfg.BaseURL != "https://api.together.xyz/v1" {
		t.Errorf("base: %v", cfg.BaseURL)
	}
	if cfg.AuthToken != "sk-oai-1" {
		t.Errorf("auth: %v", cfg.AuthToken)
	}
	if cfg.Model != "fallback-model" {
		t.Errorf("expected fallback model when KAI_OPENAI_MODEL unset, got %v", cfg.Model)
	}
}

// TestNew_ErrorsAreActionable: each missing-key error mentions the
// env var or command the user needs to run, not Go internals.
func TestNew_ErrorsAreActionable(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"kailab no token", Config{Kind: KindKailab, BaseURL: "https://x"}, "kai auth login"},
		{"kailab no base", Config{Kind: KindKailab, AuthToken: "x"}, "kai auth login"},
		{"anthropic no key", Config{Kind: KindAnthropic}, "ANTHROPIC_API_KEY"},
		{"unknown kind", Config{Kind: Kind("bogus")}, "unknown kind"},
	}
	for _, tc := range cases {
		_, err := New(tc.cfg)
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q missing %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestNew_OpenAIAllowsEmptyKey: local servers (Ollama, vLLM no
// auth) shouldn't require a key. New should succeed; the Send call
// will fail with whatever the server returns.
func TestNew_OpenAIAllowsEmptyKey(t *testing.T) {
	p, err := New(Config{Kind: KindOpenAI, BaseURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatalf("expected success with empty key for local openai-compat, got %v", err)
	}
	if p == nil {
		t.Fatal("nil provider")
	}
}

// TestNormalizeKind_AcceptsAliases pins the alias surface so a
// future change can't silently drop "openai-compatible" support
// and break user docs / muscle memory. The aliases exist because
// "openai" is misleading when the endpoint is actually Ollama /
// LM Studio / Together / Groq — same wire protocol, different
// vendor.
func TestNormalizeKind_AcceptsAliases(t *testing.T) {
	cases := map[string]Kind{
		// canonical
		"openai":            KindOpenAI,
		"anthropic":         KindAnthropic,
		"kailab":            KindKailab,
		// openai aliases
		"openai-compat":     KindOpenAI,
		"openai-compatible": KindOpenAI,
		"oai":               KindOpenAI,
		"oai-compat":        KindOpenAI,
		"local":             KindOpenAI,
		// anthropic aliases
		"anthropic-direct":  KindAnthropic,
		"claude":            KindAnthropic,
		// case + whitespace tolerance
		"OpenAI-Compatible":   KindOpenAI,
		"  anthropic-direct ": KindAnthropic,
		// empty stays empty (caller defaults to kailab)
		"": "",
		// unknowns pass through verbatim so New() can return a
		// helpful "unknown kind %q" error
		"bedrock": Kind("bedrock"),
	}
	for in, want := range cases {
		if got := normalizeKind(in); got != want {
			t.Errorf("normalizeKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFromEnv_OpenAIAliasesAllResolveSame: the resolved Config
// for any of the openai aliases should be byte-identical to the
// "openai" canonical case. Catches a future "alias matched but
// got the wrong env vars" regression.
func TestFromEnv_OpenAIAliasesAllResolveSame(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("KAI_OPENAI_BASE_URL", "http://localhost:1234/v1")
	t.Setenv("KAI_OPENAI_MODEL", "qwen2.5-coder-7b")
	for _, alias := range []string{"openai", "openai-compat", "openai-compatible", "oai", "oai-compat", "local"} {
		t.Setenv("KAI_PROVIDER", alias)
		cfg := FromEnv("ignored-base", "ignored-token", "default-model")
		if cfg.Kind != KindOpenAI {
			t.Errorf("alias %q: kind = %v, want %v", alias, cfg.Kind, KindOpenAI)
		}
		if cfg.AuthToken != "sk-test" || cfg.BaseURL != "http://localhost:1234/v1" || cfg.Model != "qwen2.5-coder-7b" {
			t.Errorf("alias %q: cfg = %+v", alias, cfg)
		}
	}
}
