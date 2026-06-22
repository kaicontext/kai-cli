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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kai/internal/agent/message"
	"kai/internal/agent/tools"
)

// Kailab routes Anthropic Messages API calls through the kai-server
// proxy at `POST /api/v1/llm/messages`. Server-side ANTHROPIC_API_KEY
// is held by kailab; the user only needs a kailab bearer token.
//
// Unlike `internal/planner.ServerCompleter` (which is single-shot
// JSON-output for plans), this client supports tool_use blocks and
// multi-turn conversations. The two coexist on the same proxy
// endpoint — the proxy forwards the request body to Anthropic
// verbatim, so any Anthropic-compatible request shape works.
type Kailab struct {
	BaseURL    string
	AuthToken  string
	HTTPClient *http.Client

	// ProviderHint, when non-empty, is forwarded to the kailab
	// proxy as the "provider" field in the request body. This lets
	// the server route the request to a specific upstream backend
	// (e.g. "openai") instead of its default (Anthropic). Set via
	// the KAI_KAILAB_UPSTREAM env var. An empty value leaves
	// routing up to the server (default: Anthropic).
	ProviderHint string

	// InitialBackoff is the first sleep between retry attempts.
	// Doubles each attempt up to MaxBackoff. Zero falls back to a
	// 1-second default so production behavior is unchanged unless a
	// caller (typically a test) explicitly shrinks it.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxAttempts    int

	// Mutex-protected snapshot of the most recent
	// x-kai-daily-cost / x-kai-daily-cap headers we observed on a
	// kailab response. Surfaced to the TUI's `kai auth status` and
	// to the trailer-warning band via DailyUsage(). The snapshot
	// is best-effort: zero values mean "no kailab response yet
	// this session." Updated on every Send / SendStream that
	// returns headers (success OR cap-exceeded 429), so the TUI's
	// running picture stays current without a separate poll.
	usageMu     sync.Mutex
	dailyCost   int
	dailyCap    int
	dailyHasObs bool
}

// NewKailab builds a Kailab provider. baseURL is kai-server's URL
// (e.g. https://kaicontext.com); authToken is the user's bearer.
// Both are required; nil checks happen on Send to keep construction
// trivially testable.
func NewKailab(baseURL, authToken string) *Kailab {
	return &Kailab{
		BaseURL:   strings.TrimSuffix(baseURL, "/"),
		AuthToken: authToken,
		HTTPClient: &http.Client{
			// Match the kai-server-side timeout to api.anthropic.com
			// (120s). One end timing out before the other masks the
			// real failure source.
			Timeout: 120 * time.Second,
		},
	}
}

// snapshotDailyUsage records the most recently observed
// x-kai-daily-cost / x-kai-daily-cap header pair. Called from
// every Send / SendStream response path. Missing headers are
// ignored (no overwrite) — older runs of kailab without the
// rate-limit middleware shouldn't blank out a previously-good
// snapshot.
func (k *Kailab) snapshotDailyUsage(h http.Header) {
	costS := h.Get("x-kai-daily-cost")
	capS := h.Get("x-kai-daily-cap")
	if costS == "" || capS == "" {
		return
	}
	cost, err1 := strconv.Atoi(costS)
	cap, err2 := strconv.Atoi(capS)
	if err1 != nil || err2 != nil {
		return
	}
	k.usageMu.Lock()
	k.dailyCost = cost
	k.dailyCap = cap
	k.dailyHasObs = true
	k.usageMu.Unlock()
}

// DailyUsage returns the most recently observed daily-cost cap
// snapshot in cents. ok=false when no kailab response has been
// observed in this process — the caller should NOT render a "0
// of 0" trailer in that case (it would lie about the cap).
//
// Reading is cheap; safe to call from any goroutine including
// the TUI render loop.
func (k *Kailab) DailyUsage() (cost, cap int, ok bool) {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	return k.dailyCost, k.dailyCap, k.dailyHasObs
}

// SupportsCache: kailab proxies to Anthropic, which has prompt
// caching. The breakpoints we set in buildAnthropicRequest /
// serializeMessages flow through verbatim, so cache hits land
// the same as direct-Anthropic.
func (k *Kailab) SupportsCache() bool { return true }

