package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// kailab → OpenAI multi-provider proxy support.
//
// When a Kailab.Send / SendStream call sees an OpenAI-family model
// id, it routes the request through kailab's `/api/v1/llm/completions`
// endpoint instead of `/api/v1/llm/messages`. The wire format uses
// OpenAI's native chat-completions shape end-to-end (kailab does not
// translate). We reuse openai.go's `buildOpenAIRequest` / `parse-
// OpenAIResponse` / `runOpenAISSE` so the marshaling stays in one
// place; only the URL and auth header change.
//
// The user keeps using the kailab provider — they don't need to know
// about the dual endpoints. Picking gpt-4o as the model is enough.

// KailabOpenRouterModels mirrors the server's openRouterAllowlist (see
// kailab-control internal/api/llm_routing.go). Both ends must agree on
// what's an open-model id; otherwise the CLI sends it to the Anthropic
// endpoint and gets a confusing 400. Keep in lockstep when
// adding/removing models.
//
// Switched from Together to OpenRouter 2026-06-05 (single router,
// cheaper). Slugs are OpenRouter's `vendor/model` form.
var KailabOpenRouterModels = map[string]bool{
	"deepseek/deepseek-v4-pro": true,
	"z-ai/glm-5.1":             true,
	"moonshotai/kimi-k2.6":     true,
	"qwen/qwen3.5-397b-a17b":   true,
	"qwen/qwen3-coder-next":    true,
}

// IsOpenAIModel returns true when a model id should route through
// kailab's `/api/v1/llm/completions` endpoint (OpenAI-shaped wire
// format) rather than `/api/v1/llm/messages` (Anthropic-shaped).
// Mirrors classifyProvider() on the server: OpenAI family + the
// OpenRouter allowlist both go to /completions. Name kept for
// blame-stability even though it now covers more than OpenAI — the
// semantic stayed "should this take the OpenAI-shaped path?"
func IsOpenAIModel(model string) bool {
	if KailabOpenRouterModels[strings.TrimSpace(model)] {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "gpt-") ||
		strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4")
}

// sendOnceOpenAI is the OpenAI counterpart of (Kailab).sendOnce. One
// HTTP round-trip; transient errors come back wrapped so the same
// retry loop in Send() drives both upstreams. Reuses the shared
// openai.go marshal/parse helpers — the only kailab-specific bits
// are the URL and the kailab Bearer token (NOT an OpenAI key).
func (k *Kailab) sendOnceOpenAI(ctx context.Context, body []byte, onState func(RequestState)) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		k.BaseURL+"/api/v1/llm/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("kailab provider (openai): building http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+k.AuthToken)

	client := k.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	if onState != nil {
		// Body-prefix hash for cache-stability diagnosis (2026-05-26).
		// Together's prompt cache requires byte-identical prefixes
		// across requests. cached=0 across all turns of a run means
		// the prefix is varying — this surfaces a short hash of the
		// first 2KB of the body so post-hoc log inspection can spot
		// the drift (or rule prefix-instability out and point at
		// upstream cache TTL). Logged via the existing provider-state
		// stream → planner-debug.log "provider_state" lines.
		const prefixLen = 2000
		n := prefixLen
		if len(body) < n {
			n = len(body)
		}
		digest := sha256.Sum256(body[:n])
		onState(RequestState{
			Phase:  PhaseSent,
			Detail: fmt.Sprintf("POST /api/v1/llm/completions body-prefix-sha=%x (first %d / %d bytes)", digest[:6], n, len(body)),
			When:   time.Now(),
		})
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("kailab provider (openai): sending request: %w", err)}
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseConnected, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), When: time.Now()})
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("kailab provider (openai): reading response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Errorf("kailab provider (openai): %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		if isRetryableStatus(resp.StatusCode) {
			return Response{}, &transientError{cause: errMsg}
		}
		return Response{}, errMsg
	}

	var raw openaiResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Response{}, fmt.Errorf("kailab provider (openai): parsing response: %w", err)
	}
	out := parseOpenAIResponse(raw)
	out.ProviderNote = "(no cache support)"
	return out, nil
}

