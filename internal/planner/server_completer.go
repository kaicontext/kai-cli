package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/ai"
)

// ServerCompleter calls the kailab-control LLM proxy at
// /api/v1/llm/messages instead of going direct to api.anthropic.com.
// This is the production path: developers don't hold their own
// Anthropic API key, the server does, and the server can do rate
// limiting / billing / audit / caching in one place.
//
// Compared to NewAIAdapter (direct-to-Anthropic), the wire format is
// identical — kailab-control's handler forwards verbatim — so the
// only differences here are the URL and the auth header.
//
// When the configured model is an OpenAI/Groq model id (detected by
// provider.IsOpenAIModel), Complete() routes to /api/v1/llm/completions
// using the OpenAI chat-completions wire format instead.
type ServerCompleter struct {
	BaseURL    string       // kailab-control base URL, e.g. https://kaicontext.com
	AuthToken  string       // Bearer token from ~/.kai/credentials.json
	Model      string       // model id (Anthropic or OpenAI/Groq)
	HTTPClient *http.Client // optional; defaults to a 120s-timeout client
}

// NewServerCompleter constructs a Completer that routes through the
// kai-server. BaseURL must be the kailab-control endpoint (NOT a
// per-repo data-plane URL); AuthToken must be a valid bearer token
// (typically from remote.GetValidAccessToken); model picks which
// model to use ("Qwen/Qwen3.5-397B-A17B", "claude-sonnet-4-6",
// "gpt-5.5", etc.). Empty model falls back to a sensible default.
func NewServerCompleter(baseURL, authToken, model string) *ServerCompleter {
	if model == "" {
		model = "deepseek/deepseek-v4-pro"
	}
	return &ServerCompleter{
		BaseURL:   strings.TrimSuffix(baseURL, "/"),
		AuthToken: authToken,
		Model:     model,
		HTTPClient: &http.Client{
			// 120s matches the server-side timeout to api.anthropic.com.
			// One end timing out before the other masks the real cause.
			Timeout: 120 * time.Second,
		},
	}
}

// Complete satisfies the Completer interface used by Plan / Replan.
// For OpenAI/Groq model ids it posts to /api/v1/llm/completions using
// the OpenAI chat-completions shape; for Anthropic models it posts to
// /api/v1/llm/messages using the Anthropic shape.
func (c *ServerCompleter) Complete(system string, messages []ai.Message, maxTokens int) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("planner: server completer has no BaseURL")
	}
	if c.AuthToken == "" {
		return "", fmt.Errorf("planner: server completer has no auth token (run `kai auth login`)")
	}

	if provider.IsOpenAIModel(c.Model) {
		return c.completeOpenAI(system, messages, maxTokens)
	}
	return c.completeAnthropic(system, messages, maxTokens)
}

// completeAnthropic uses the Anthropic-shaped wire format and posts to
// /api/v1/llm/messages. This is the original (unchanged) behaviour.
func (c *ServerCompleter) completeAnthropic(system string, messages []ai.Message, maxTokens int) (string, error) {
	// Match the schema kailab-control's handler validates: model,
	// max_tokens, messages required; system optional. Model is set
	// to a sane default; the planner caller will eventually thread
	// its config through if model selection moves into the wire.
	body, err := json.Marshal(map[string]interface{}{
		"model":      c.Model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   messages,
	})
	if err != nil {
		return "", fmt.Errorf("planner: marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1/llm/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("planner: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AuthToken)

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("planner: sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("planner: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Surface the upstream message verbatim — the kailab-control
		// proxy forwards Anthropic's error responses, so this gives
		// the user the actual upstream reason (rate-limited, model
		// invalid, etc.) rather than a generic wrapper.
		return "", fmt.Errorf("planner: server returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Parse Anthropic's response shape and return the first text block.
	// Same shape as ai.Response so we reuse it directly.
	var apiResp ai.Response
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("planner: unmarshaling response: %w", err)
	}
	if apiResp.Error != nil {
		return "", fmt.Errorf("planner: API error: %s: %s",
			apiResp.Error.Type, apiResp.Error.Message)
	}
	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("planner: empty response")
	}
	return apiResp.Content[0].Text, nil
}

// openAIMessage is a single entry in the OpenAI chat-completions
// messages array.
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAICompletionsRequest is the OpenAI chat-completions request
// body sent to /api/v1/llm/completions.
type openAICompletionsRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []openAIMessage `json:"messages"`
}

// openAICompletionsResponse is the minimal subset of the OpenAI
// chat-completions response we need: choices[0].message.content.
type openAICompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// completeOpenAI uses the OpenAI chat-completions shaped wire format
// and posts to /api/v1/llm/completions.
func (c *ServerCompleter) completeOpenAI(system string, messages []ai.Message, maxTokens int) (string, error) {
	// Build the messages array; prepend system as a system-role message
	// if non-empty (OpenAI does not have a separate top-level system key).
	var oaiMessages []openAIMessage
	if system != "" {
		oaiMessages = append(oaiMessages, openAIMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		oaiMessages = append(oaiMessages, openAIMessage{Role: m.Role, Content: m.Content})
	}

	body, err := json.Marshal(openAICompletionsRequest{
		Model:     c.Model,
		MaxTokens: maxTokens,
		Messages:  oaiMessages,
	})
	if err != nil {
		return "", fmt.Errorf("planner: marshaling openai request: %w", err)
	}

	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1/llm/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("planner: building openai request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AuthToken)

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("planner: sending openai request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("planner: reading openai response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("planner: server returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var apiResp openAICompletionsResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("planner: unmarshaling openai response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("planner: empty openai response (no choices)")
	}
	return apiResp.Choices[0].Message.Content, nil
}