// Send translates the internal Request to Anthropic's Messages API
// shape, posts it, and translates the response back. Error messages
// from upstream are forwarded verbatim so the user sees the real
// upstream reason (rate limit, invalid model, no credit, etc.).
//
// Transient upstream errors (429 rate-limit, 529 overloaded, 502/503/
// 504 gateway hiccups, network errors) are retried with exponential
// backoff before surfacing. Non-transient errors (400 invalid request,
// 401 unauthorized, 404, 413 too-large) bubble up immediately — no
// amount of retrying fixes them and the user wants the real reason.
func (k *Kailab) Send(ctx context.Context, req Request) (Response, error) {
	if k.BaseURL == "" {
		return Response{}, fmt.Errorf("kailab provider: BaseURL not set")
	}
	if k.AuthToken == "" {
		return Response{}, fmt.Errorf("kailab provider: not logged in (run `kai auth login`)")
	}
	if req.Model == "" {
		return Response{}, fmt.Errorf("kailab provider: Model required")
	}
	// OpenAI-family models (gpt-*, o3-*, o4-*) route through kailab's
	// /api/v1/llm/completions endpoint with OpenAI's native wire shape.
	// Kailab handles auth+metering+forwarding; no translation, both
	// ends speak OpenAI. See kailab_openai.go for the routed path.
	if IsOpenAIModel(req.Model) {
		return k.sendOpenAI(ctx, req)
	}

	bodyMap := buildAnthropicRequest(req)
	if k.ProviderHint != "" {
		bodyMap["provider"] = k.ProviderHint
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return Response{}, fmt.Errorf("kailab provider: marshaling request: %w", err)
	}

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
		resp, err := k.sendOnce(ctx, body, req.OnState)
		if err == nil {
			emitState(req, PhaseDone, "", nil)
			return resp, nil
		}
		lastErr = err
		// Honor cancellation immediately — don't retry through a
		// user-initiated Ctrl+C.
		if cerr := ctx.Err(); cerr != nil {
			return Response{}, cerr
		}
		if !isTransient(err) || attempt == maxAttempts {
			emitState(req, PhaseError, err.Error(), err)
			return Response{}, err
		}
		// Sleep with cancellation awareness. tea.Tick semantics: the
		// timer fires once; ctx.Done() preempts it.
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

// sendOnce performs a single HTTP round-trip. Returns a typed
// transientError for retryable upstream conditions; everything else
// is returned as a regular error.
func (k *Kailab) sendOnce(ctx context.Context, body []byte, onState func(RequestState)) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		k.BaseURL+"/api/v1/llm/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("kailab provider: building http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+k.AuthToken)

	client := k.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	if onState != nil {
		onState(RequestState{Phase: PhaseSent, Detail: "POST /api/v1/llm/messages", When: time.Now()})
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// Network errors (DNS, connection reset, EOF mid-stream,
		// timeout) are transient — the next attempt may succeed.
		return Response{}, &transientError{cause: fmt.Errorf("kailab provider: sending request: %w", err)}
	}
	if onState != nil {
		onState(RequestState{
			Phase:  PhaseConnected,
			Detail: fmt.Sprintf("HTTP %d", resp.StatusCode),
			When:   time.Now(),
		})
	}
	defer resp.Body.Close()

	// Capture daily-usage headers from EVERY response — success
	// and cap-exceeded 429 alike — so `kai auth status` and the
	// trailer warning band have a current snapshot regardless of
	// the request's outcome.
	k.snapshotDailyUsage(resp.Header)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &transientError{cause: fmt.Errorf("kailab provider: reading response: %w", err)}
	}
	// Daily-cap carve-out: when kailab rejects a request because
	// the user is over their per-user cost ceiling, the response
	// carries `kai-cap-exceeded: true`. We do NOT retry — the cap
	// won't move until midnight UTC — and we surface a typed
	// CapExceededError so the TUI can render the formatted
	// message (cap amount, reset time, BYOM hint) directly from
	// the parsed body instead of a generic 429 stack.
	if resp.StatusCode == http.StatusTooManyRequests &&
		resp.Header.Get("kai-cap-exceeded") == "true" {
		return Response{}, parseCapExceeded(respBody)
	}
	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Errorf("kailab provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		if isRetryableStatus(resp.StatusCode) {
			return Response{}, &transientError{cause: errMsg}
		}
		return Response{}, errMsg
	}

	var raw anthropicResponse
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return Response{}, fmt.Errorf("kailab provider: parsing response: %w", err)
	}
	return parseAnthropicResponse(raw), nil
}

// transientError marks an error as worth retrying. Wrapping (rather
// than a status-code int) keeps the public API status-code-agnostic
// for future provider implementations that signal retry-worthiness
// differently (e.g. direct-Anthropic via SDK).
type transientError struct{ cause error }

