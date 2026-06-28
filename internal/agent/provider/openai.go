package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/tools"
)

// OpenAI talks to any OpenAI-compatible chat.completions endpoint.
// Real OpenAI, Together, Groq, vLLM, Ollama (via /v1), LM Studio
// — all use the same protocol. Per-vendor differences (rate limits,
// model availability, tool-use fidelity) surface as upstream errors.
//
// Two structural differences from the Anthropic providers:
//
//  1. No cache *control*. The protocol has no concept of cache
//     breakpoints, so kai can't pin a prefix. But the usage block
//     does report a read-only `prompt_tokens_details.cached_tokens`
//     for vendors that cache server-side (Together, OpenAI): we
//     surface that as CacheReadTokens. Only when a turn reports zero
//     cached tokens do we fall back to the "(no cache support)"
//     ProviderNote, so a real 0 doesn't read as a failure.
//
//  2. Tool calls and tool results are first-class on different
//     message roles than Anthropic's content-block design. We
//     translate both directions: kai's ToolCall content parts
//     become assistant.tool_calls; kai's ToolResult parts become
//     role:"tool" messages.
//
// We do NOT attempt to parse tool calls from text fallback. If a
// model emits "I'll call read_file with path=foo" instead of a
// real tool_call, the runner sees no call, treats it as a normal
// turn, and the user gets a clear "model didn't call a tool"
// outcome. This is preferable to a brittle regex extractor that
// produces subtly-wrong dispatches.
type OpenAI struct {
	BaseURL    string
	APIKey     string // optional — local servers may not require auth
	HTTPClient *http.Client

	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxAttempts    int
}

func NewOpenAI(baseURL, apiKey string) *OpenAI {
	return &OpenAI{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: sharedHTTPClient(),
	}
}

// SupportsCache: the OpenAI chat.completions protocol has no
// concept of cache breakpoints. Some vendors (Anthropic via
// Bedrock, Together, etc.) layer caching server-side, but it's
// invisible to us — we report no cache support so the planner
// doesn't over-budget and the trailer is honest.
func (o *OpenAI) SupportsCache() bool { return false }

func (o *OpenAI) Send(ctx context.Context, req Request) (Response, error) {
	if o.BaseURL == "" {
		return Response{}, fmt.Errorf("openai provider: BaseURL not set")
	}
	if req.Model == "" {
		return Response{}, fmt.Errorf("openai provider: Model required")
	}

	body, err := json.Marshal(buildOpenAIRequest(req, false))
	if err != nil {
		return Response{}, fmt.Errorf("openai provider: marshaling request: %w", err)
	}

	maxAttempts := o.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	backoff := o.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := o.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := o.sendOnce(ctx, body, req.OnState)
		if err == nil {
			applyTextCallFallback(&resp, req.Tools)
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

func (o *OpenAI) sendOnce(ctx context.Context, body []byte, onState func(RequestState)) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("openai provider: building http request: %w", err)
	}
	o.applyHeaders(httpReq)

	client := o.HTTPClient
	if client == nil {
		client = sharedHTTPClient()
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseSent, Detail: "POST /chat/completions", When: time.Now()})
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("openai provider: sending request: %w", err)}
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseConnected, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), When: time.Now()})
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("openai provider: reading response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Errorf("openai provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		if isRetryableStatus(resp.StatusCode) {
			return Response{}, &transientError{cause: errMsg}
		}
		return Response{}, errMsg
	}

	rawDump("send", string(respBody))
	var raw openaiResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Response{}, fmt.Errorf("openai provider: parsing response: %w", err)
	}
	out := parseOpenAIResponse(raw)
	return out, nil
}

