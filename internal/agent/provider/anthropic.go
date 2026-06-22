package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic talks directly to Anthropic's Messages API. Same wire
// shape as Kailab (which proxies to Anthropic verbatim), so all
// the request/response translation helpers are reused. The only
// real differences are URL, auth header, the anthropic-version
// header, and the cost-pricing table.
//
// Use case: developers who already have an ANTHROPIC_API_KEY and
// don't want to wait on or sign up for kailab. Also the path that
// makes "self-host kai" trivial — the kai binary alone suffices.
type Anthropic struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	// AnthropicVersion is the API version header. Defaults to a
	// known-good string; pin it as a field so tests / advanced
	// users can override without us needing to ship a new binary
	// every time Anthropic publishes a date.
	AnthropicVersion string

	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxAttempts    int
}

// defaultAnthropicVersion pins the API contract this code was
// written against. Bump when we start using fields newer than the
// 2023-06-01 contract guarantees (extended thinking blocks,
// computer-use tools, etc).
const defaultAnthropicVersion = "2023-06-01"

// NewAnthropic builds an Anthropic-direct provider. Both args are
// required; missing-key validation happens at config.New time, not
// here, so this constructor is trivially testable.
func NewAnthropic(baseURL, apiKey string) *Anthropic {
	return &Anthropic{
		BaseURL:          strings.TrimSuffix(baseURL, "/"),
		APIKey:           apiKey,
		HTTPClient:       sharedHTTPClient(),
		AnthropicVersion: defaultAnthropicVersion,
	}
}

// SupportsCache: Anthropic Messages API natively supports prompt
// caching with cache_control breakpoints, which we set in the
// shared serializers.
func (a *Anthropic) SupportsCache() bool { return true }

func (a *Anthropic) version() string {
	if a.AnthropicVersion != "" {
		return a.AnthropicVersion
	}
	return defaultAnthropicVersion
}

// Send is a near-clone of Kailab.Send. Differences are confined to
// the URL and auth headers; retry, backoff, and request shape are
// shared. We intentionally don't extract a "round trip" helper
// shared between the two — duplicated retry loops are ~30 lines and
// the per-provider differences (network errors here vs through-
// proxy errors there) are easier to read inline than abstracted.
func (a *Anthropic) Send(ctx context.Context, req Request) (Response, error) {
	if a.BaseURL == "" {
		return Response{}, fmt.Errorf("anthropic provider: BaseURL not set")
	}
	if a.APIKey == "" {
		return Response{}, fmt.Errorf("anthropic provider: ANTHROPIC_API_KEY not set")
	}
	if req.Model == "" {
		return Response{}, fmt.Errorf("anthropic provider: Model required")
	}

	body, err := json.Marshal(buildAnthropicRequest(req))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic provider: marshaling request: %w", err)
	}

	maxAttempts := a.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	backoff := a.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := a.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := a.sendOnce(ctx, body, req.Model, req.OnState)
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

func (a *Anthropic) sendOnce(ctx context.Context, body []byte, model string, onState func(RequestState)) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		a.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic provider: building http request: %w", err)
	}
	a.applyHeaders(httpReq)

	client := a.HTTPClient
	if client == nil {
		client = sharedHTTPClient()
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseSent, Detail: "POST /v1/messages", When: time.Now()})
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("anthropic provider: sending request: %w", err)}
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseConnected, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), When: time.Now()})
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("anthropic provider: reading response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Errorf("anthropic provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		if isRetryableStatus(resp.StatusCode) {
			return Response{}, &transientError{cause: errMsg}
		}
		return Response{}, errMsg
	}

	var raw anthropicResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Response{}, fmt.Errorf("anthropic provider: parsing response: %w", err)
	}
	out := parseAnthropicResponse(raw)
	out.EstimatedCostUSD = anthropicCost(model, out)
	return out, nil
}

