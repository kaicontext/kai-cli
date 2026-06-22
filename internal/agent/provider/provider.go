// Package provider abstracts LLM providers behind a single Send call
// that the agent runner uses on every turn. The runner doesn't care
// whether requests go to api.anthropic.com directly, through kailab's
// proxy, or through some other vendor — only that Send takes a
// uniform Request and returns a uniform Response.
//
// Slice 1 ships one implementation: KailabProvider in `kailab.go`,
// which routes through the kai-server's `POST /api/v1/llm/messages`
// endpoint using the user's stored bearer token. Direct-Anthropic
// (with a per-developer API key) is a deferred slice; on-prem
// deployments will need it eventually but no v1 user does.
package provider
import (
	"context"
	"time"

	"kai/internal/agent/message"
	"kai/internal/agent/tools"
)

// RequestPhase is a discrete state in the lifecycle of a single
// provider call. Emitted via Request.OnState so callers see the
// actual HTTP/SSE state of the call instead of inferring it from
// derived events. The intent is observability, not control flow:
// the runner / TUI does not need to react to a phase, but rendering
// it gives the user a "is this call alive?" answer that doesn't
// rely on heuristics like "no deltas in N seconds."
type RequestPhase string

const (
	// PhaseSent: the HTTP request has been dispatched (about to
	// block on Do/Read of headers). No upstream-side response yet.
	PhaseSent RequestPhase = "sent"
	// PhaseConnected: response headers received with non-error
	// status. Body may still be empty.
	PhaseConnected RequestPhase = "connected"
	// PhaseStreaming: first SSE byte read (or a non-stream
	// response body fully read). Model has begun replying.
	PhaseStreaming RequestPhase = "streaming"
	// PhaseStreamIdle: the SSE body is open but no scanner line
	// has arrived for a soft idle threshold (default 10s). Emitted
	// once per idle gap; PhaseStreaming re-emits when bytes resume.
	PhaseStreamIdle RequestPhase = "stream_idle"
	// PhaseDone: terminal — call completed cleanly.
	PhaseDone RequestPhase = "done"
	// PhaseError: terminal — call failed. Err is the underlying
	// error; Detail is the human-readable summary the TUI should
	// surface (status line, not stack trace).
	PhaseError RequestPhase = "error"

	// PhaseUpstreamSent / Connected / Error: emitted by the kailab
	// proxy via `event: kai_state` SSE frames so the client can see
	// the kailab → api.anthropic.com (or api.openai.com) leg's
	// state. Without these the client only sees its own
	// connection to kailab; if the upstream provider stalls,
	// kailab looks healthy from the client's vantage.
	PhaseUpstreamSent      RequestPhase = "upstream_sent"
	PhaseUpstreamConnected RequestPhase = "upstream_connected"
	PhaseUpstreamError     RequestPhase = "upstream_error"
)

// RequestState is one lifecycle event for a single Send/SendStream
// call. Fired synchronously from inside the provider; the callback
// MUST NOT block (the TUI handler is expected to do a non-blocking
// channel send and drop on backpressure).
type RequestState struct {
	Phase  RequestPhase
	Detail string // status code, error summary, or empty for plain transitions
	// IdleSince is set on PhaseStreamIdle — the duration since the
	// most recent SSE line. Lets the TUI render "stream idle 12s"
	// without the watchdog needing to emit a periodic tick.
	IdleSince time.Duration
	// Err is set on PhaseError. Detail summarizes; Err is the raw
	// error for callers that want to type-check (e.g. CapExceeded).
	Err  error
	When time.Time
}