func (t *transientError) Error() string { return t.cause.Error() }
func (t *transientError) Unwrap() error { return t.cause }

func isTransient(err error) bool {
	var te *transientError
	return errAs(err, &te)
}

// IsTransient is the exported classifier the agent runner uses to
// decide between "retry" and "surface". A transient error is one
// the next attempt may fix on its own — rate limit, overload,
// gateway hiccup, network reset. The runner must NOT retry for any
// other failure mode (auth, billing, malformed request).
func IsTransient(err error) bool { return isTransient(err) }

// IsContextOverflow reports whether the provider rejected the
// request because the prompt exceeded the model's context window.
// Drives the runner's "compact and retry" path: on true, the
// runner forces compaction and re-sends instead of giving up.
//
// We match against status code 413 and the Anthropic-specific
// "prompt is too long" / "context_length" / "tokens > maximum"
// signal that comes back as a 400 with a structured error body.
// Both shapes are tested in kailab_test.go.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, " 413:") || strings.HasSuffix(msg, " 413") {
		return true
	}
	if strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "context_length") ||
		strings.Contains(msg, "tokens > maximum") ||
		strings.Contains(msg, "context window") {
		return true
	}
	return false
}

// errAs is a thin wrapper around errors.As to keep the import
// surface obvious and let us tweak matching later without hunting
// through call sites.
func errAs(err error, target interface{}) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if t, ok := target.(**transientError); ok {
			if v, ok2 := err.(*transientError); ok2 {
				*t = v
				return true
			}
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// isRetryableStatus picks the upstream codes that warrant a retry:
//
//   - 408 request timeout (intermediary)
//   - 425 too early (rare, but transient by definition)
//   - 429 rate-limited (Anthropic throttling)
//   - 500 server error (genuine intermittent failure)
//   - 502/503/504 gateway hiccups
//   - 529 overloaded (Anthropic-specific "we're saturated")
//
// Notably *not* retryable: 400 (bad request — won't fix itself),
// 401 (auth — needs login), 403, 404, 413 (too-large — needs
// compaction, not a retry).
func isRetryableStatus(code int) bool {
	switch code {
	case 408, 425, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// SendStream opens an SSE-enabled request to the kailab proxy and
// returns a channel of StreamEvents the runner consumes
// incrementally. The transport is the same `/api/v1/llm/messages`
// endpoint as Send; we just add `stream: true` to the body and
// Accept: text/event-stream so kailab forwards Anthropic's SSE
// frames verbatim.
//
// Retries: a transient error before the first byte returns from
// SendStream synchronously (caller can retry). Errors mid-stream
// surface as a single "error" event followed by channel close —
// the runner falls back to either a synthetic completion or a
// fresh non-streaming Send on the next turn.
func (k *Kailab) SendStream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	if k.BaseURL == "" {
		return nil, fmt.Errorf("kailab provider: BaseURL not set")
	}
	if k.AuthToken == "" {
		return nil, fmt.Errorf("kailab provider: not logged in (run `kai auth login`)")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("kailab provider: Model required")
	}
	// Same model-based routing as Send(). See kailab_openai.go.
	if IsOpenAIModel(req.Model) {
		return k.sendStreamOpenAI(ctx, req)
	}

	body := buildAnthropicRequest(req)
	body["stream"] = true
	if k.ProviderHint != "" {
		body["provider"] = k.ProviderHint
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("kailab provider: marshaling stream request: %w", err)
	}
	// KAI_RAW_CAPTURE=<dir> dumps the outgoing request body + the
	// incoming SSE bytes to files under <dir>. Used to investigate
	// suspect responses (e.g. the 2026-05-12 "spaced gibberish" plan):
	// without raw bytes we can only guess whether garbage came from
	// upstream or from our parsing.
	captureDir := strings.TrimSpace(os.Getenv("KAI_RAW_CAPTURE"))
	if captureDir != "" {
		if err := os.MkdirAll(captureDir, 0o755); err == nil {
			ts := time.Now().UnixNano()
			_ = os.WriteFile(filepath.Join(captureDir, fmt.Sprintf("req-%d.json", ts)), raw, 0o644)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		k.BaseURL+"/api/v1/llm/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("kailab provider: building stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+k.AuthToken)

	// SSE responses are long-lived (one stream per turn). Use a
	// dedicated client with no timeout so a slow model can't trip
	// the standard request-level deadline. Per-event ctx is the
	// cancellation lever instead.
	client := k.HTTPClient
	if client == nil || client.Timeout > 0 {
		client = &http.Client{} // zero timeout, infinite read
	}
	emitState(req, PhaseSent, "POST /api/v1/llm/messages (stream)", nil)
	resp, err := client.Do(httpReq)
	if err != nil {
		emitState(req, PhaseError, "opening stream: "+err.Error(), err)
		return nil, fmt.Errorf("kailab provider: opening stream: %w", err)
	}
	emitState(req, PhaseConnected, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	// Stream path also snapshots the visibility headers — they
	// arrive on the response (before the SSE body starts) so we
	// have them whether the stream succeeds or fails below.
	k.snapshotDailyUsage(resp.Header)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		// Cap-exceeded mirrors the non-streaming path: typed error
		// so the runner / TUI can render the structured message
		// instead of a generic 429 stack.
		if resp.StatusCode == http.StatusTooManyRequests &&
			resp.Header.Get("kai-cap-exceeded") == "true" {
			ce := parseCapExceeded(respBody)
			emitState(req, PhaseError, "daily cost cap exceeded", ce)
			return nil, ce
		}
		err := fmt.Errorf("kailab provider: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
		emitState(req, PhaseError, err.Error(), err)
		return nil, err
	}

	out := make(chan StreamEvent, 32)
	// KAI_RAW_CAPTURE: tee the response body to a file before parsing
	// so we can inspect the bytes that arrived on the wire. The tee
	// runs INLINE on the same reader the SSE scanner consumes, so what
	// the parser sees is exactly what got captured.
	captureDir2 := strings.TrimSpace(os.Getenv("KAI_RAW_CAPTURE"))
	if captureDir2 != "" {
		if err := os.MkdirAll(captureDir2, 0o755); err == nil {
			ts := time.Now().UnixNano()
			capPath := filepath.Join(captureDir2, fmt.Sprintf("sse-%d.raw", ts))
			if f, err := os.Create(capPath); err == nil {
				resp.Body = &teeReadCloser{r: io.TeeReader(resp.Body, f), c: resp.Body, f: f}
			}
		}
	}
	go runSSE(resp, out, req.OnState)
	return out, nil
}

// teeReadCloser wraps a TeeReader so the captured file is closed
// when the response body is closed. Used by KAI_RAW_CAPTURE. Made
// idempotent + nil-safe so accidental double-close doesn't panic
// (which would silently kill the TUI process — same shape as the
// 16:08 dogfood incident where the agent vanished mid-turn).
type teeReadCloser struct {
	r      io.Reader
	c      io.Closer
	f      *os.File
	closed bool
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	if t == nil || t.r == nil {
		return 0, io.EOF
	}
	return t.r.Read(p)
}
func (t *teeReadCloser) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	if t.f != nil {
		_ = t.f.Close()
	}
	if t.c == nil {
		return nil
	}
	return t.c.Close()
}

// runSSE reads Anthropic-style SSE frames off the response body
// and translates them to StreamEvents. Closes out exactly once at
// the end (success, error, or context cancel).
func runSSE(resp *http.Response, out chan<- StreamEvent, onState func(RequestState)) {
	defer resp.Body.Close()
	defer close(out)

	bumpActivity, stopWatchdog, watchdogFired := sseWatchdog(resp, streamIdleTimeout, onState)
	defer stopWatchdog()
	// Track whether we've already emitted PhaseStreaming so we
	// don't double-emit on the very first Scan iteration.
	streamingEmitted := false

	scanner := bufio.NewScanner(resp.Body)
	// Anthropic delta payloads can be large when the model dumps
	// long text in one chunk; bump the scanner buffer well above
	// the default 64K so a 256K delta doesn't truncate the stream.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	state := &sseState{}
	var dataBuf strings.Builder
	// currentEvent tracks the last `event:` line seen for the
	// upcoming frame. Most Anthropic frames don't carry one and the
	// JSON `type` field is authoritative — but kailab injects
	// `event: kai_state` frames to surface upstream-call lifecycle
	// (kailab → api.anthropic.com). We route those to onState
	// instead of the Anthropic content parser.
	currentEvent := ""
	for scanner.Scan() {
		line := scanner.Text()
		rawDump("kailab-sse", line)
		// Bump activity ONLY for `data:` lines — the real content
		// carriers. SSE keepalive comments (`: ping` every ~15s),
		// bare `event:` headers, and frame-separator blank lines
		// would otherwise reset the soft idle clock, defeating it
		// entirely on a stream where the upstream is keeping TCP
		// alive but the model has stopped emitting real frames.
		// "streaming" means real bytes are flowing, not just "the
		// connection hasn't died." (The hard 60s watchdog still
		// uses the same bumpActivity, so it's also keepalive-immune
		// now — a TCP connection alone won't keep the stream from
		// being aborted.)
		if strings.HasPrefix(line, "data:") {
			bumpActivity()
			if !streamingEmitted && onState != nil {
				onState(RequestState{Phase: PhaseStreaming, When: time.Now()})
				streamingEmitted = true
			}
		}
		if line == "" {
			// Blank line terminates an event — flush.
			if dataBuf.Len() > 0 {
				if currentEvent == "kai_state" {
					handleKaiStateFrame(dataBuf.String(), onState)
				} else if ev, ok := state.handleData(dataBuf.String()); ok {
					out <- ev
					if ev.Kind == "done" || ev.Kind == "error" {
						return
					}
				}
				dataBuf.Reset()
			}
			currentEvent = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / keepalive
		}
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		if watchdogFired() {
			detail := fmt.Sprintf("stream idle for %s — no SSE events from upstream; aborting",
				streamIdleTimeout.Round(time.Second))
			if onState != nil {
				onState(RequestState{Phase: PhaseError, Detail: detail, When: time.Now()})
			}
			// Transient — next attempt usually gets a healthy
			// stream. See openai.go for the matching wrap.
			out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("kailab provider: %s", detail)}}
			return
		}
		if onState != nil {
			onState(RequestState{Phase: PhaseError, Detail: "stream read: " + err.Error(), Err: err, When: time.Now()})
		}
		out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("kailab provider: stream read: %w", err)}}
		return
	}
	if watchdogFired() {
		// Scanner returned cleanly (EOF) but only because the
		// watchdog closed the body. Treat as a stuck-stream
		// abort so the runner surfaces a real error instead of a
		// truncated success.
		detail := fmt.Sprintf("stream idle for %s — no SSE events from upstream; aborting",
			streamIdleTimeout.Round(time.Second))
		if onState != nil {
			onState(RequestState{Phase: PhaseError, Detail: detail, When: time.Now()})
		}
		out <- StreamEvent{Kind: "error", Err: &transientError{cause: fmt.Errorf("kailab provider: %s", detail)}}
		return
	}
	// Stream ended without a message_stop — synthesize a done event
	// from whatever state we accumulated so the runner doesn't hang.
	if onState != nil {
		onState(RequestState{Phase: PhaseDone, When: time.Now()})
	}
	out <- StreamEvent{Kind: "done", Final: state.finalize()}
}

