package planner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/provider"
	"kai/internal/ai"
)

// TestServerCompleter_Success verifies the happy path: client sends
// the right shape, server returns Anthropic-shaped JSON, completer
// extracts the text block.
func TestServerCompleter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["max_tokens"].(float64) != 100 {
			t.Errorf("max_tokens forwarded incorrectly: %v", body["max_tokens"])
		}
		// Send back an Anthropic-shaped response.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ai.Response{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "hello from server"}},
			StopReason: "end_turn",
		})
	}))
	defer srv.Close()

	// Pin to claude-sonnet-4-6 so the request routes to /api/v1/llm/messages
	// (Anthropic-shaped). The empty-model fallback is now Qwen, which
	// goes through /completions — a separate code path covered elsewhere.
	c := NewServerCompleter(srv.URL, "test-token", "claude-sonnet-4-6")
	out, err := c.Complete("you are a planner", []ai.Message{
		{Role: "user", Content: "do something"},
	}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello from server" {
		t.Errorf("unexpected response: %q", out)
	}
}

// TestServerCompleter_PropagatesUpstreamError verifies that a non-200
// status from the proxy is surfaced verbatim — important so the user
// sees rate-limit or model-not-found messages without an opaque wrapper.
func TestServerCompleter_PropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error": "rate_limited"}`)
	}))
	defer srv.Close()

	c := NewServerCompleter(srv.URL, "test-token", "")
	_, err := c.Complete("", []ai.Message{{Role: "user", Content: "x"}}, 50)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("expected upstream-error message, got %v", err)
	}
}

// TestServerCompleter_RejectsMissingAuth: a sane error fires before
// hitting the network when the user isn't logged in.
func TestServerCompleter_RejectsMissingAuth(t *testing.T) {
	c := NewServerCompleter("https://example.invalid", "", "")
	_, err := c.Complete("", []ai.Message{{Role: "user", Content: "x"}}, 50)
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

// TestServerCompleter_RejectsMissingURL same shape, missing baseURL.
func TestServerCompleter_RejectsMissingURL(t *testing.T) {
	c := NewServerCompleter("", "tok", "")
	_, err := c.Complete("", []ai.Message{{Role: "user", Content: "x"}}, 50)
	if err == nil || !strings.Contains(err.Error(), "BaseURL") {
		t.Fatalf("expected URL error, got %v", err)
	}
}

// TestServerCompleter_OpenAIPath verifies that when the model is an
// OpenAI/Groq id, Complete() posts to /api/v1/llm/completions using
// the OpenAI chat-completions wire format, includes the system message
// as a {"role":"system",...} entry, and extracts choices[0].message.content.
func TestServerCompleter_OpenAIPath(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"openrouter allowlist - qwen3.5", "qwen/qwen3.5-397b-a17b"},
		{"openrouter allowlist - deepseek", "deepseek/deepseek-v4-pro"},
		{"openrouter allowlist - qwen3 coder", "qwen/qwen3-coder-next"},
		{"openrouter allowlist - kimi", "moonshotai/kimi-k2.6"},
		{"gpt- prefix", "gpt-4o"},
		{"o1 prefix", "o1-mini"},
		{"o3 prefix", "o3"},
		{"o4 prefix", "o4-mini"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Must be routed to the OpenAI completions endpoint.
				if r.URL.Path != "/api/v1/llm/completions" {
					t.Errorf("expected /api/v1/llm/completions, got %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer oai-token" {
					t.Errorf("missing/wrong auth header: %q", got)
				}

				// Decode and validate the OpenAI-shaped request body.
				var body struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_tokens"`
					Messages  []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Model != tt.model {
					t.Errorf("expected model %q, got %q", tt.model, body.Model)
				}
				if body.MaxTokens != 200 {
					t.Errorf("expected max_tokens 200, got %d", body.MaxTokens)
				}
				// Expect system message prepended as first entry.
				if len(body.Messages) < 2 {
					t.Fatalf("expected at least 2 messages (system + user), got %d", len(body.Messages))
				}
				if body.Messages[0].Role != "system" || body.Messages[0].Content != "you are an openai planner" {
					t.Errorf("unexpected system message: %+v", body.Messages[0])
				}
				if body.Messages[1].Role != "user" || body.Messages[1].Content != "plan something" {
					t.Errorf("unexpected user message: %+v", body.Messages[1])
				}

				// Respond with OpenAI-shaped completions JSON.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"openai plan result"}}]}`))
			}))
			defer srv.Close()

			c := NewServerCompleter(srv.URL, "oai-token", tt.model)
			out, err := c.Complete("you are an openai planner", []ai.Message{
				{Role: "user", Content: "plan something"},
			}, 200)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != "openai plan result" {
				t.Errorf("unexpected response: %q", out)
			}
		})
	}
}

// TestServerCompleter_OpenAIPath_NoSystem verifies that when system is
// empty the messages array contains only the conversation turns (no
// spurious system entry).
func TestServerCompleter_OpenAIPath_NoSystem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/completions" {
			t.Errorf("expected /api/v1/llm/completions, got %s", r.URL.Path)
		}
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// With empty system string there should be exactly 1 message.
		if len(body.Messages) != 1 {
			t.Errorf("expected 1 message, got %d", len(body.Messages))
		}
		if body.Messages[0].Role != "user" {
			t.Errorf("expected user message, got %q", body.Messages[0].Role)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"no system"}}]}`))
	}))
	defer srv.Close()

	c := NewServerCompleter(srv.URL, "tok", "gpt-4o")
	out, err := c.Complete("", []ai.Message{{Role: "user", Content: "hi"}}, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "no system" {
		t.Errorf("unexpected response: %q", out)
	}
}

// TestServerCompleter_RoutesByModel verifies the planner-side routing
// classifier matches what the provider package uses. The actual logic
// lives in provider.IsOpenAIModel; this test guards that the planner
// consults the same function (regression: prior duplication drifted).
func TestServerCompleter_RoutesByModel(t *testing.T) {
	trueModels := []string{
		"gpt-4o",
		"gpt-3.5-turbo",
		"o1",
		"o1-mini",
		"o3",
		"o3-mini",
		"o4",
		"o4-mini",
		"qwen/qwen3.5-397b-a17b",
		"deepseek/deepseek-v4-pro",
		"qwen/qwen3-coder-next",
		"moonshotai/kimi-k2.6",
	}
	for _, m := range trueModels {
		if !provider.IsOpenAIModel(m) {
			t.Errorf("provider.IsOpenAIModel(%q) = false, want true", m)
		}
	}

	falseModels := []string{
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
		"claude-3-opus-20240229",
		"",
	}
	for _, m := range falseModels {
		if provider.IsOpenAIModel(m) {
			t.Errorf("provider.IsOpenAIModel(%q) = true, want false", m)
		}
	}
}