// Request is one turn worth of input to the model.
type Request struct {
	// Model is the Anthropic model id (e.g. "claude-sonnet-4-6").
	// Required: empty string is a misconfiguration, not a default.
	Model string

	// System is the top-level system prompt. May be empty for
	// follow-up turns where the system prompt is already implicit.
	System string

	// Messages is the conversation history including the latest
	// user turn. Tool results from previous turns appear as parts
	// of user-role messages here.
	Messages []message.Message

	// Tools is the catalog of tools the model may call this turn.
	// Each tool's schema is sent as JSON-Schema; the model produces
	// matching ToolCalls in the response.
	Tools []tools.ToolInfo

	// MaxTokens caps a single response. The runner sums these to
	// enforce per-run budgets.
	MaxTokens int

	// OnState, if set, receives RequestState transitions for this
	// request. The provider MUST fire it from inside the Send /
	// SendStream goroutine so callers see real HTTP/SSE state, not
	// downstream events. The callback is best-effort: providers
	// invoke it synchronously, so callers should hand off to a
	// channel with non-blocking send and drop on backpressure.
	OnState func(RequestState)

	// EphemeralTailMessages, when > 0, tells the provider that the
	// last N messages in the slice are per-turn ephemera (graph
	// context hint, convergence nudge) and must NOT be included in
	// the cache_control breakpoint. With caching on, the breakpoint
	// is placed at the end of the message preceding these tails so
	// the canonical history prefix stays byte-stable across turns
	// while the ephemeral hint is sent fresh each turn.
	//
	// Without this hint, the runner's per-turn injection
	// invalidated the cache prefix every turn — confirmed May 2026
	// via run-log per-message hash diff (msg[26] differed between
	// turns 0/1, all other prefix bytes matched).
	EphemeralTailMessages int

	// OutputJSONSchema, when non-nil, constrains the model's FINAL text
	// response to match this JSON Schema. Tool calls during exploration
	// turns are unaffected — Anthropic's `output_config` only narrows
	// the terminal text block. The planner uses this to make its
	// WorkPlan JSON guaranteed-parseable instead of fishing it out of a
	// markdown fence (the May 2026 regression that motivated this).
	//
	// Restrictions imposed by Anthropic's grammar: every object must
	// declare `additionalProperties: false`, every property must be
	// in `required` (no optional fields), no recursion, no numerical /
	// string constraints. Build the schema with that in mind.
	//
	// Providers that don't support structured outputs (the OpenAI-
	// shaped route, Together-routed Qwen, etc.) silently ignore this
	// field today. The fenced-JSON extractor on the caller side is
	// still the parser of record — the schema just makes its job
	// trivial when the upstream cooperates.
	OutputJSONSchema map[string]interface{}

	// RequireToolUse, when true, instructs the provider to set
	// Anthropic's `tool_choice: {"type": "any"}` (or the equivalent
	// in other API shapes) so the model is structurally required to
	// emit a tool call before any plain-text response. Used by the
	// planner's forced-exploration reprompt: a soft "please make a
	// tool call" instruction is something opus-4-6 ignores; this
	// flag turns the requirement into an API-level constraint.
	//
	// Providers that don't honor it (OpenAI-shaped routes that
	// don't expose tool_choice in this way) MUST silently ignore
	// the field — callers fall back to soft enforcement.
	RequireToolUse bool
}

// Response is the model's reply for one turn.
type Response struct {
	// Parts is the structured content the model produced. For a
	// non-tool turn this is typically a single TextContent. For a
	// tool turn it includes ToolCall parts the runner dispatches.
	Parts []message.ContentPart

	// FinishReason matches the model's stop reason. When this is
	// FinishReasonToolUse the runner runs tools and loops; when it's
	// FinishReasonEndTurn the runner exits cleanly.
	FinishReason message.FinishReason

	// InputTokens / OutputTokens are billed-as accounting. Plumbed
	// for the orchestrator's MaxAgentTokens cap. InputTokens
	// reflects new (uncached) input tokens — the billing-aligned
	// number. With prompt caching enabled, the prompt's true size
	// is InputTokens + CachedInputTokens; use that for context-
	// window math (compaction triggers etc.).
	InputTokens  int
	OutputTokens int

	// CachedInputTokens is the sum of cache_read + cache_creation
	// usage when the provider supports prompt caching. Zero when
	// the request didn't hit (or write) the cache, or when the
	// provider doesn't report cache stats. Callers that just want
	// "how many tokens of context did this turn carry" should sum
	// InputTokens + CachedInputTokens.
	//
	// CacheCreationTokens / CacheReadTokens split the same total
	// for cost accounting: they bill at very different rates
	// (creation ~1.25× normal input, read ~0.10× normal input on
	// Sonnet 4.6), so a trailer that lumped them together would
	// hide the actual driver of cost. CachedInputTokens =
	// CacheCreationTokens + CacheReadTokens is preserved for
	// callers that don't care about the split.
	CachedInputTokens   int
	CacheCreationTokens int
	CacheReadTokens     int

	// ProviderNote is an optional human-readable hint the trailer
	// surfaces alongside token counts. Used by providers that lack
	// some accounting feature so the UI can be honest about it
	// rather than silently displaying zeros that look like cache
	// misses. Examples:
	//   "(no cache support)"  — OpenAI-compat: cache_read_input_tokens
	//                           is always zero because the protocol
	//                           doesn't carry the field
	//   "local"               — Ollama / local model: cost is $0
	// Empty string = no annotation (kailab and anthropic-direct).
	ProviderNote string

	// EstimatedCostUSD is the provider's best estimate of what this
	// turn cost in dollars, computed from token counts and a
	// per-model pricing table. Zero when the provider can't price
	// (local models) or doesn't yet ship a table for the model id.
	// The trailer prefers this when non-zero; falls back to its own
	// internal estimator otherwise.
	EstimatedCostUSD float64
}