// sseState accumulates Anthropic SSE deltas into a Response. Each
// content block (text or tool_use) is built up in `current` and
// pushed onto `parts` on content_block_stop.
type sseState struct {
	parts             []message.ContentPart
	currentText       *strings.Builder
	currentTool       *toolBuilder
	stopReason        string
	inputTokens       int
	outputTokens      int
	cachedInputTokens int
	cacheCreationTokens int
	cacheReadTokens     int
}

// toolBuilder accumulates a tool_use block's args incrementally —
// Anthropic streams the JSON one chunk at a time as
// input_json_delta events. We concatenate them and parse on stop.
type toolBuilder struct {
	id    string
	name  string
	input strings.Builder
}

// handleData processes one SSE data payload. Returns (event, true)
// when the payload should be forwarded to the runner; (zero,
// false) when it's a no-op (block start, intermediate frames).
func (s *sseState) handleData(data string) (StreamEvent, bool) {
	var env struct {
		Type  string          `json:"type"`
		Index int             `json:"index"`
		Delta json.RawMessage `json:"delta"`
		// content_block_start
		ContentBlock json.RawMessage `json:"content_block"`
		// message_start
		Message json.RawMessage `json:"message"`
		// message_delta carries usage at the TOP LEVEL of the
		// frame (NOT under delta). We were previously reading
		// usage from inside Delta and silently getting zero
		// every time — which made the TUI's trailer report
		// "0 out" even when the model produced thousands of
		// output tokens. The display lie made it look like
		// streaming runs were free; they were not.
		Usage *struct {
			OutputTokens             int `json:"output_tokens"`
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		// errors
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return StreamEvent{Kind: "error", Err: fmt.Errorf("sse: parsing frame: %w", err)}, true
	}

	switch env.Type {
	case "ping", "message_start":
		// message_start carries usage.input_tokens (and the cache
		// stats when prompt caching is in play) up front.
		if len(env.Message) > 0 {
			var msg struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(env.Message, &msg); err == nil {
				s.inputTokens = msg.Usage.InputTokens
				s.cacheCreationTokens = msg.Usage.CacheCreationInputTokens
				s.cacheReadTokens = msg.Usage.CacheReadInputTokens
				s.cachedInputTokens = s.cacheCreationTokens + s.cacheReadTokens
			}
		}
		return StreamEvent{}, false

	case "content_block_start":
		var cb struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(env.ContentBlock, &cb); err != nil {
			return StreamEvent{}, false
		}
		switch cb.Type {
		case "text":
			s.currentText = &strings.Builder{}
		case "tool_use":
			s.currentTool = &toolBuilder{id: cb.ID, name: cb.Name}
			// input may be {} initially; deltas fill it in.
		}
		return StreamEvent{}, false

	case "content_block_delta":
		var d struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		}
		if err := json.Unmarshal(env.Delta, &d); err != nil {
			return StreamEvent{}, false
		}
		switch d.Type {
		case "text_delta":
			if s.currentText != nil {
				s.currentText.WriteString(d.Text)
			}
			return StreamEvent{Kind: "text_delta", Text: d.Text}, true
		case "input_json_delta":
			if s.currentTool != nil {
				s.currentTool.input.WriteString(d.PartialJSON)
			}
			return StreamEvent{}, false
		}
		return StreamEvent{}, false

	case "content_block_stop":
		if s.currentText != nil {
			s.parts = append(s.parts, message.TextContent{Text: s.currentText.String()})
			s.currentText = nil
		}
		if s.currentTool != nil {
			tc := message.ToolCall{
				ID:       s.currentTool.id,
				Name:     s.currentTool.name,
				Input:    s.currentTool.input.String(),
				Type:     "tool_use",
				Finished: true,
			}
			// Empty input string parses to {} so downstream JSON
			// unmarshal in the tools doesn't fail on an empty body.
			if strings.TrimSpace(tc.Input) == "" {
				tc.Input = "{}"
			}
			s.parts = append(s.parts, tc)
			s.currentTool = nil
			ev := StreamEvent{Kind: "tool_use", ToolCall: &tc}
			return ev, true
		}
		return StreamEvent{}, false

	case "message_delta":
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(env.Delta, &d); err == nil {
			if d.StopReason != "" {
				s.stopReason = d.StopReason
			}
		}
		// usage is at the top level of the frame, not in the
		// delta. message_delta's usage carries the FINAL output
		// token count for the whole turn (Anthropic's spec —
		// the running counter lives in the message_delta event,
		// not in content_block_delta). Last write wins, so the
		// final message_delta of the turn settles the count.
		if env.Usage != nil && env.Usage.OutputTokens > 0 {
			s.outputTokens = env.Usage.OutputTokens
		}
		return StreamEvent{}, false

	case "message_stop":
		return StreamEvent{Kind: "done", Final: s.finalize()}, true

	case "error":
		if env.Error != nil {
			return StreamEvent{Kind: "error",
				Err: fmt.Errorf("kailab provider: %s: %s", env.Error.Type, env.Error.Message)}, true
		}
		return StreamEvent{Kind: "error", Err: fmt.Errorf("kailab provider: unspecified upstream error")}, true
	}
	return StreamEvent{}, false
}