// SendStream is the streaming counterpart. Mirrors Kailab.SendStream
// — same SSE frame shapes, same handler — different URL and headers.
func (a *Anthropic) SendStream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	if a.BaseURL == "" {
		return nil, fmt.Errorf("anthropic provider: BaseURL not set")
	}
	if a.APIKey == "" {
		return nil, fmt.Errorf("anthropic provider: ANTHROPIC_API_KEY not set")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("anthropic provider: Model required")
	}

	body := buildAnthropicRequest(req)
	body["stream"] = true
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic provider: marshaling stream request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		a.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic provider: building stream request: %w", err)
	}
	a.applyHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := a.HTTPClient
	if client == nil || client.Timeout > 0 {
		client = &http.Client{} // zero timeout for SSE
	}
	emitState(req, PhaseSent, "POST /v1/messages (stream)", nil)
	resp, err := client.Do(httpReq)
	if err != nil {
		emitState(req, PhaseError, "opening stream: "+err.Error(), err)
		return nil, fmt.Errorf("anthropic provider: opening stream: %w", err)
	}
	emitState(req, PhaseConnected, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		err := fmt.Errorf("anthropic provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		emitState(req, PhaseError, err.Error(), err)
		return nil, err
	}

	out := make(chan StreamEvent, 32)
	// Wrap runSSE so we can post-process the final Response with a
	// cost estimate. runSSE closes the inner channel; we close the
	// outer one once we've forwarded everything.
	inner := make(chan StreamEvent, 32)
	go runSSE(resp, inner, req.OnState)
	go func() {
		defer close(out)
		for ev := range inner {
			if ev.Kind == "done" && ev.Final != nil {
				ev.Final.EstimatedCostUSD = anthropicCost(req.Model, *ev.Final)
			}
			out <- ev
		}
	}()
	return out, nil
}

func (a *Anthropic) applyHeaders(httpReq *http.Request) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", a.version())
}

// anthropicCost computes a USD estimate from token counts using a
// per-model pricing table. Update when Anthropic publishes new
// pricing. Source: https://www.anthropic.com/pricing (verified
// 2026-05). Returns 0 for unknown models — the trailer falls back
// to its own estimator in that case rather than display "$0.00"
// which would be misleading.
//
// Pricing is per-1M-tokens, broken into:
//   - input  (uncached, regular request input)
//   - cache_write (1.25× input — Anthropic's stated multiplier)
//   - cache_read  (0.10× input — Anthropic's stated multiplier)
//   - output
func anthropicCost(model string, r Response) float64 {
	p, ok := anthropicPricing[normalizeAnthropicModel(model)]
	if !ok {
		return 0
	}
	per := func(tokens int, rate float64) float64 {
		return float64(tokens) / 1_000_000 * rate
	}
	return per(r.InputTokens, p.input) +
		per(r.CacheCreationTokens, p.cacheWrite) +
		per(r.CacheReadTokens, p.cacheRead) +
		per(r.OutputTokens, p.output)
}

// normalizeAnthropicModel collapses dated variants ("claude-sonnet-4-6-20251015")
// to the family key used in the pricing table ("claude-sonnet-4-6"). New
// snapshot dates inherit the family's pricing automatically — Anthropic
// has historically kept the same price across snapshots of one family.
func normalizeAnthropicModel(model string) string {
	// Strip a trailing "-YYYYMMDD" if present.
	if i := strings.LastIndex(model, "-"); i > 0 && len(model)-i == 9 {
		tail := model[i+1:]
		if len(tail) == 8 && allDigits(tail) {
			return model[:i]
		}
	}
	return model
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type anthropicPriceRow struct {
	input      float64 // $ per 1M input tokens
	cacheWrite float64 // $ per 1M cache_creation tokens
	cacheRead  float64 // $ per 1M cache_read tokens
	output     float64 // $ per 1M output tokens
}

// anthropicPricing — keep in sync with anthropic.com/pricing.
// Verified: 2026-05.
var anthropicPricing = map[string]anthropicPriceRow{
	"claude-opus-4-7":     {input: 15.0, cacheWrite: 18.75, cacheRead: 1.50, output: 75.0},
	"claude-opus-4-6":     {input: 15.0, cacheWrite: 18.75, cacheRead: 1.50, output: 75.0},
	"claude-sonnet-4-6":   {input: 3.0, cacheWrite: 3.75, cacheRead: 0.30, output: 15.0},
	"claude-haiku-4-5":    {input: 0.25, cacheWrite: 0.30, cacheRead: 0.03, output: 1.25},
}