func (o *OpenAI) SendStream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	if o.BaseURL == "" {
		return nil, fmt.Errorf("openai provider: BaseURL not set")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("openai provider: Model required")
	}

	raw, err := json.Marshal(buildOpenAIRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("openai provider: marshaling stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		o.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("openai provider: building stream request: %w", err)
	}
	o.applyHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := o.HTTPClient
	if client == nil || client.Timeout > 0 {
		client = &http.Client{}
	}
	emitState(req, PhaseSent, "POST /chat/completions (stream)", nil)
	resp, err := client.Do(httpReq)
	if err != nil {
		emitState(req, PhaseError, "opening stream: "+err.Error(), err)
		return nil, fmt.Errorf("openai provider: opening stream: %w", err)
	}
	emitState(req, PhaseConnected, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		err := fmt.Errorf("openai provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		emitState(req, PhaseError, err.Error(), err)
		return nil, err
	}

	out := make(chan StreamEvent, 32)
	go runOpenAISSE(resp, out, req.Tools, req.OnState)
	return out, nil
}

func (o *OpenAI) applyHeaders(httpReq *http.Request) {
	httpReq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
}

// --- request translation ---------------------------------------------

// buildOpenAIRequest converts the Anthropic-shaped Request into the
// chat.completions body. The mapping:
//
//   - System prompt → leading {role:"system", content: <string>}
//   - User/assistant message with TextContent → {role, content: <string>}
//   - Assistant message with ToolCall parts → {role:"assistant",
//     content: <text or null>, tool_calls: [...]}
//   - User message with ToolResult parts → one {role:"tool",
//     tool_call_id, content} message PER tool result (OpenAI's
//     protocol requires individual tool messages, not bundled)
//   - Tools → top-level "tools" array with {type:"function",
//     function:{name, description, parameters}}
func buildOpenAIRequest(req Request, stream bool) map[string]interface{} {
	msgs := openaiSerializeMessages(req.System, req.Messages)
	out := map[string]interface{}{
		"model":      req.Model,
		"messages":   msgs,
		"max_tokens": req.MaxTokens,
	}
	if stream {
		out["stream"] = true
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		out["tools"] = openaiSerializeTools(req.Tools)
	}
	// Together AI has normalized response_format: json_schema across
	// their entire catalog, so when the caller asked for structured
	// output (req.OutputJSONSchema set), attach it here. The upstream
	// (kailab proxy → Together) understands this field and constrains
	// the model's final message to match. Providers that don't
	// implement it typically ignore unknown top-level params; if a
	// future provider hard-rejects this, gate by model id here.
	if len(req.OutputJSONSchema) > 0 {
		out["response_format"] = map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "structured_output",
				"schema": req.OutputJSONSchema,
				"strict": true,
			},
		}
	}
	return out
}

func openaiSerializeTools(ts []tools.ToolInfo) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(ts))
	for _, t := range ts {
		schema := map[string]interface{}{
			"type":       "object",
			"properties": t.Parameters,
		}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	return out
}

func openaiSerializeMessages(system string, msgs []message.Message) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs)+1)
	if s := strings.TrimSpace(system); s != "" {
		out = append(out, map[string]interface{}{
			"role":    "system",
			"content": s,
		})
	}
	for _, m := range msgs {
		if m.Role == message.RoleSystem {
			continue
		}

		// Walk the parts once, separating text / tool_calls / tool_results.
		var (
			textBuf   strings.Builder
			toolCalls []map[string]interface{}
			results   []message.ToolResult
		)
		for _, p := range m.Parts {
			switch v := p.(type) {
			case message.TextContent:
				textBuf.WriteString(v.Text)
			case message.ToolCall:
				args := strings.TrimSpace(v.Input)
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   v.ID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      v.Name,
						"arguments": args,
					},
				})
			case message.ToolResult:
				results = append(results, v)
			}
		}

		// Tool results become standalone role:"tool" messages.
		// Anthropic packs them inside a user message; OpenAI requires
		// each one as its own entry with tool_call_id set.
		for _, r := range results {
			out = append(out, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": r.ToolCallID,
				"content":      r.Content,
			})
		}

		// If after extracting tool_results the message is otherwise
		// empty, skip it — adding an empty user/assistant message
		// would just confuse the model.
		hasText := strings.TrimSpace(textBuf.String()) != ""
		hasCalls := len(toolCalls) > 0
		if !hasText && !hasCalls {
			continue
		}

		entry := map[string]interface{}{
			"role": string(m.Role),
		}
		if hasText {
			entry["content"] = textBuf.String()
		} else {
			// OpenAI's API accepts null content when tool_calls are
			// present and there's no accompanying prose.
			entry["content"] = nil
		}
		if hasCalls {
			entry["tool_calls"] = toolCalls
		}
		out = append(out, entry)
	}
	return out
}

// --- response translation --------------------------------------------

type openaiResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // DeepSeek-R1/V4-Pro, Qwen3-style hidden CoT
			Reasoning        string `json:"reasoning"`         // Together/Groq alt key for the same content
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseOpenAIResponse(raw openaiResponse) Response {
	out := Response{
		InputTokens:      raw.Usage.PromptTokens,
		OutputTokens:     raw.Usage.CompletionTokens,
		EstimatedCostUSD: raw.Usage.Cost,
	}
	applyOpenAICacheUsage(&out, raw.Usage.PromptTokens, raw.Usage.PromptTokensDetails.CachedTokens)
	if len(raw.Choices) == 0 {
		out.FinishReason = message.FinishReasonUnknown
		return out
	}
	c := raw.Choices[0]
	// Reasoning content (DeepSeek-R1/V4-Pro emits hidden CoT under
	// reasoning_content; Together/Groq uses 'reasoning'). Preserved in
	// the transcript so the debug log can show what the model was
	// thinking when it decided whether to call a tool — answers
	// "why didn't it use tools?" by surfacing the model's pre-output
	// rationale instead of leaving us to guess from priors.
	if thinking := c.Message.ReasoningContent; thinking != "" {
		out.Parts = append(out.Parts, message.ReasoningContent{Thinking: thinking})
	} else if thinking := c.Message.Reasoning; thinking != "" {
		out.Parts = append(out.Parts, message.ReasoningContent{Thinking: thinking})
	}
	if c.Message.Content != "" {
		// Strip DeepSeek's leaked tool-call delimiters; see dsml_filter.go.
		out.Parts = append(out.Parts, message.TextContent{Text: stripDSMLLeak(c.Message.Content)})
	}
	for _, tc := range c.Message.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.Parts = append(out.Parts, message.ToolCall{
			ID:       tc.ID,
			Name:     tc.Function.Name,
			Input:    args,
			Type:     "tool_use",
			Finished: true,
		})
	}
	out.FinishReason = mapOpenAIFinishReason(c.FinishReason)
	return out
}

// applyOpenAICacheUsage records server-side prompt caching from an
// OpenAI-compatible usage block. The chat.completions protocol gives
// no cache-breakpoint *control* (SupportsCache stays false), but it
// does report a read-only `prompt_tokens_details.cached_tokens` count:
// Together, real OpenAI, and others automatically reuse a prompt
// prefix and tell us how much of it hit. Unlike Anthropic, OpenAI's
// `prompt_tokens` is the FULL prompt (cached + new), so the
// billing-aligned new-input figure is prompt_tokens − cached_tokens.
//
// Without this, every Together/Kimi turn reported cached=0 even when
// the provider was reusing the prompt — the K2.6 benchmark's
// "cached=0 on every turn" was this blind spot, not a real miss.
func applyOpenAICacheUsage(out *Response, promptTokens, cachedTokens int) {
	if cachedTokens <= 0 {
		// No cache read reported — either a genuine miss or a vendor
		// that omits the field. Keep the honest "no cache" note so a
		// zero doesn't read as a failure.
		out.ProviderNote = "(no cache support)"
		return
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}
	out.InputTokens = promptTokens - cachedTokens
	out.CacheReadTokens = cachedTokens
	out.CachedInputTokens = cachedTokens // OpenAI reports no creation split
	out.ProviderNote = ""
}

func mapOpenAIFinishReason(r string) message.FinishReason {
	switch r {
	case "stop":
		return message.FinishReasonEndTurn
	case "tool_calls", "function_call":
		return message.FinishReasonToolUse
	case "length":
		return message.FinishReasonMaxTokens
	default:
		return message.FinishReasonUnknown
	}
}

// --- streaming -------------------------------------------------------

// openaiStreamState accumulates SSE delta chunks for OpenAI's
// chat.completions stream. Different shape from Anthropic: every
// delta is a small JSON envelope under choices[0].delta with either
// "content" (text) or "tool_calls" (incremental tool dispatch).
// Tool-call args stream one chunk at a time keyed by index.
type openaiStreamState struct {
	text       strings.Builder
	reasoning  strings.Builder // accumulates delta.reasoning_content / delta.reasoning for reasoning-class models
	tools      []*openaiToolBuilder // index-aligned with delta.tool_calls[*].index
	stopReason string
	inTok      int
	outTok     int
	cachedTok  int
	costUSD    float64
}

type openaiToolBuilder struct {
	id   string
	name string
	args strings.Builder
}

func (s *openaiStreamState) ensureTool(idx int) *openaiToolBuilder {
	for len(s.tools) <= idx {
		s.tools = append(s.tools, &openaiToolBuilder{})
	}
	return s.tools[idx]
}