func (s *sseState) finalize() *Response {
	return &Response{
		Parts:               s.parts,
		FinishReason:        mapStopReason(s.stopReason),
		InputTokens:         s.inputTokens,
		OutputTokens:        s.outputTokens,
		CachedInputTokens:   s.cachedInputTokens,
		CacheCreationTokens: s.cacheCreationTokens,
		CacheReadTokens:     s.cacheReadTokens,
	}
}

// --- request translation ---------------------------------------------

// buildAnthropicRequest converts the internal Request to the JSON
// shape Anthropic's Messages API accepts. Tool definitions are
// flattened to {name, description, input_schema}; messages are
// serialized as content-block arrays so tool_use / tool_result
// blocks fit naturally.
//
// Prompt caching: we tag the system prompt and the tools array
// with `cache_control: {type: "ephemeral"}` so subsequent turns
// within an agent run hit Anthropic's 5-minute prompt cache. Cache
// reads bill at ~10% of normal input cost — across a multi-turn
// agent run with tool calls, that's a 70-90% savings on the
// system+tools slice (which is normally the dominant fixed cost).
//
// We keep just two cache breakpoints (the API allows up to 4):
//   - system: the chat system prompt + injected overview
//   - tools: the JSON-schema definitions for kai_*, view, write, etc.
//
// Both are stable across turns within a single agent.Run, so the
// cache hit rate is effectively 100% on turn 2 and beyond.
func buildAnthropicRequest(req Request) map[string]interface{} {
	out := map[string]interface{}{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   serializeMessages(req.Messages, req.EphemeralTailMessages),
	}
	if s := strings.TrimSpace(req.System); s != "" {
		// System stays as a string for now — the kailab proxy's
		// llmRequest type expects `system: string` and rejects the
		// content-array form Anthropic also accepts. When the
		// proxy is updated to accept either shape, switch to:
		//   out["system"] = []map[string]interface{}{
		//     {"type": "text", "text": s,
		//      "cache_control": map[string]string{"type": "ephemeral"}},
		//   }
		// to add a cache breakpoint on the system prompt too.
		out["system"] = s
	}
	if len(req.Tools) > 0 {
		serialized := serializeTools(req.Tools)
		// Marking the LAST tool with cache_control instructs
		// Anthropic to cache the entire tools array up to and
		// including that tool. We always set it on the last
		// entry — tools are stable so the boundary doesn't shift
		// between calls. The kailab proxy passes per-tool fields
		// through unchanged, so this works without a server change.
		if n := len(serialized); n > 0 {
			serialized[n-1]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		out["tools"] = serialized
	}
	// Anthropic's structured-outputs path: when the caller supplied a
	// JSON Schema, attach it as `output_config.format` so the model's
	// final text block is constrained to match. Tool calls during
	// exploration turns are not affected — only the terminal response.
	// Providers that don't honor this field (the OpenAI-shaped route)
	// receive the request without it; we filter on the kai-cli side
	// rather than on the proxy because kailab-control passes unknown
	// top-level fields through to upstream verbatim.
	if len(req.OutputJSONSchema) > 0 {
		out["output_config"] = map[string]interface{}{
			"format": map[string]interface{}{
				"type":   "json_schema",
				"schema": req.OutputJSONSchema,
			},
		}
	}
	// Anthropic's tool_choice: when the caller set RequireToolUse,
	// the model is forced to start with a tool call. {"type":"any"}
	// means "any tool you've been given," which is what we want for
	// the planner's forced-exploration reprompt — the model picks
	// kai_grep or view or whatever, but it CANNOT emit a final text
	// block (and therefore can't emit a hallucinated JSON plan)
	// without first observing some code via tools.
	if req.RequireToolUse {
		out["tool_choice"] = map[string]interface{}{"type": "any"}
	}
	return out
}

func serializeTools(ts []tools.ToolInfo) []map[string]interface{} {
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
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": schema,
		})
	}
	return out
}

