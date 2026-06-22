package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// kaiConsoleTool attaches to a Chromium / Electron instance that's
// already running with --remote-debugging-port=<N> and captures
// console events + uncaught exceptions for a bounded window. This is
// the RT-1 sense organ: the planner can finally see runtime errors
// (the kind of TypeError that fires in a sandboxed preload script
// and never appears in static analysis) without a human running the
// app and reading DevTools.
//
// Attach-only on purpose. v1 deliberately does NOT launch the target
// — the lifecycle (port collision, "is the app ready?", who kills
// it) is the user's problem, not kai's. The user runs their app with
// --remote-debugging-port=9222 (or similar), and kai connects.
// Launch is a later add that composes with kai_exec when that ships.
type kaiConsoleTool struct{}

type kaiConsoleParams struct {
	Port       int    `json:"port"`
	DurationMS int    `json:"duration_ms"`
	All        bool   `json:"all"`
	URLPrefix  string `json:"url_prefix"`
}

const (
	defaultConsolePort     = 9222
	defaultConsoleDuration = 5000
	maxConsoleDuration     = 30000
	// maxEvents caps total events returned so a chatty page can't
	// blow the token budget. Same shape as the TOK read-cost work:
	// tail-bound the output before the planner sees it.
	maxConsoleEvents = 200
	// maxArgValueBytes truncates individual console.log argument
	// previews — a single console.log of a large object should not
	// take over the response. Per-arg, not per-event.
	maxArgValueBytes = 400
)

func (t *kaiConsoleTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_console",
		Description: "Attach to a running Chromium/Electron app via the DevTools Protocol and " +
			"capture console messages + uncaught exceptions for a bounded window. The target " +
			"must already be running with --remote-debugging-port=<port>. Use this when the " +
			"symptom is a runtime/UI bug — a value that shouldn't be empty, a screen that " +
			"shows defaults, a feature that silently doesn't work — and static analysis of " +
			"the source files doesn't explain it. Returns text: one line per event with " +
			"severity, source location, and the message.",
		Parameters: map[string]any{
			"port": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Remote debugging port to connect to. Default %d.", defaultConsolePort),
			},
			"duration_ms": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How long to capture events, in milliseconds. Default %d, hard cap %d.", defaultConsoleDuration, maxConsoleDuration),
			},
			"all": map[string]any{
				"type": "boolean",
				"description": "If true, include info/debug/log messages too. By default only " +
					"errors, warnings, and uncaught exceptions are returned — those are what " +
					"matter for runtime debugging. Set true if you need full console history.",
			},
			"url_prefix": map[string]any{
				"type": "string",
				"description": "Optional URL prefix to filter which debug target to attach to. " +
					"Useful when multiple pages are open (DevTools itself, the app, an iframe). " +
					"Defaults to picking the first non-devtools target.",
			},
		},
	}
}

func (t *kaiConsoleTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var p kaiConsoleParams
	if len(call.Input) > 0 {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_console: invalid input json: " + err.Error()), nil
		}
	}
	if p.Port == 0 {
		p.Port = defaultConsolePort
	}
	if p.DurationMS == 0 {
		p.DurationMS = defaultConsoleDuration
	}
	if p.DurationMS > maxConsoleDuration {
		p.DurationMS = maxConsoleDuration
	}

	target, err := discoverTarget(ctx, p.Port, p.URLPrefix)
	if err != nil {
		return NewTextErrorResponse("kai_console: " + err.Error()), nil
	}

	captureCtx, cancel := context.WithTimeout(ctx, time.Duration(p.DurationMS)*time.Millisecond+5*time.Second)
	defer cancel()

	events, err := captureCDPEvents(captureCtx, target.WebSocketDebuggerURL, time.Duration(p.DurationMS)*time.Millisecond)
	if err != nil {
		return NewTextErrorResponse("kai_console: " + err.Error()), nil
	}

	return NewTextResponse(formatEvents(events, target.URL, p.All)), nil
}