func (s *openaiStreamState) finalize() *Response {
	out := &Response{
		FinishReason:     mapOpenAIFinishReason(s.stopReason),
		InputTokens:      s.inTok,
		OutputTokens:     s.outTok,
		EstimatedCostUSD: s.costUSD,
	}
	applyOpenAICacheUsage(out, s.inTok, s.cachedTok)
	// Reasoning content first so the debug log shows the model's
	// pre-output rationale before its visible text. Ignored for tool
	// dispatch (matches Anthropic path); preserved in transcript.
	if s.reasoning.Len() > 0 {
		out.Parts = append(out.Parts, message.ReasoningContent{Thinking: s.reasoning.String()})
	}
	if s.text.Len() > 0 {
		out.Parts = append(out.Parts, message.TextContent{Text: s.text.String()})
	}
	for _, t := range s.tools {
		if t == nil || t.id == "" {
			continue
		}
		args := t.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.Parts = append(out.Parts, message.ToolCall{
			ID:       t.id,
			Name:     t.name,
			Input:    args,
			Type:     "tool_use",
			Finished: true,
		})
	}
	return out
}

// rawDump is a diagnostic sink for the unparsed provider wire format.
// When KAI_RAW_DUMP names a file, every chat.completions response body
// and every SSE line is appended to it verbatim. It exists to tell a
// model-side serialization bug (the model emitted a tool call in the
// text channel) apart from a kai-side parse bug (the SSE parser
// misrouted tool_calls deltas into content). Off unless the env var is
// set; the file is opened once and shared, mutex-guarded, across the
// process's concurrent provider calls.
var (
	rawDumpOnce sync.Once
	rawDumpFile *os.File
	rawDumpMu   sync.Mutex
)

func rawDump(tag, payload string) {
	path := os.Getenv("KAI_RAW_DUMP")
	if path == "" {
		return
	}
	rawDumpOnce.Do(func() {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			rawDumpFile = f
		}
	})
	if rawDumpFile == nil {
		return
	}
	rawDumpMu.Lock()
	defer rawDumpMu.Unlock()
	fmt.Fprintf(rawDumpFile, "%s [%s] %s\n", time.Now().Format("15:04:05.000"), tag, payload)
}