// serializeMessages converts our internal Message slice to Anthropic's
// message array. Each Message becomes one entry whose content is an
// array of typed blocks (text / tool_use / tool_result). Roles are
// passed through unchanged ("user", "assistant"); system role is
// hoisted to the top-level `system` field by the caller.
//
// Prompt caching: we mark the LAST content block of the LAST message
// with cache_control: ephemeral. Anthropic interprets this as "every
// token up to and including this point is cacheable as one unit." On
// the next turn (same conversation, ≤5 minutes later), the prefix
// matches and is billed at ~10% of normal input cost.
//
// This matters most for agent loops: each turn re-sends the entire
// conversation including all prior tool results, which would be N²
// without caching. With this breakpoint, a 6-turn planner run that
// would've billed ~250k uncached input tokens bills ~30k fresh +
// ~220k cached — roughly 5× cost reduction. The cache also makes
// the latency cheaper because cached tokens skip re-prefill.
//
// Anthropic allows up to 4 cache breakpoints per request. We use
// two: one on the tools array (set in buildAnthropicRequest) and
// one here on the conversation history. The remaining two are
// available for finer-grained breakpoints if we need them later.
func serializeMessages(msgs []message.Message, ephemeralTail int) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == message.RoleSystem {
			continue // handled by buildAnthropicRequest
		}
		blocks := make([]map[string]interface{}, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch v := p.(type) {
			case message.TextContent:
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": v.Text,
				})
			case message.ToolCall:
				var input map[string]interface{}
				_ = json.Unmarshal([]byte(v.Input), &input)
				if input == nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    v.ID,
					"name":  v.Name,
					"input": input,
				})
			case message.ToolResult:
				blocks = append(blocks, map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": v.ToolCallID,
					"content":     v.Content,
					"is_error":    v.IsError,
				})
			}
		}
		out = append(out, map[string]interface{}{
			"role":    string(m.Role),
			"content": blocks,
		})
	}

	// Place up to 3 cache_control breakpoints in the messages array
	// (Anthropic allows 4 total; tools uses 1). Each breakpoint
	// becomes an independent cached prefix Anthropic can match
	// against on subsequent turns. Strategy:
	//
	//   - Walk backward from len(out)-1-ephemeralTail (the last
	//     canonical message; the ephemeral tail is excluded so
	//     per-turn hint bytes don't pollute the cache extent).
	//   - Mark up to 3 user-role messages, walking back. Skip
	//     assistant messages; their bytes are stable too but the
	//     conventional cache breakpoint sits on user messages
	//     (tool_results / seed prompt), and rolling breakpoints
	//     across multiple turns means each new turn's freshest
	//     marker is already at a position the prior turn ALSO
	//     marked — so Anthropic's lookup can match.
	//
	// Why 3: with one breakpoint, the marker position drifts each
	// turn (history grows by ~2 messages per tool round) and a
	// lookup at the new dynamic end finds no cached prefix at
	// THAT exact position. Empirically that produced ~50%
	// turn-by-turn cache misses (May 2026 run-log evidence:
	// turns 1, 2, 5, 10 missed with cache_read=0 while turns
	// 3, 8, 9 hit). Three rolling markers keep at least one
	// position stable across consecutive turn-pairs.
	//
	// Defensive: skip if no eligible messages — better to send
	// uncached than to crash on malformed inputs.
	const maxMessageBreakpoints = 3
	canonicalEnd := len(out) - 1 - ephemeralTail
	if canonicalEnd < 0 {
		canonicalEnd = len(out) - 1 // fall back: ephemeralTail mis-set
	}
	placed := 0
	for i := canonicalEnd; i >= 0 && placed < maxMessageBreakpoints; i-- {
		role, _ := out[i]["role"].(string)
		if role != "user" {
			continue
		}
		blocks, ok := out[i]["content"].([]map[string]interface{})
		if !ok || len(blocks) == 0 {
			continue
		}
		blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
		placed++
	}
	// Fallback: if no user messages were marked (unusual — empty
	// history or all-assistant prefix), put one breakpoint on the
	// canonical-end message regardless of role so something gets
	// cached. Worse than the user-message path but better than
	// nothing.
	if placed == 0 && canonicalEnd >= 0 {
		blocks, ok := out[canonicalEnd]["content"].([]map[string]interface{})
		if ok && len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
	}

	return out
}

