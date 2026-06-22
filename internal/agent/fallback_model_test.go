package agent

import "testing"

// TestPickFallbackModel_SkipsCurrent — if the failing model IS the
// first fallback, we must pick a DIFFERENT one rather than retrying
// the same model. Avoids the obvious "same model returned empty
// again because it's still the same model" loop.
func TestPickFallbackModel_SkipsCurrent(t *testing.T) {
	got := pickFallbackModel("z-ai/glm-5.1")
	if got == "z-ai/glm-5.1" {
		t.Errorf("must not return same model as fallback, got %q", got)
	}
	// Should fall through to Kimi (the second entry).
	if got != "moonshotai/kimi-k2.6" {
		t.Errorf("expected Kimi as fallback when GLM is the current model, got %q", got)
	}
}

// TestPickFallbackModel_DefaultsToFirst — for a non-fallback
// primary (e.g. DeepSeek-V4-Pro), pick the first entry in the chain.
func TestPickFallbackModel_DefaultsToFirst(t *testing.T) {
	cases := []string{
		"deepseek/deepseek-v4-pro",
		"qwen/qwen3-72b",
		"anthropic/claude-opus-4-6",
	}
	for _, c := range cases {
		got := pickFallbackModel(c)
		if got != "z-ai/glm-5.1" {
			t.Errorf("pickFallbackModel(%q) = %q, want z-ai/glm-5.1", c, got)
		}
	}
}

// TestPickFallbackModel_CaseInsensitive — model ids in the wild
// arrive with mixed casing (provider config vs request log vs API).
// Match should be lowercase-stable.
func TestPickFallbackModel_CaseInsensitive(t *testing.T) {
	if got := pickFallbackModel("Z-AI/GLM-5.1"); got == "z-ai/glm-5.1" {
		t.Errorf("case-insensitive match should still skip current model; got %q", got)
	}
}

// TestPickFallbackModel_AllExhausted — when current matches both
// fallbacks, return "" so the caller knows there's no recovery
// option (and falls back to the original empty-response surface).
func TestPickFallbackModel_AllExhausted(t *testing.T) {
	// Synthetic: current contains BOTH fallback names. Implausible
	// in practice but tests the guard.
	got := pickFallbackModel("z-ai/glm-5.1 moonshotai/kimi-k2.6 combo")
	if got != "" {
		t.Errorf("expected empty when all fallbacks match current, got %q", got)
	}
}