func runOpenAISSE(resp *http.Response, out chan<- StreamEvent, allowedTools []tools.ToolInfo, onState func(RequestState)) {
	defer resp.Body.Close()
	defer close(out)

	bumpActivity, stopWatchdog, watchdogFired := sseWatchdog(resp, streamIdleTimeout, onState)
	defer stopWatchdog()
	streamingEmitted := false

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	state := &openaiStreamState{}
	// Per-stream filter that suppresses DeepSeek's `<｜DSML｜…｜>`
	// tool-call delimiters from text deltas, even when the markers
	// split across SSE chunks. See dsml_filter.go.
	dsml := &dsmlFilter{}
	// currentEvent tracks the last `event:` line for the upcoming
	// frame. OpenAI's native SSE doesn't use event names — but
	// kailab injects `event: kai_state` frames to surface upstream-
	// call lifecycle (kailab → api.openai.com). We route those to
	// onState instead of the OpenAI content parser.
	currentEvent := ""
	for scanner.Scan() {
		line := scanner.Text()
		rawDump("sse", line)
		// Bump activity only on `data:` lines — see the long
		// comment in kailab.go's runSSE for why keepalives must
		// not count as content. Keeps both the soft idle hint and
		// the hard abort honest about "are real bytes flowing?"
		if strings.HasPrefix(line, "data:") {
			bumpActivity()
			if !streamingEmitted && onState != nil {
				onState(RequestState{Phase: PhaseStreaming, When: time.Now()})
				streamingEmitted = true
			}
		}
		if line == "" {
			currentEvent = "" // frame boundary
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			// kailab-control's SSE proxy occasionally writes the
			// upstream's HTTP-error body to the stream as raw JSON
			// without the `data: ` prefix — surfaces when Groq
			// returns a 413/429 (TPM/RPM rate limit) for example.
			// Without this branch the line was silently dropped,
			// the stream finished with zero content, and the agent
			// loop reported "no text" instead of the actual error.
			// Detected by the literal `{"error":` prefix to avoid
			// false-positives on comment/blank lines that already
			// got handled above.
			if strings.HasPrefix(strings.TrimSpace(line), `{"error":`) {
				var bare struct {
					Error *struct {
						Type    string `json:"type"`
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(line), &bare) == nil && bare.Error != nil {
					kind := bare.Error.Type
					if bare.Error.Code != "" {
						kind = bare.Error.Code
					}
					out <- StreamEvent{Kind: "error",
						Err: fmt.Errorf("openai provider: %s: %s", kind, bare.Error.Message)}
					return
				}
			}
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if currentEvent == "kai_state" {
			handleKaiStateFrame(data, onState)
			currentEvent = ""
			continue
		}
		if data == "[DONE]" {
			// Some vendors send [DONE] before the final usage chunk;
			// others send it after. Treat as end-of-stream.
			final := state.finalize()
			applyTextCallFallback(final, allowedTools)
			if tail := dsml.Flush(); tail != "" {
				out <- StreamEvent{Kind: "text_delta", Text: tail}
			}
			scrubDSMLParts(final)
			out <- StreamEvent{Kind: "done", Final: final}
			return
		}

		var env struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"` // DeepSeek-R1/V4-Pro hidden CoT
					Reasoning        string `json:"reasoning"`         // Together/Groq alt key
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int     `json:"prompt_tokens"`
				CompletionTokens    int     `json:"completion_tokens"`
				Cost                float64 `json:"cost"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			out <- StreamEvent{Kind: "error", Err: fmt.Errorf("openai sse: parse: %w", err)}
			return
		}
		if env.Error != nil {
			out <- StreamEvent{Kind: "error",
				Err: fmt.Errorf("openai provider: %s: %s", env.Error.Type, env.Error.Message)}
			return
		}
		if env.Usage != nil {
			state.inTok = env.Usage.PromptTokens
			state.outTok = env.Usage.CompletionTokens
			state.cachedTok = env.Usage.PromptTokensDetails.CachedTokens
			state.costUSD = env.Usage.Cost
		}
		for _, ch := range env.Choices {
			// Reasoning content (DeepSeek-R1/V4-Pro emit it under
			// reasoning_content; Together/Groq use 'reasoning').
			// Accumulate silently — it's diagnostic, not user-facing
			// (no text_delta emit). Surfaced via the final
			// ReasoningContent part for the debug log to render.
			if ch.Delta.ReasoningContent != "" {
				state.reasoning.WriteString(ch.Delta.ReasoningContent)
			}
			if ch.Delta.Reasoning != "" {
				state.reasoning.WriteString(ch.Delta.Reasoning)
			}
			if ch.Delta.Content != "" {
				// Accumulate raw; the final TextContent is scrubbed via
				// scrubDSMLParts before the `done` event, so history is
				// clean even if a marker straddled a chunk boundary.
				state.text.WriteString(ch.Delta.Content)
				if safe := dsml.Feed(ch.Delta.Content); safe != "" {
					out <- StreamEvent{Kind: "text_delta", Text: safe}
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				tb := state.ensureTool(tc.Index)
				if tc.ID != "" {
					tb.id = tc.ID
				}
				if tc.Function.Name != "" {
					tb.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					tb.args.WriteString(tc.Function.Arguments)
				}
			}
			if ch.FinishReason != "" {
				state.stopReason = ch.FinishReason
				// On finish_reason, materialize any completed
				// tool_use events for the runner to dispatch eagerly.
				for _, tb := range state.tools {
					if tb == nil || tb.id == "" {
						continue
					}
					args := tb.args.String()
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					tc := message.ToolCall{
						ID:       tb.id,
						Name:     tb.name,
						Input:    args,
						Type:     "tool_use",
						Finished: true,
					}
					out <- StreamEvent{Kind: "tool_use", ToolCall: &tc}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if watchdogFired() {
			detail := fmt.Sprintf("stream idle for %s — no SSE events from upstream; aborting",
				streamIdleTimeout.Round(time.Second))
			if onState != nil {
				onState(RequestState{Phase: PhaseError, Detail: detail, When: time.Now()})
			}
			// Idle upstream is transient — next attempt usually
			// gets a healthy stream. Wrap so sendWithRecovery
			// retries instead of surfacing "context deadline
			// exceeded" to the agent loop, which kills the run.
			out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("openai provider: %s", detail)}}
			return
		}
		if onState != nil {
			onState(RequestState{Phase: PhaseError, Detail: "stream read: " + err.Error(), Err: err, When: time.Now()})
		}
		// Stream-read failures (TLS reset, gateway hiccup,
		// underlying-conn EOF) are essentially always transient.
		// The runner's ctx.Err() check above takes precedence for
		// real cancellation; everything else gets retried.
		out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("openai provider: stream read: %w", err)}}
		return
	}
	if watchdogFired() {
		detail := fmt.Sprintf("stream idle for %s — no SSE events from upstream; aborting",
			streamIdleTimeout.Round(time.Second))
		if onState != nil {
			onState(RequestState{Phase: PhaseError, Detail: detail, When: time.Now()})
		}
		out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("openai provider: %s", detail)}}
		return
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseDone, When: time.Now()})
	}
	final := state.finalize()
	applyTextCallFallback(final, allowedTools)
	if tail := dsml.Flush(); tail != "" {
		out <- StreamEvent{Kind: "text_delta", Text: tail}
	}
	scrubDSMLParts(final)
	out <- StreamEvent{Kind: "done", Final: final}
}
