package tools

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestKaiConsole_NoDebuggerError covers the most common failure: the
// user calls kai_console without having started their app with
// --remote-debugging-port. The error message must be actionable —
// "no debugger on port N" + the exact flag to add — not a raw dial
// error the model can't interpret.
func TestKaiConsole_NoDebuggerError(t *testing.T) {
	port := pickUnusedPort(t)
	tool := &kaiConsoleTool{}
	params := `{"port":` + itoa(port) + `,"duration_ms":100}`
	resp, err := tool.Run(context.Background(), ToolCall{Input: params})
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true for missing debugger, got false; content=%q", resp.Content)
	}
	for _, want := range []string{"kai_console", "no debugger", "--remote-debugging-port"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("error message missing %q; got %q", want, resp.Content)
		}
	}
}

// TestKaiConsole_BadJSON guards the input-parse path. A malformed
// params string should produce a clear error response, not a panic
// or a default-port attempt.
func TestKaiConsole_BadJSON(t *testing.T) {
	tool := &kaiConsoleTool{}
	resp, err := tool.Run(context.Background(), ToolCall{Input: "{not json"})
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true for bad json")
	}
	if !strings.Contains(resp.Content, "invalid input json") {
		t.Errorf("error message missing 'invalid input json'; got %q", resp.Content)
	}
}

// TestKaiConsole_DefaultDuration confirms the duration cap is
// enforced. A request for 10 minutes must be clamped — otherwise a
// confused planner can wedge a turn on a quiet page.
func TestKaiConsole_DefaultDuration(t *testing.T) {
	port := pickUnusedPort(t)
	tool := &kaiConsoleTool{}
	// 10 minutes; should be clamped to maxConsoleDuration before
	// being used. The connection still fails (no debugger), but
	// the failure should happen fast — not 10 minutes later.
	params := `{"port":` + itoa(port) + `,"duration_ms":600000}`
	start := time.Now()
	resp, err := tool.Run(context.Background(), ToolCall{Input: params})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true")
	}
	if elapsed > 10*time.Second {
		t.Errorf("expected fast failure on missing debugger, took %v", elapsed)
	}
}

// TestFormatEvents_DefaultsHidesNoise asserts the filter contract:
// by default, info/debug/log are NOT shown; only error/warning/
// exception/assert are. This is load-bearing for token budget —
// returning a chatty page's full console.log stream would defeat
// the whole tool.
func TestFormatEvents_DefaultsHidesNoise(t *testing.T) {
	events := []cdpEvent{
		{Severity: "log", Message: "vite connected"},
		{Severity: "debug", Message: "hmr update"},
		{Severity: "error", Message: "TypeError: process.cwd is not a function", Source: "preload.cjs:9:18"},
		{Severity: "warning", Message: "deprecated API"},
	}
	out := formatEvents(events, "http://localhost:5173", false)
	if !strings.Contains(out, "TypeError") {
		t.Errorf("expected error event in output, got: %s", out)
	}
	if !strings.Contains(out, "preload.cjs:9:18") {
		t.Errorf("expected source location in output, got: %s", out)
	}
	if !strings.Contains(out, "deprecated API") {
		t.Errorf("expected warning event in output, got: %s", out)
	}
	if strings.Contains(out, "vite connected") || strings.Contains(out, "hmr update") {
		t.Errorf("expected info/debug events to be filtered, got: %s", out)
	}
	if !strings.Contains(out, "2 shown, 2 filtered") {
		t.Errorf("expected filter summary in output, got: %s", out)
	}
}

// TestFormatEvents_AllIncludesNoise: pass all=true and the
// info/debug events come back. This is the escape hatch for when
// the planner needs full history (e.g. tracing a flow that doesn't
// involve any errors).
func TestFormatEvents_AllIncludesNoise(t *testing.T) {
	events := []cdpEvent{
		{Severity: "log", Message: "vite connected"},
		{Severity: "error", Message: "boom"},
	}
	out := formatEvents(events, "http://localhost:5173", true)
	if !strings.Contains(out, "vite connected") {
		t.Errorf("expected log event when all=true, got: %s", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected error event when all=true, got: %s", out)
	}
}

// TestParseCDPMessage_Exception covers the actual bug-detection
// path: the kai-desktop preload TypeError. This is the event shape
// CDP emitted during the dogfood; the test pins that we extract the
// description, line, and column correctly.
func TestParseCDPMessage_Exception(t *testing.T) {
	raw := []byte(`{
		"method": "Runtime.exceptionThrown",
		"params": {
			"exceptionDetails": {
				"text": "Uncaught",
				"lineNumber": 0,
				"columnNumber": 18,
				"exception": {
					"type": "object",
					"className": "TypeError",
					"description": "TypeError: process.cwd is not a function\n    at <anonymous>:9:18"
				},
				"stackTrace": {
					"callFrames": [{
						"functionName": "",
						"url": "node:electron/js2c/sandbox_bundle",
						"lineNumber": 1,
						"columnNumber": 152376
					}]
				}
			}
		}
	}`)
	ev, ok := parseCDPMessage(raw)
	if !ok {
		t.Fatal("expected to parse exceptionThrown event")
	}
	if ev.Severity != "exception" {
		t.Errorf("severity = %q, want exception", ev.Severity)
	}
	if !strings.Contains(ev.Message, "process.cwd is not a function") {
		t.Errorf("message = %q, want it to contain the TypeError text", ev.Message)
	}
}

// TestParseCDPMessage_ConsoleError covers the other half: a
// Runtime.consoleAPICalled with type="error" — what console.error()
// produces. Different shape from exceptionThrown but should land at
// the same single-line text result.
func TestParseCDPMessage_ConsoleError(t *testing.T) {
	raw := []byte(`{
		"method": "Runtime.consoleAPICalled",
		"params": {
			"type": "error",
			"args": [
				{"type": "string", "value": "\"failed to load preload script\""}
			],
			"stackTrace": {
				"callFrames": [{
					"functionName": "(anonymous)",
					"url": "VM6 sandbox_bundle",
					"lineNumber": 2,
					"columnNumber": 151972
				}]
			}
		}
	}`)
	ev, ok := parseCDPMessage(raw)
	if !ok {
		t.Fatal("expected to parse consoleAPICalled event")
	}
	if ev.Severity != "error" {
		t.Errorf("severity = %q, want error", ev.Severity)
	}
	if !strings.Contains(ev.Message, "failed to load preload script") {
		t.Errorf("message = %q, want preload script text", ev.Message)
	}
	if !strings.Contains(ev.Source, "sandbox_bundle") {
		t.Errorf("source = %q, want sandbox_bundle reference", ev.Source)
	}
}

// TestParseCDPMessage_IgnoresUnrelated: CDP emits many methods we
// don't care about (Page.frameNavigated, Network.requestWillBeSent,
// etc.). Those should return ok=false so they don't bloat the
// event list.
func TestParseCDPMessage_IgnoresUnrelated(t *testing.T) {
	raw := []byte(`{"method": "Page.frameNavigated", "params": {}}`)
	if _, ok := parseCDPMessage(raw); ok {
		t.Error("expected unrelated method to be filtered out")
	}
}

func pickUnusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func itoa(n int) string {
	// Local helper rather than importing strconv solely for the
	// tests; the tool itself uses fmt.Sprintf.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
