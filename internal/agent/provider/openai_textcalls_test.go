package provider

import (
	"testing"

	"kai/internal/agent/message"
	"kai/internal/agent/tools"
)

func toolset(names ...string) []tools.ToolInfo {
	out := make([]tools.ToolInfo, len(names))
	for i, n := range names {
		out[i] = tools.ToolInfo{Name: n}
	}
	return out
}

// rawJSONExtraction: bare JSON object embedded in prose extracts to
// a ToolCall part with the surrounding prose preserved as text.
func TestExtractRaw_BareJSON(t *testing.T) {
	content := `I'll check the impact.

{"name": "kai_callers", "arguments": {"file": "main.go"}}

That'll tell us what depends on it.`
	parts, ok := extractToolCallsFromText(content, toolset("kai_callers"), "raw")
	if !ok {
		t.Fatal("expected extraction")
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts (text + call), got %d: %+v", len(parts), parts)
	}
	if _, ok := parts[0].(message.TextContent); !ok {
		t.Errorf("first part should be TextContent, got %T", parts[0])
	}
	tc, ok := parts[1].(message.ToolCall)
	if !ok || tc.Name != "kai_callers" || tc.Input != `{"file": "main.go"}` {
		t.Errorf("tool call wrong: %+v", parts[1])
	}
}

// rawJSONExtraction with markdown fence: the ```json fence is
// transparent — the JSON inside is still extracted.
func TestExtractRaw_MarkdownFence(t *testing.T) {
	content := "Here's the call:\n\n```json\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"x.go\"}}\n```\n"
	parts, ok := extractToolCallsFromText(content, toolset("view"), "raw")
	if !ok {
		t.Fatal("expected extraction")
	}
	tc, _ := parts[len(parts)-1].(message.ToolCall)
	if tc.Name != "view" {
		t.Errorf("wrong tool: %+v", tc)
	}
}

// Hermes wrapper: <tool_call>...</tool_call> is recognized.
func TestExtractHermes(t *testing.T) {
	content := `Reasoning... <tool_call>{"name": "bash", "arguments": {"command": "ls"}}</tool_call> done.`
	parts, ok := extractToolCallsFromText(content, toolset("bash"), "hermes")
	if !ok {
		t.Fatal("expected extraction")
	}
	tc, _ := parts[len(parts)-1].(message.ToolCall)
	if tc.Name != "bash" {
		t.Errorf("wrong tool: %+v", tc)
	}
}

// Llama 3 wrapper: <|python_tag|>{...} with "parameters" instead
// of "arguments" still parses.
func TestExtractLlama3_PythonTag(t *testing.T) {
	content := `<|python_tag|>{"name": "view", "parameters": {"file_path": "x.go"}}`
	parts, ok := extractToolCallsFromText(content, toolset("view"), "llama3")
	if !ok {
		t.Fatal("expected extraction")
	}
	tc, _ := parts[len(parts)-1].(message.ToolCall)
	if tc.Name != "view" || tc.Input != `{"file_path": "x.go"}` {
		t.Errorf("wrong call: %+v", tc)
	}
}

// Unknown tool name is ignored — never dispatch what wasn't offered.
func TestExtractRaw_UnknownToolIgnored(t *testing.T) {
	content := `{"name": "rm_dash_rf_slash", "arguments": {}}`
	_, ok := extractToolCallsFromText(content, toolset("view"), "raw")
	if ok {
		t.Error("unknown tool should NOT extract")
	}
}

// Malformed JSON is skipped without error.
func TestExtractRaw_MalformedSkipped(t *testing.T) {
	content := `{"name": "view", "arguments": BROKEN_NOT_JSON}`
	_, ok := extractToolCallsFromText(content, toolset("view"), "raw")
	if ok {
		t.Error("malformed JSON should NOT extract")
	}
}

// No tool call in plain text returns no extraction.
func TestExtractRaw_NoToolCall(t *testing.T) {
	_, ok := extractToolCallsFromText("No tool call here, just analysis.", toolset("view"), "raw")
	if ok {
		t.Error("plain text should NOT extract")
	}
}

// Nested braces in arguments don't confuse the extractor.
func TestExtractRaw_NestedBraces(t *testing.T) {
	content := `{"name": "view", "arguments": {"opts": {"recursive": true, "depth": 3}}}`
	parts, ok := extractToolCallsFromText(content, toolset("view"), "raw")
	if !ok {
		t.Fatal("expected extraction")
	}
	tc, _ := parts[0].(message.ToolCall)
	if tc.Input != `{"opts": {"recursive": true, "depth": 3}}` {
		t.Errorf("nested args lost: %q", tc.Input)
	}
}

// Multiple tool calls in one response: both extract, in order.
func TestExtractRaw_MultipleCalls(t *testing.T) {
	content := `First: {"name": "view", "arguments": {"file_path": "a"}} then: {"name": "view", "arguments": {"file_path": "b"}}`
	parts, ok := extractToolCallsFromText(content, toolset("view"), "raw")
	if !ok {
		t.Fatal("expected extraction")
	}
	var tcs []message.ToolCall
	for _, p := range parts {
		if tc, ok := p.(message.ToolCall); ok {
			tcs = append(tcs, tc)
		}
	}
	if len(tcs) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Input != `{"file_path": "a"}` || tcs[1].Input != `{"file_path": "b"}` {
		t.Errorf("order wrong: %+v %+v", tcs[0], tcs[1])
	}
}

// applyTextCallFallback is a no-op when the response already has
// a structured ToolCall — don't double-dispatch.
func TestApplyTextCallFallback_NoOpWhenStructured(t *testing.T) {
	resp := &Response{
		Parts: []message.ContentPart{
			message.TextContent{Text: `{"name": "view", "arguments": {}}`},
			message.ToolCall{Name: "real_call", ID: "x"},
		},
	}
	applyTextCallFallback(resp, toolset("view", "real_call"))
	if len(resp.Parts) != 2 {
		t.Errorf("structured call present should prevent fallback: %+v", resp.Parts)
	}
}

// applyTextCallFallback flips FinishReason to ToolUse on success
// so the runner dispatches instead of treating as a final reply.
func TestApplyTextCallFallback_FlipsFinishReason(t *testing.T) {
	resp := &Response{
		Parts: []message.ContentPart{
			message.TextContent{Text: `{"name": "view", "arguments": {"file_path": "x"}}`},
		},
		FinishReason: message.FinishReasonEndTurn,
	}
	applyTextCallFallback(resp, toolset("view"))
	if resp.FinishReason != message.FinishReasonToolUse {
		t.Errorf("expected ToolUse finish, got %v", resp.FinishReason)
	}
}