// cdpTarget is the JSON shape returned by /json/list. We only need
// the WebSocket URL and the page URL (for filtering + reporting).
type cdpTarget struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// discoverTarget hits the DevTools HTTP discovery endpoint and picks
// a target to attach to. Filters out devtools:// frontends (those
// can't be debugged from outside) and prefers a user-supplied URL
// prefix when given.
func discoverTarget(ctx context.Context, port int, urlPrefix string) (*cdpTarget, error) {
	url := fmt.Sprintf("http://localhost:%d/json/list", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// The most common failure: nothing listening on the port.
		// Surface the actionable hint, not just the dial error.
		return nil, fmt.Errorf("no debugger on port %d — run the target with --remote-debugging-port=%d (%s)", port, port, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("debugger at localhost:%d returned HTTP %d: %s", port, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("debugger response was not valid json: %w", err)
	}
	for i := range targets {
		t := &targets[i]
		if t.WebSocketDebuggerURL == "" {
			continue
		}
		// Skip the DevTools UI itself — we want the page it's
		// inspecting, not the inspector.
		if strings.HasPrefix(t.URL, "devtools://") {
			continue
		}
		if urlPrefix != "" && !strings.HasPrefix(t.URL, urlPrefix) {
			continue
		}
		return t, nil
	}
	if urlPrefix != "" {
		return nil, fmt.Errorf("no debug target matched url_prefix=%q on port %d", urlPrefix, port)
	}
	return nil, fmt.Errorf("debugger on port %d has no attachable targets", port)
}

// cdpEvent is the minimal shape we extract from CDP messages. Both
// Runtime.consoleAPICalled and Runtime.exceptionThrown collapse into
// this single shape — the planner doesn't care which CDP method
// fired, only "what severity, what message, where."
type cdpEvent struct {
	Severity string // "error" | "warning" | "log" | "info" | "debug" | "exception"
	Message  string
	Source   string // optional file:line:col
	Time     time.Time
}

// captureCDPEvents opens the WebSocket, enables Runtime + Log
// domains, and drains events until the duration elapses or the
// connection drops. We do NOT reload the page — that's a different
// design choice (capture-only attach), matching the spec's
// "lifecycle isn't kai's problem" stance.
func captureCDPEvents(ctx context.Context, wsURL string, duration time.Duration) ([]cdpEvent, error) {
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 5 * time.Second
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close()

	// Enable the two domains that surface what we want. Log.enable
	// produces browser-level diagnostics (network errors, CSP
	// violations); Runtime.enable produces console.* and uncaught
	// exceptions. We don't wait for the responses — Chrome ack's
	// quickly and any race just means we miss a few events from
	// the very first millisecond, which is fine.
	for id, method := range []string{"Runtime.enable", "Log.enable"} {
		msg := map[string]any{"id": id + 1, "method": method}
		raw, _ := json.Marshal(msg)
		if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			return nil, fmt.Errorf("enable %s: %w", method, err)
		}
	}

	deadline := time.Now().Add(duration)
	conn.SetReadDeadline(deadline)

	events := make([]cdpEvent, 0, 32)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// Timeout (deadline elapsed) is the expected exit; any
			// other read error we surface as a partial result with
			// a note appended at format time.
			if isExpectedClose(err) {
				break
			}
			break
		}
		ev, ok := parseCDPMessage(raw)
		if !ok {
			continue
		}
		events = append(events, ev)
		if len(events) >= maxConsoleEvents {
			break
		}
	}
	return events, nil
}

// isExpectedClose treats net deadline (read timeout) and normal
// close as the boring end-of-capture path so we don't return an
// error for the duration expiring as designed.
func isExpectedClose(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "deadline exceeded") {
		return true
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
		return true
	}
	return false
}

// parseCDPMessage extracts the events we care about. Returns ok=false
// for messages that aren't consoleAPICalled / exceptionThrown / Log.
func parseCDPMessage(raw []byte) (cdpEvent, bool) {
	var env struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return cdpEvent{}, false
	}
	switch env.Method {
	case "Runtime.consoleAPICalled":
		return parseConsoleAPI(env.Params), true
	case "Runtime.exceptionThrown":
		return parseException(env.Params), true
	case "Log.entryAdded":
		return parseLogEntry(env.Params), true
	}
	return cdpEvent{}, false
}

func parseConsoleAPI(params json.RawMessage) cdpEvent {
	var p struct {
		Type       string `json:"type"`
		Timestamp  float64
		Args       []cdpRuntimeValue `json:"args"`
		StackTrace cdpStackTrace     `json:"stackTrace"`
	}
	_ = json.Unmarshal(params, &p)
	ev := cdpEvent{Severity: p.Type, Time: timeFromMillis(p.Timestamp)}
	var parts []string
	for _, a := range p.Args {
		parts = append(parts, a.preview())
	}
	ev.Message = strings.Join(parts, " ")
	ev.Source = p.StackTrace.firstFrame()
	return ev
}