// --- response translation --------------------------------------------

// anthropicResponse mirrors Anthropic's Messages API response shape,
// limited to the fields the runner consumes.
type anthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      struct {
		InputTokens             int `json:"input_tokens"`
		OutputTokens            int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicContent struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`
	// thinking block (optional, when extended thinking is on)
	Thinking string `json:"thinking,omitempty"`
}

// parseAnthropicResponse maps the wire shape back into our internal
// ContentPart slice + finish reason. Unknown content-block types are
// ignored (forward compat with future Anthropic additions).
func parseAnthropicResponse(raw anthropicResponse) Response {
	out := Response{
		FinishReason:        mapStopReason(raw.StopReason),
		InputTokens:         raw.Usage.InputTokens,
		OutputTokens:        raw.Usage.OutputTokens,
		CachedInputTokens:   raw.Usage.CacheCreationInputTokens + raw.Usage.CacheReadInputTokens,
		CacheCreationTokens: raw.Usage.CacheCreationInputTokens,
		CacheReadTokens:     raw.Usage.CacheReadInputTokens,
	}
	for _, c := range raw.Content {
		switch c.Type {
		case "text":
			out.Parts = append(out.Parts, message.TextContent{Text: c.Text})
		case "thinking":
			out.Parts = append(out.Parts, message.ReasoningContent{Thinking: c.Thinking})
		case "tool_use":
			inputJSON, _ := json.Marshal(c.Input)
			out.Parts = append(out.Parts, message.ToolCall{
				ID:       c.ID,
				Name:     c.Name,
				Input:    string(inputJSON),
				Type:     "tool_use",
				Finished: true,
			})
		}
	}
	return out
}

func mapStopReason(r string) message.FinishReason {
	switch r {
	case "end_turn":
		return message.FinishReasonEndTurn
	case "tool_use":
		return message.FinishReasonToolUse
	case "max_tokens":
		return message.FinishReasonMaxTokens
	case "stop_sequence":
		return message.FinishReasonEndTurn
	default:
		return message.FinishReasonUnknown
	}
}