// sendOpenAI is the openAI-routed equivalent of Send(). It mirrors
// the retry loop but builds an OpenAI-shape body and dispatches to
// sendOnceOpenAI. Splitting per-model rather than per-method keeps
// the existing Anthropic Send() byte-identical — the openai branch
// only fires when the model id matches.
func (k *Kailab) sendOpenAI(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(buildOpenAIRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("kailab provider (openai): marshaling request: %w", err)
	}
	// Client-side mirror of the server's groqReasoning adapter
	// (kailab-control/internal/api/llm_completions.go:151). Lets us
	// validate the fix end-to-end against qwen3 BEFORE shipping the
	// server change. Once the server-side fix is deployed, this
	// becomes a no-op (idempotent — only adds fields the server
	// would have added anyway). Safe to keep for older servers that
	// haven't shipped the floor yet.
	body = adjustForReasoning(body, req.Model)

	maxAttempts := k.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	backoff := k.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := k.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := k.sendOnceOpenAI(ctx, body, req.OnState)
		if err == nil {
			emitState(req, PhaseDone, "", nil)
			return resp, nil
		}
		lastErr = err
		if cerr := ctx.Err(); cerr != nil {
			return Response{}, cerr
		}
		if !isTransient(err) || attempt == maxAttempts {
			emitState(req, PhaseError, err.Error(), err)
			return Response{}, err
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return Response{}, ctx.Err()
		case <-t.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	return Response{}, lastErr
}

// sendStreamOpenAI is the streaming counterpart. POSTs to
// /api/v1/llm/completions with stream=true; lets kailab inject the
// stream_options.include_usage flag (or honors what the client set
// — kailab doesn't override an explicit setting). We don't retry on
// failure here: the runner re-issues the whole turn on a transient
// stream error rather than trying to resume mid-response.
func (k *Kailab) sendStreamOpenAI(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	body, err := json.Marshal(buildOpenAIRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("kailab provider (openai stream): marshaling request: %w", err)
	}
	body = adjustForReasoning(body, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		k.BaseURL+"/api/v1/llm/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kailab provider (openai stream): building http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+k.AuthToken)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := k.HTTPClient
	if client == nil || client.Timeout > 0 {
		client = &http.Client{}
	}
	emitState(req, PhaseSent, "POST /api/v1/llm/completions (stream)", nil)
	resp, err := client.Do(httpReq)
	if err != nil {
		emitState(req, PhaseError, "opening stream: "+err.Error(), err)
		return nil, fmt.Errorf("kailab provider (openai stream): opening stream: %w", err)
	}
	emitState(req, PhaseConnected, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("kailab provider (openai stream): %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	out := make(chan StreamEvent, 32)
	go runOpenAISSE(resp, out, req.Tools, req.OnState)
	return out, nil
}

// isReasoningModel reports whether a model id is a reasoning-class
// model — one that emits a hidden chain-of-thought, consuming output
// tokens BEFORE any visible text. These need a max_tokens floor (see
// adjustForReasoning) so the reasoning step doesn't starve the visible
// answer and produce the "Model returned no text" empty completion.
//
// CANONICAL classifier: agent.isReasoningModel delegates here so the
// two cannot drift. They DID drift (2026-05-29): this copy matched only
// Qwen3, so deepseek-ai/DeepSeek-V4-Pro was never recognized as
// reasoning, never got the floor, and silently returned empty
// completions / truncated answers — while the agent-side copy already
// knew it was a reasoning model. Match families case-insensitively here
// (the upstream-id case sensitivity is handled at request time).
//
// Keep in sync with the server-side helper in
// kailab-control/internal/api/llm_completions.go.
func isReasoningModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "qwen3"), strings.Contains(m, "qwen2.5"):
		return true
	case strings.HasPrefix(m, "gpt-5"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return true
	case strings.Contains(m, "deepseek-r1"), strings.Contains(m, "deepseek-r2"), strings.Contains(m, "deepseek-v4"):
		return true
	case strings.Contains(m, "reasoning"), strings.Contains(m, "-r1"), strings.Contains(m, "-r2"):
		return true
	}
	return false
}

// IsReasoningModel is the exported form of the canonical reasoning-model
// classifier. Other packages (notably internal/agent) call this instead
// of maintaining a divergent copy — see the drift note on
// isReasoningModel.
func IsReasoningModel(model string) bool { return isReasoningModel(model) }

// reasoningMaxTokensFloor is the minimum max_tokens we'll send
// upstream for a reasoning model when the response IS schema-
// constrained (planner JSON-schema mode). Qwen3-class models
// silently spend 200+ tokens on internal reasoning per response;
// 4096 leaves room for the reasoning step + a structured-output
// JSON body. Clients may raise; we never lower.
const reasoningMaxTokensFloor = 4096

// reasoningChatMaxTokensFloor is the minimum max_tokens we'll
// send upstream for a reasoning model in free-form chat mode (no
// response_format constraint). Without a schema cap, the model
// has to both reason AND emit a real prose answer; 4096 is enough
// for the reasoning step but routinely leaves zero tokens for the
// visible reply on complex meta questions — exactly the "Model
// returned no text" pathology the user hit in the 2026-05-15
// dogfood. 16384 gives both phases room.
//
// Two floors instead of one because: planner has a JSON schema
// cap that bounds output length naturally; chat does not. Same
// model, different request shape, different sane defaults.
const reasoningChatMaxTokensFloor = 16384

// adjustForReasoning mirrors the server's floorMaxTokens behavior
// on the CLI side. For a reasoning model id, floor max_tokens
// based on whether the request is schema-constrained (planner) or
// free-form (chat). Skips when the client already set max_tokens
// higher than the relevant floor.
//
// Together doesn't expose Groq's reasoning_format parameter — their
// API strips <think> blocks by default — so we no longer inject that
// field (it would just be an unknown param). If a future Together
// model needs an explicit format hint, extend this hook.
//
// Once the server-side floor lands, this becomes effectively a
// no-op (the server still applies the floor; we'd just set it to
// the same value first). Keeping the client-side path means older
// servers without the floor can still serve reasoning models.
func adjustForReasoning(body []byte, model string) []byte {
	if !isReasoningModel(model) {
		return body
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	// Schema-constrained requests (planner) need a smaller floor
	// because the JSON schema bounds output length. Free-form chat
	// has no such bound — the model can run the full think+answer
	// pipeline and produce arbitrary prose, so it gets the bigger
	// floor.
	floor := reasoningChatMaxTokensFloor
	if rf, ok := raw["response_format"].(map[string]interface{}); ok {
		if t, _ := rf["type"].(string); t == "json_schema" {
			floor = reasoningMaxTokensFloor
		}
	}
	switch cur := raw["max_tokens"].(type) {
	case float64:
		if int(cur) < floor {
			raw["max_tokens"] = floor
		}
	case nil:
		raw["max_tokens"] = floor
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}