// Provider sends a Request to an LLM and returns its Response.
// Implementations must be safe to call concurrently from multiple
// goroutines (the orchestrator may run agents in parallel).
type Provider interface {
	Send(ctx context.Context, req Request) (Response, error)
}

// StreamEvent is one chunk of the model's response delivered via
// SSE. The runner consumes events from a channel until either an
// EventDone or EventError arrives. Text deltas accumulate into the
// final Response.Parts; the final aggregated Response is also sent
// via EventDone so callers don't need to track partial state.
type StreamEvent struct {
	// Kind is one of:
	//   - "text_delta"  Text holds an incremental piece of assistant prose
	//   - "tool_use"    ToolCall is the fully-formed tool dispatch the model emitted
	//   - "done"        Final non-incremental Response with FinishReason + token counts
	//   - "error"       Err holds the failure
	Kind string

	// Text is set for "text_delta" — the new characters since the
	// last delta. Concatenate to build the full assistant message.
	Text string

	// ToolCall is set for "tool_use" — emitted when the model
	// finishes serializing a tool's input args. Runners that want
	// "speculative tool dispatch" can act on it as soon as it
	// arrives; conservative runners can wait for the "done" event.
	ToolCall *message.ToolCall

	// Final is set for "done" — the same Response shape Send
	// returns, ready to drop into history.
	Final *Response

	// Err is set for "error".
	Err error
}

// DailyUsageReporter is the optional interface providers
// implement when they have visibility into a server-side daily
// cost cap. Currently kailab is the only implementer; BYOM
// providers (anthropic-direct, openai) have no shared cap to
// report.
//
// Returns cents (not dollars) so the wire format and the CLI
// display share one integer arithmetic. ok=false means "no
// snapshot yet" — the caller should NOT render a "0 / 0" line.
type DailyUsageReporter interface {
	DailyUsage() (cost, cap int, ok bool)
}

// DailyUsage extracts the snapshot if the provider implements
// DailyUsageReporter; returns ok=false otherwise. Use this from
// the TUI rather than a type assertion so the call site doesn't
// need to know which providers have caps.
func DailyUsage(p Provider) (cost, cap int, ok bool) {
	if du, ok2 := p.(DailyUsageReporter); ok2 {
		return du.DailyUsage()
	}
	return 0, 0, false
}

// CacheSupporter is the optional interface providers implement when
// the wire protocol carries a server-side prompt cache. Used by:
//
//   - Planner: to scale per-turn token budgets up (cache available)
//     or down (every turn re-bills the full prompt).
//   - Trailer: to annotate "(no cache support)" so the user isn't
//     confused by zero cache_read tokens.
//   - `kai auth status`: to display "Cache: supported / unsupported".
//
// Defaults: kailab and anthropic implement this returning true;
// openai implements it returning false. New providers that don't
// implement the interface are conservatively treated as
// cache-unsupported.
type CacheSupporter interface {
	SupportsCache() bool
}

// SupportsCache is the type-assertion helper callers should use
// instead of asserting in-line. Returns false for providers that
// don't implement CacheSupporter — safer default than assuming
// cache support and over-budgeting on a wire protocol that doesn't
// have it.
func SupportsCache(p Provider) bool {
	if cs, ok := p.(CacheSupporter); ok {
		return cs.SupportsCache()
	}
	return false
}

// Streamer is the optional interface providers may implement to
// deliver responses incrementally. The runner falls back to Send
// when the provider doesn't implement Streamer.
//
// SendStream returns a channel that emits StreamEvents until either
// "done" or "error" is delivered, after which the channel is
// closed. Implementations must close the channel exactly once and
// must honor ctx cancellation by closing the channel and stopping
// the upstream HTTP read.
type Streamer interface {
	SendStream(ctx context.Context, req Request) (<-chan StreamEvent, error)
}

// emitState invokes req.OnState if set. Centralized so providers
// don't repeat the nil-check + timestamp dance at every call site.
// Safe to call with a zero-value Request (no callback → no-op).
func emitState(req Request, p RequestPhase, detail string, err error) {
	if req.OnState == nil {
		return
	}
	req.OnState(RequestState{
		Phase:  p,
		Detail: detail,
		Err:    err,
		When:   time.Now(),
	})
}

// emitStreamIdle is the watchdog-specific helper: includes IdleSince
// so the TUI can render "stream idle for 12s" without separately
// timing the gap.
func emitStreamIdle(onState func(RequestState), idleSince time.Duration) {
	if onState == nil {
		return
	}
	onState(RequestState{
		Phase:     PhaseStreamIdle,
		IdleSince: idleSince,
		When:      time.Now(),
	})
}
