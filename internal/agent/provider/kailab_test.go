package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/tools"
)

// TestKailab_StreamingRoundTrip drives a fake kailab server that
// emits Anthropic-shaped SSE frames and asserts the SendStream
// channel produces text deltas + a final done event with the
// aggregated Response. Pins the SSE parser against the Anthropic
// Messages API event vocabulary so an upstream change is caught.
func TestKailab_StreamingRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		send := func(data string) {
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		send(`{"type":"message_start","message":{"usage":{"input_tokens":42}}}`)
		send(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		send(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
		send(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`)
		send(`{"type":"content_block_stop","index":0}`)
		// Anthropic places usage at the TOP LEVEL of message_delta
		// (not under delta). Earlier versions of this test had it
		// nested, which masked the parser bug for months.
		send(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
		send(`{"type":"message_stop"}`)
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	ch, err := k.SendStream(context.Background(), Request{
		Model:     "claude",
		MaxTokens: 100,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	var deltas []string
	var final *Response
	for ev := range ch {
		switch ev.Kind {
		case "text_delta":
			deltas = append(deltas, ev.Text)
		case "done":
			final = ev.Final
		case "error":
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Errorf("deltas concatenated: %q", got)
	}
	if final == nil {
		t.Fatal("no done event")
	}
	if final.InputTokens != 42 || final.OutputTokens != 7 {
		t.Errorf("usage: in=%d out=%d", final.InputTokens, final.OutputTokens)
	}
	if final.FinishReason != message.FinishReasonEndTurn {
		t.Errorf("finish reason: %v", final.FinishReason)
	}
	if len(final.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(final.Parts))
	}
	if t1, ok := final.Parts[0].(message.TextContent); !ok || t1.Text != "Hello world" {
		t.Errorf("part: %+v", final.Parts[0])
	}
}

// TestKailab_RetriesTransientThenSucceeds: a 429 followed by a 200
// completes in two attempts. Backoff is shrunk to a millisecond so
// the test stays fast.
func TestKailab_RetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	k.InitialBackoff = time.Millisecond
	k.MaxBackoff = 5 * time.Millisecond

	resp, err := k.Send(context.Background(), Request{
		Model:     "claude",
		MaxTokens: 10,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 retry), got %d", calls)
	}
	if len(resp.Parts) != 1 {
		t.Errorf("expected 1 content part, got %d", len(resp.Parts))
	}
}

// TestKailab_DoesNotRetry400: 400 is a client mistake (bad model
// name, malformed body). Retrying just wastes time and confuses the
// user about the real error.
func TestKailab_DoesNotRetry400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	k.InitialBackoff = time.Millisecond

	_, err := k.Send(context.Background(), Request{
		Model:     "claude",
		MaxTokens: 10,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on 400), got %d", calls)
	}
}

// TestKailab_GivesUpAfterMaxAttempts: persistent 529 surfaces the
// last error after exhausting the retry budget.
func TestKailab_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`overloaded`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	k.InitialBackoff = time.Millisecond
	k.MaxBackoff = 5 * time.Millisecond
	k.MaxAttempts = 3

	_, err := k.Send(context.Background(), Request{
		Model:     "claude",
		MaxTokens: 10,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatalf("expected error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "529") {
		t.Errorf("expected 529 in error, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

// TestKailab_TranslatesRequestAndResponse drives a full round-trip
// against a fake kai-server: sends an Anthropic-shaped request, gets
// back a tool_use block, parses it. Pins the wire format so when
// Anthropic adds new content-block types we know to update.
func TestKailab_TranslatesRequestAndResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing auth header: %q", got)
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req["model"] != "claude-sonnet-4-6" {
			t.Errorf("model wrong: %v", req["model"])
		}
		// Tools serialized as expected
		toolsList, _ := req["tools"].([]interface{})
		if len(toolsList) != 1 {
			t.Fatalf("expected 1 tool, got %v", toolsList)
		}
		// Messages: one user message with one text block
		msgs, _ := req["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %v", msgs)
		}

		// Send back a synthetic tool_use response
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_1","type":"message","role":"assistant",
			"content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"toolu_1","name":"view","input":{"file_path":"x.go"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":50,"output_tokens":12}
		}`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "test-token")
	resp, err := k.Send(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		System:    "you are an agent",
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "look"}}}},
		Tools:     []tools.ToolInfo{{Name: "view", Description: "read file", Parameters: map[string]any{"file_path": map[string]any{"type": "string"}}, Required: []string{"file_path"}}},
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.FinishReason != message.FinishReasonToolUse {
		t.Errorf("expected tool_use, got %s", resp.FinishReason)
	}
	if resp.InputTokens != 50 || resp.OutputTokens != 12 {
		t.Errorf("token counts wrong: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
	if len(resp.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(resp.Parts))
	}
	tc, ok := resp.Parts[1].(message.ToolCall)
	if !ok {
		t.Fatalf("expected ToolCall, got %T", resp.Parts[1])
	}
	if tc.Name != "view" || !strings.Contains(tc.Input, "x.go") {
		t.Errorf("tool call wrong: %+v", tc)
	}
}

func TestKailab_PropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	}))
	defer srv.Close()
	k := NewKailab(srv.URL, "tok")
	_, err := k.Send(context.Background(), Request{
		Model:     "x",
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "x"}}}},
		MaxTokens: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("expected upstream error to be forwarded, got %v", err)
	}
}

func TestKailab_RejectsMissingAuth(t *testing.T) {
	k := NewKailab("https://example.invalid", "")
	_, err := k.Send(context.Background(), Request{Model: "x", MaxTokens: 1})
	if err == nil || !strings.Contains(err.Error(), "logged in") {
		t.Fatalf("expected login hint, got %v", err)
	}
}

func TestKailab_SerializesToolResults(t *testing.T) {
	// Verify that tool_result messages translate to the wire shape
	// Anthropic expects (block under user role with tool_use_id).
	captured := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	k := NewKailab(srv.URL, "tok")
	_, err := k.Send(context.Background(), Request{
		Model: "x",
		Messages: []message.Message{
			{Role: message.RoleUser, Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "tu_1", Name: "view", Content: "alpha"},
			}},
		},
		MaxTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_result"`, `"tool_use_id":"tu_1"`, `"content":"alpha"`} {
		if !strings.Contains(captured, want) {
			t.Errorf("serialized body missing %s\nfull: %s", want, captured)
		}
	}
}

// readAll is a tiny helper to avoid an extra import inline.
func readAll(r interface {
	Read([]byte) (int, error)
}) ([]byte, error) {
	var b []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b = append(b, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return b, nil
			}
			return b, err
		}
	}
}
