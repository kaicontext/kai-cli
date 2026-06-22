package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kai/internal/agent/message"
)

// TestAnthropic_SendHappyPath confirms the direct provider sets the
// expected URL path, x-api-key + anthropic-version headers, and
// parses a normal Messages API response into our internal Response
// shape — including a non-zero EstimatedCostUSD when the model is
// in the pricing table.
func TestAnthropic_SendHappyPath(t *testing.T) {
	var sawAuth, sawVersion, sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("x-api-key")
		sawVersion = r.Header.Get("anthropic-version")
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"hello"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":100,"output_tokens":20,
			         "cache_creation_input_tokens":0,"cache_read_input_tokens":50}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "secret-key")
	resp, err := a.Send(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawAuth != "secret-key" {
		t.Errorf("x-api-key header: %q", sawAuth)
	}
	if sawVersion == "" {
		t.Errorf("anthropic-version header missing")
	}
	if sawPath != "/v1/messages" {
		t.Errorf("path: %q", sawPath)
	}
	if resp.InputTokens != 100 || resp.OutputTokens != 20 || resp.CacheReadTokens != 50 {
		t.Errorf("usage: %+v", resp)
	}
	if resp.EstimatedCostUSD <= 0 {
		t.Errorf("expected non-zero cost for known model, got %v", resp.EstimatedCostUSD)
	}
}

// TestAnthropic_RejectsMissingKey verifies the constructor allows
// an empty key (validation happens at provider.New) but Send fails
// loudly rather than silently routing an unauthenticated request.
func TestAnthropic_RejectsMissingKey(t *testing.T) {
	a := NewAnthropic("https://example.invalid", "")
	_, err := a.Send(context.Background(), Request{Model: "claude"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error, got %v", err)
	}
}

// TestAnthropicCost_UnknownModelReturnsZero pins the policy: when
// we don't have pricing for a model id, return 0 (let the trailer
// fall back) instead of inventing a number.
func TestAnthropicCost_UnknownModelReturnsZero(t *testing.T) {
	got := anthropicCost("claude-mystery-9000", Response{InputTokens: 1000, OutputTokens: 500})
	if got != 0 {
		t.Errorf("unknown model should be 0, got %v", got)
	}
}

// TestNormalizeAnthropicModel ensures snapshot suffixes collapse to
// the family key so a new dated release inherits family pricing.
func TestNormalizeAnthropicModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-6":          "claude-sonnet-4-6",
		"claude-sonnet-4-6-20251015": "claude-sonnet-4-6",
		"claude-opus-4-7":            "claude-opus-4-7",
		"claude-opus-4-7-20260101":   "claude-opus-4-7",
		"some-non-snapshot":          "some-non-snapshot",
	}
	for in, want := range cases {
		if got := normalizeAnthropicModel(in); got != want {
			t.Errorf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}

// TestAnthropic_StreamingRoundTrip drives the Anthropic-direct SSE
// path with the same fixture shape kailab uses, since the wire
// format is identical. Adds a check that EstimatedCostUSD is set on
// the final event when the model is known.
func TestAnthropic_StreamingRoundTrip(t *testing.T) {
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
		send(`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
		send(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		send(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
		send(`{"type":"content_block_stop","index":0}`)
		send(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		send(`{"type":"message_stop"}`)
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k")
	ch, err := a.SendStream(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 50,
		Messages: []message.Message{{
			Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		}},
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	var final *Response
	for ev := range ch {
		if ev.Kind == "done" {
			final = ev.Final
		}
		if ev.Kind == "error" {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if final == nil {
		t.Fatal("no done event")
	}
	if final.InputTokens != 10 || final.OutputTokens != 4 {
		t.Errorf("usage: %+v", final)
	}
	if final.EstimatedCostUSD <= 0 {
		t.Errorf("expected cost on stream final, got %v", final.EstimatedCostUSD)
	}
}
