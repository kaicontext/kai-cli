package provider

import (
	"encoding/json"
	"testing"
)

func TestIsOpenAIModel(t *testing.T) {
	cases := map[string]bool{
		"claude-sonnet-4-6":         false,
		"claude-haiku-4-5-20251001": false,
		"gpt-4o":                    true,
		"gpt-4o-mini":               true,
		"gpt-4-turbo":               true,
		"o3":                        true,
		"o3-mini":                   true,
		"o4-mini":                   true,
		"o1-preview":                true,
		"  GPT-4o  ":                true, // trim + lower
		"":                          false,
		"gemini-1.5-pro":            false,
		"some-future-model":         false,

		// OpenRouter allowlist — must route through /completions just
		// like OpenAI. Lockstep with the server's openRouterAllowlist
		// (kailab-control internal/api/llm_routing.go). Slugs are
		// OpenRouter's lowercase vendor/model form.
		"deepseek/deepseek-v4-pro": true,
		"z-ai/glm-5.1":             true,
		"moonshotai/kimi-k2.6":     true,
		"qwen/qwen3.5-397b-a17b":   true,
		"qwen/qwen3-coder-next":    true,

		// OLD Together ids (uppercase) are no longer recognized → route
		// to /messages by default (which then 400s). Other unknowns too.
		"Qwen/Qwen3.5-397B-A17B":      false,
		"deepseek-ai/DeepSeek-V4-Pro": false,
		"qwen/qwen3-32b":              false,
		"llama-3.3-70b-versatile":     false,
	}
	for model, want := range cases {
		got := IsOpenAIModel(model)
		if got != want {
			t.Errorf("IsOpenAIModel(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestAdjustForReasoning_QwenFloorsMaxTokens pins the client-side
// mirror of the server's max_tokens floor for SCHEMA-CONSTRAINED
// (planner) requests: for a Together Qwen3 model with
// response_format=json_schema, max_tokens is raised to the planner
// floor (4096) when the client sent less. The JSON schema bounds
// output length naturally; we just need room for the silent
// reasoning step plus a structured-output body.
func TestAdjustForReasoning_QwenFloorsMaxTokens(t *testing.T) {
	in := []byte(`{"model":"Qwen/Qwen3.5-397B-A17B","max_tokens":1024,"response_format":{"type":"json_schema","json_schema":{}}}`)
	out := adjustForReasoning(in, "Qwen/Qwen3.5-397B-A17B")
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != reasoningMaxTokensFloor {
		t.Errorf("max_tokens = %v, want %d", got["max_tokens"], reasoningMaxTokensFloor)
	}
	// Together's API doesn't use Groq's reasoning_format param —
	// the client shouldn't inject one and risk an unknown-param
	// reject from Together.
	if _, has := got["reasoning_format"]; has {
		t.Errorf("client should not inject reasoning_format for Together; got %v", got["reasoning_format"])
	}
}

// TestAdjustForReasoning_DeepSeekV4FloorsMaxTokens is the regression
// guard for the 2026-05-29 bug: this file's isReasoningModel matched
// only Qwen3, so deepseek-ai/DeepSeek-V4-Pro was NOT recognized as a
// reasoning model here, never got the max_tokens floor, and returned
// empty completions ("Model returned no text") in chat + planner
// finalization. The classifier now delegates to one canonical list;
// DeepSeek-V4-Pro must get the chat floor.
func TestAdjustForReasoning_DeepSeekV4FloorsMaxTokens(t *testing.T) {
	const model = "deepseek-ai/DeepSeek-V4-Pro"
	if !isReasoningModel(model) {
		t.Fatalf("isReasoningModel(%q) = false; DeepSeek-V4-Pro must be recognized as reasoning", model)
	}
	in := []byte(`{"model":"deepseek-ai/DeepSeek-V4-Pro","max_tokens":1024}`)
	out := adjustForReasoning(in, model)
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != reasoningChatMaxTokensFloor {
		t.Errorf("DeepSeek-V4-Pro chat max_tokens = %v, want %d (floor must apply)", got["max_tokens"], reasoningChatMaxTokensFloor)
	}
}

// TestAdjustForReasoning_QwenChatFloorsToChatBudget covers the
// free-form chat path: no response_format → chat floor (16384)
// applies. Without this, complex meta questions blow the 4096
// budget on the silent <think> step and the visible response is
// empty. The 2026-05-15 dogfood hit this on /chat with a long
// meta-question.
func TestAdjustForReasoning_QwenChatFloorsToChatBudget(t *testing.T) {
	in := []byte(`{"model":"Qwen/Qwen3.5-397B-A17B","max_tokens":1024}`)
	out := adjustForReasoning(in, "Qwen/Qwen3.5-397B-A17B")
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != reasoningChatMaxTokensFloor {
		t.Errorf("chat-path max_tokens = %v, want %d", got["max_tokens"], reasoningChatMaxTokensFloor)
	}
}

// TestAdjustForReasoning_ChatFloorAppliesWhenAboveSchemaFloor:
// a chat request that already has 6000 tokens (above the schema
// floor of 4096) is still raised to the chat floor (16384) because
// it's not schema-constrained. The two floors are independent.
func TestAdjustForReasoning_ChatFloorAppliesWhenAboveSchemaFloor(t *testing.T) {
	in := []byte(`{"model":"Qwen/Qwen3.5-397B-A17B","max_tokens":6000}`)
	out := adjustForReasoning(in, "Qwen/Qwen3.5-397B-A17B")
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != reasoningChatMaxTokensFloor {
		t.Errorf("chat-path with 6000 should raise to chat floor; got %v", got["max_tokens"])
	}
}

// TestAdjustForReasoning_NonReasoningUntouched confirms the helper
// is a no-op for non-reasoning models — the client must not start
// raising max_tokens on Anthropic / OpenAI / non-Qwen Together
// models that don't need the floor.
func TestAdjustForReasoning_NonReasoningUntouched(t *testing.T) {
	in := []byte(`{"model":"claude-opus-4-7","max_tokens":1024}`)
	out := adjustForReasoning(in, "claude-opus-4-7")
	if string(out) != string(in) {
		t.Errorf("expected no mutation for non-reasoning model; got %s", out)
	}
}

// TestAdjustForReasoning_RespectsClientOverride mirrors the server's
// "trust explicit settings" semantic for the schema-constrained
// (planner) path: if the caller already set max_tokens at or above
// the planner floor and the request has response_format=json_schema,
// leave it alone. Advanced clients raising the limit for long
// structured replies must not be silently overridden.
func TestAdjustForReasoning_RespectsClientOverride(t *testing.T) {
	in := []byte(`{"model":"Qwen/Qwen3.5-397B-A17B","max_tokens":8000,"response_format":{"type":"json_schema","json_schema":{}}}`)
	out := adjustForReasoning(in, "Qwen/Qwen3.5-397B-A17B")
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != 8000 {
		t.Errorf("max_tokens lowered below client setting: %v", got["max_tokens"])
	}
}

// TestAdjustForReasoning_ChatRespectsHigherClientOverride: if a
// chat caller has already set max_tokens above the chat floor
// (e.g. 32k for a long-form explanation request), we must not
// lower it.
func TestAdjustForReasoning_ChatRespectsHigherClientOverride(t *testing.T) {
	in := []byte(`{"model":"Qwen/Qwen3.5-397B-A17B","max_tokens":32000}`)
	out := adjustForReasoning(in, "Qwen/Qwen3.5-397B-A17B")
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if mt, _ := got["max_tokens"].(float64); int(mt) != 32000 {
		t.Errorf("chat max_tokens lowered below client setting: %v", got["max_tokens"])
	}
}
