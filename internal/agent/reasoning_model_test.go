package agent

import "testing"

// TestIsReasoningModel covers the known-reasoning family detection.
// Used by the runner to pre-allocate a larger MaxTokens budget so
// the silent <think> trace and visible output can coexist. Adding
// a new family means one line in isReasoningModel; the test rows
// pin existing positives + the negative cases that must NOT trip
// (the regular chat models we don't want over-budgeted).
func TestIsReasoningModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		// Positives — should get the higher budget.
		{"Qwen/Qwen3-32B", true},
		{"qwen3-235b-a22b", true},
		{"Qwen/Qwen2.5-7B-Instruct", true},
		{"o1", true},
		{"o1-preview", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4", true},
		{"o4-mini", true},
		{"gpt-5", true},
		{"gpt-5-thinking", true},
		{"deepseek-ai/DeepSeek-R1", true},
		{"DeepSeek-R1-Distill-Qwen-32B", true},
		{"some-vendor/proprietary-reasoning-model", true},
		{"upstream-r1-variant", true},
		// 2026-05-24 dogfood: V4-Pro burned 4m52s of hidden
		// reasoning for 472 visible output tokens. Behaves like
		// the R-series families, gets the same scaled budget.
		{"deepseek-ai/DeepSeek-V4-Pro", true},

		// Negatives — these are non-reasoning models and shouldn't
		// trip the heuristic; they get the standard budget.
		{"claude-sonnet-4-6", false},
		{"claude-opus-4-7", false},
		{"moonshotai/Kimi-K2.6", false},
		{"gpt-4o", false},
		{"gpt-4-turbo", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := isReasoningModel(c.model); got != c.want {
				t.Errorf("isReasoningModel(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}