func parseException(params json.RawMessage) cdpEvent {
	var p struct {
		ExceptionDetails struct {
			Text             string `json:"text"`
			LineNumber       int    `json:"lineNumber"`
			ColumnNumber     int    `json:"columnNumber"`
			URL              string `json:"url"`
			Exception        cdpRuntimeValue
			StackTrace       cdpStackTrace `json:"stackTrace"`
		} `json:"exceptionDetails"`
		Timestamp float64
	}
	_ = json.Unmarshal(params, &p)
	ev := cdpEvent{Severity: "exception", Time: timeFromMillis(p.Timestamp)}
	d := p.ExceptionDetails
	desc := d.Exception.Description
	if desc == "" {
		desc = d.Text
	}
	ev.Message = desc
	if d.URL != "" {
		ev.Source = fmt.Sprintf("%s:%d:%d", d.URL, d.LineNumber, d.ColumnNumber)
	} else {
		ev.Source = d.StackTrace.firstFrame()
	}
	return ev
}

func parseLogEntry(params json.RawMessage) cdpEvent {
	var p struct {
		Entry struct {
			Source    string `json:"source"`
			Level     string `json:"level"`
			Text      string `json:"text"`
			Timestamp float64
			URL       string `json:"url"`
			LineNumber int   `json:"lineNumber"`
		} `json:"entry"`
	}
	_ = json.Unmarshal(params, &p)
	src := p.Entry.Source
	if p.Entry.URL != "" {
		src = fmt.Sprintf("%s:%d", p.Entry.URL, p.Entry.LineNumber)
	}
	return cdpEvent{
		Severity: p.Entry.Level,
		Message:  p.Entry.Text,
		Source:   src,
		Time:     timeFromMillis(p.Entry.Timestamp),
	}
}

type cdpRuntimeValue struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	ClassName   string          `json:"className"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
}

// preview renders a single console arg as a short string. The CDP
// "Runtime.RemoteObject" shape varies (primitives have Value;
// objects/errors have Description + ClassName); we want a single
// readable form regardless.
func (v cdpRuntimeValue) preview() string {
	var s string
	switch {
	case v.Description != "":
		s = v.Description
	case len(v.Value) > 0:
		// Value is JSON. Strip the leading/trailing quotes for
		// strings so the output reads naturally.
		val := strings.TrimSpace(string(v.Value))
		if strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`) && len(val) >= 2 {
			val = val[1 : len(val)-1]
		}
		s = val
	case v.ClassName != "":
		s = "[" + v.ClassName + "]"
	case v.Type != "":
		s = "[" + v.Type + "]"
	}
	if len(s) > maxArgValueBytes {
		s = s[:maxArgValueBytes] + "…"
	}
	return s
}

type cdpStackTrace struct {
	CallFrames []struct {
		FunctionName string `json:"functionName"`
		URL          string `json:"url"`
		LineNumber   int    `json:"lineNumber"`
		ColumnNumber int    `json:"columnNumber"`
	} `json:"callFrames"`
}

func (s cdpStackTrace) firstFrame() string {
	if len(s.CallFrames) == 0 {
		return ""
	}
	f := s.CallFrames[0]
	if f.URL == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", f.URL, f.LineNumber, f.ColumnNumber)
}

// timeFromMillis: CDP timestamps are milliseconds since epoch as
// float64. Round to a Time.
func timeFromMillis(ms float64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	sec := int64(ms / 1000)
	nsec := int64((ms - float64(sec)*1000) * 1e6)
	return time.Unix(sec, nsec)
}

// formatEvents builds the text response. One line per event. Filters
// out info/debug/log unless all=true — the default is "things that
// matter for runtime debugging" because returning a thousand
// console.log lines from the dev-server framework noise is itself a
// failure mode (token budget, signal-to-noise).
func formatEvents(events []cdpEvent, targetURL string, all bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kai_console: attached to %s — %d event(s) captured", targetURL, len(events))
	shown := 0
	for _, ev := range events {
		if !all && !isInterestingSeverity(ev.Severity) {
			continue
		}
		shown++
	}
	if shown == 0 && len(events) > 0 {
		fmt.Fprintf(&b, " (none at error/warning/exception severity — pass all=true to see info/debug/log)\n")
		return b.String()
	}
	if shown != len(events) {
		fmt.Fprintf(&b, " (%d shown, %d filtered as info/debug/log; pass all=true to see)\n", shown, len(events)-shown)
	} else {
		b.WriteString("\n")
	}
	for _, ev := range events {
		if !all && !isInterestingSeverity(ev.Severity) {
			continue
		}
		msg := strings.ReplaceAll(ev.Message, "\n", " ")
		if ev.Source != "" {
			fmt.Fprintf(&b, "  [%s] %s\n      at %s\n", ev.Severity, msg, ev.Source)
		} else {
			fmt.Fprintf(&b, "  [%s] %s\n", ev.Severity, msg)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func isInterestingSeverity(s string) bool {
	switch s {
	case "error", "warning", "exception", "assert":
		return true
	}
	return false
}
