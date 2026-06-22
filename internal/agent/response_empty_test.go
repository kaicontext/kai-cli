package agent

import (
	"testing"

	"kai/internal/agent/message"
	"kai/internal/agent/provider"
)

// TestResponseIsEmpty_NoParts confirms the canonical empty case:
// the provider returned a Response with literally no content parts.
// Common with Qwen3 reasoning burning the entire budget on <think>.
func TestResponseIsEmpty_NoParts(t *testing.T) {
	if !responseIsEmpty(provider.Response{Parts: nil}) {
		t.Error("nil Parts must read as empty")
	}
	if !responseIsEmpty(provider.Response{Parts: []message.ContentPart{}}) {
		t.Error("empty Parts must read as empty")
	}
}

// TestResponseIsEmpty_WhitespaceOnlyText covers the case where the
// model produced a TextContent that is purely whitespace — also a
// useless response that should trigger the retry.
func TestResponseIsEmpty_WhitespaceOnlyText(t *testing.T) {
	for _, txt := range []string{"", "   ", "\n\n", "\t\t  \n"} {
		resp := provider.Response{
			Parts: []message.ContentPart{message.TextContent{Text: txt}},
		}
		if !responseIsEmpty(resp) {
			t.Errorf("whitespace-only text %q should read as empty", txt)
		}
	}
}

// TestResponseIsEmpty_HasText pins the negative path: any real text
// makes the response non-empty even if the model also did nothing
// else.
func TestResponseIsEmpty_HasText(t *testing.T) {
	resp := provider.Response{
		Parts: []message.ContentPart{message.TextContent{Text: "ok"}},
	}
	if responseIsEmpty(resp) {
		t.Error("text 'ok' should make response non-empty")
	}
}

// TestResponseIsEmpty_HasToolCall confirms a tool-only turn (no
// text but a real ToolCall) is NOT empty — the agent did real
// work. Retry would duplicate it.
func TestResponseIsEmpty_HasToolCall(t *testing.T) {
	resp := provider.Response{
		Parts: []message.ContentPart{
			message.ToolCall{Name: "view", Input: `{"file_path":"foo.go"}`},
		},
	}
	if responseIsEmpty(resp) {
		t.Error("tool-call-only response should not be empty")
	}
}

// TestResponseIsEmpty_MixedTextAndToolCall is the standard non-
// empty case: text plus a tool call. Definitely not empty.
func TestResponseIsEmpty_MixedTextAndToolCall(t *testing.T) {
	resp := provider.Response{
		Parts: []message.ContentPart{
			message.TextContent{Text: "Reading the file now."},
			message.ToolCall{Name: "view", Input: `{"file_path":"foo.go"}`},
		},
	}
	if responseIsEmpty(resp) {
		t.Error("text + tool call must not be empty")
	}
}
