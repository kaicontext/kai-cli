package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kai/internal/agent/message"
	"kai/internal/agent/tools"
)

// TestOpenAI_SendHappyPath verifies the request body is in
// chat.completions shape (top-level model + messages, tools as
// {type:"function", function:{...}}) and that the response is
// parsed into our internal Response with ProviderNote indicating
// no cache support.
func TestOpenAI_SendHappyPath(t *testing.T) {
	var sawBody map[string]interface{}
	var sawAuth, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sawBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"finish_reason":"stop",
				"message":{"role":"assistant","content":"hello"}}],
			"usage":{"prompt_tokens":50,"completion_tokens":10}
		}`))
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "sk-test")
	resp, err := o.Send(context.Background(), Request{
		Model:     "gpt-4o-mini",
		System:    "you are kai",
		MaxTokens: 100,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		}},
		Tools: []tools.ToolInfo{{
			Name:        "echo",
			Description: "echo input",
			Parameters:  map[string]interface{}{"text": map[string]string{"type": "string"}},
			Required:    []string{"text"},
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawPath != "/chat/completions" {
		t.Errorf("path: %q", sawPath)
	}
	if sawAuth != "Bearer sk-test" {
		t.Errorf("auth header: %q", sawAuth)
	}
	if sawBody["model"] != "gpt-4o-mini" {
		t.Errorf("body model: %v", sawBody["model"])
	}
	tools, ok := sawBody["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("expected one tool, body: %+v", sawBody["tools"])
	}
	tool0 := tools[0].(map[string]interface{})
	if tool0["type"] != "function" {
		t.Errorf("tool[0].type: %v", tool0["type"])
	}
	if resp.InputTokens != 50 || resp.OutputTokens != 10 {
		t.Errorf("usage: %+v", resp)
	}
	if resp.ProviderNote != "(no cache support)" {
		t.Errorf("ProviderNote: %q", resp.ProviderNote)
	}
	if resp.FinishReason != message.FinishReasonEndTurn {
		t.Errorf("finish: %v", resp.FinishReason)
	}
}

// TestOpenAI_ToolCallTranslation walks a more interesting message
// shape: an assistant turn with a tool_call followed by a user turn
// containing a tool_result. We assert the wire form has the
// assistant with tool_calls plus a separate role:"tool" entry.
func TestOpenAI_ToolCallTranslation(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "do it"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "call_1", Name: "echo", Input: `{"text":"hi"}`, Type: "tool_use", Finished: true},
		}},
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_1", Content: "hi"},
		}},
	}
	out := openaiSerializeMessages("sys", msgs)
	if len(out) != 4 {
		t.Fatalf("expected 4 wire messages (system + user + assistant-with-tools + tool), got %d: %+v", len(out), out)
	}
	if out[0]["role"] != "system" {
		t.Errorf("first should be system, got %v", out[0])
	}
	asst := out[2]
	if asst["role"] != "assistant" {
		t.Errorf("expected assistant at [2], got %v", asst["role"])
	}
	if asst["content"] != nil {
		t.Errorf("assistant content should be nil when only tool_calls, got %v", asst["content"])
	}
	tcs := asst["tool_calls"].([]map[string]interface{})
	if len(tcs) != 1 || tcs[0]["id"] != "call_1" {
		t.Errorf("tool_calls: %+v", tcs)
	}
	tool := out[3]
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("expected tool message at [3], got %v", tool)
	}
}

// TestOpenAI_StreamingDeltas drives the SSE branch with a small
// vendor-realistic stream (text deltas, then a tool_call, then the
// final usage chunk and [DONE]). Asserts text accumulates, the
// tool_use event fires on finish_reason, and final usage is set.
func TestOpenAI_StreamingDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		send := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		send(`{"choices":[{"delta":{"content":"Hel"}}]}`)
		send(`{"choices":[{"delta":{"content":"lo"}}]}`)
		send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]}}]}`)
		send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		send(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
		send(`[DONE]`)
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "k")
	ch, err := o.SendStream(context.Background(), Request{
		Model: "gpt-4o-mini", MaxTokens: 50,
		Messages: []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	var deltas []string
	var tool *message.ToolCall
	var final *Response
	for ev := range ch {
		switch ev.Kind {
		case "text_delta":
			deltas = append(deltas, ev.Text)
		case "tool_use":
			tool = ev.ToolCall
		case "done":
			final = ev.Final
		case "error":
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if got := strings.Join(deltas, ""); got != "Hello" {
		t.Errorf("text: %q", got)
	}
	if tool == nil || tool.ID != "call_1" || tool.Name != "echo" {
		t.Errorf("tool: %+v", tool)
	}
	if final == nil {
		t.Fatal("no done event")
	}
	if final.InputTokens != 12 || final.OutputTokens != 3 {
		t.Errorf("usage: %+v", final)
	}
	if final.ProviderNote != "(no cache support)" {
		t.Errorf("note: %q", final.ProviderNote)
	}
	if final.FinishReason != message.FinishReasonToolUse {
		t.Errorf("finish: %v", final.FinishReason)
	}
}

// TestOpenAI_NoAuthHeaderWhenKeyEmpty: local servers (Ollama, vLLM
// without auth) need no Authorization. Setting an empty header
// would break some strict implementations.
func TestOpenAI_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()

	o := NewOpenAI(srv.URL, "")
	_, err := o.Send(context.Background(), Request{Model: "local", MaxTokens: 1})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawAuth != "" {
		t.Errorf("expected no Authorization header when key empty, got %q", sawAuth)
	}
}

// TestApplyOpenAICacheUsage covers the cached_tokens fix: a Together /
// OpenAI usage block that reports server-side cache reads must surface
// as CacheReadTokens, with InputTokens reduced to the new (uncached)
// portion. A zero keeps the honest "(no cache support)" note.
func TestApplyOpenAICacheUsage(t *testing.T) {
	t.Run("cache hit", func(t *testing.T) {
		out := Response{InputTokens: 1000}
		applyOpenAICacheUsage(&out, 1000, 800)
		if out.InputTokens != 200 {
			t.Errorf("InputTokens = %d, want 200 (1000 prompt - 800 cached)", out.InputTokens)
		}
		if out.CacheReadTokens != 800 || out.CachedInputTokens != 800 {
			t.Errorf("cache fields = read %d cached %d, want 800/800",
				out.CacheReadTokens, out.CachedInputTokens)
		}
		if out.ProviderNote != "" {
			t.Errorf("ProviderNote = %q, want empty on a real cache hit", out.ProviderNote)
		}
	})
	t.Run("no cache", func(t *testing.T) {
		out := Response{InputTokens: 1000}
		applyOpenAICacheUsage(&out, 1000, 0)
		if out.InputTokens != 1000 || out.CacheReadTokens != 0 {
			t.Errorf("zero cached must leave usage untouched, got in=%d read=%d",
				out.InputTokens, out.CacheReadTokens)
		}
		if out.ProviderNote != "(no cache support)" {
			t.Errorf("ProviderNote = %q, want the no-cache note", out.ProviderNote)
		}
	})
	t.Run("cached clamped to prompt total", func(t *testing.T) {
		out := Response{InputTokens: 500}
		applyOpenAICacheUsage(&out, 500, 9999)
		if out.InputTokens != 0 || out.CacheReadTokens != 500 {
			t.Errorf("over-large cached must clamp, got in=%d read=%d",
				out.InputTokens, out.CacheReadTokens)
		}
	})
}
