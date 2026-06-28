package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// DebugLog writes structured per-event entries about a planner run
// to a file the developer can `tail -f` while the TUI is running.
// The TUI itself swallows stderr, so this file is the only window
// into what the planner agent is actually doing turn by turn.
//
// Each entry is a single line prefixed with HH:MM:SS.mmm and an
// event tag (TURN, TOOL, RESULT, TEXT, RETRY, ERROR, DONE). Tool
// inputs and assistant text are truncated to keep individual lines
// readable; the truncation length is generous (2KB) so most
// real-world tool calls fit untruncated.
//
// The file is opened in append mode so multiple runs accumulate
// (with a banner separating them). Rotate it manually by deleting
// the file when it gets noisy.
type DebugLog struct {
	mu    sync.Mutex
	f     *os.File
	turn  int
	t0    time.Time
	label string // "PLANNER" / "CHAT" — used by Close banner
}

// OpenDebugLog opens (creating if necessary) the planner debug log
// at <kaiDir>/planner-debug.log and writes a session-start banner.
// Returns nil + nil on any failure to open — debug logging is best-
// effort and must not block the planner from running.
//
// The returned path is what should be displayed to the user so they
// know where to tail.
func OpenDebugLog(kaiDir, request string) (*DebugLog, string) {
	return OpenDebugLogNamed(kaiDir, request, "planner-debug.log", "PLANNER")
}

// OpenChatDebugLog is the chat-agent twin of OpenDebugLog. Writes
// to <kaiDir>/chat-debug.log so chat-agent runs don't intermix
// with planner runs in one file. The chat agent's runs are
// typically more interesting per-turn (tool dispatches, file
// writes, longer assistant text) so a dedicated log keeps them
// readable.
func OpenChatDebugLog(kaiDir, request string) (*DebugLog, string) {
	return OpenDebugLogNamed(kaiDir, request, "chat-debug.log", "CHAT")
}

// OpenDebugLogNamed is the underlying constructor. label is the
// banner tag (e.g. "PLANNER", "CHAT", "ORCHESTRATOR").
func OpenDebugLogNamed(kaiDir, request, filename, label string) (*DebugLog, string) {
	if kaiDir == "" {
		return nil, ""
	}
	path := filepath.Join(kaiDir, filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, ""
	}
	d := &DebugLog{f: f, t0: time.Now()}
	d.writeRaw("\n========================================\n")
	d.writeRaw(fmt.Sprintf("[%s] %s RUN START\n", time.Now().Format(time.RFC3339), label))
	d.writeRaw(fmt.Sprintf("request: %s\n", truncate(strings.TrimSpace(request), 500)))
	d.writeRaw("========================================\n")
	d.label = label
	return d, path
}

// Close releases the underlying file. Safe to call on a nil receiver
// so callers can defer it without nil-checking.
func (d *DebugLog) Close() {
	if d == nil || d.f == nil {
		return
	}
	label := d.label
	if label == "" {
		label = "PLANNER" // backward compat with pre-label callers
	}
	d.writeRaw(fmt.Sprintf("[%s] +%s %s RUN END\n", d.stamp(), d.elapsed(), label))
	_ = d.f.Close()
}

// Request dumps the FULL request the runner is about to send to
// the provider. Used by the OnRequest agent hook so we can
// answer "did the project overview actually reach the model on
// this turn" without guessing.
//
// Output structure:
//
//	REQUEST turn=N model=... system_bytes=N tools_count=N messages_count=N
//	  system: <truncated system prompt>
//	  msg[0] role=user parts=N total_bytes=N
//	    [0] text   bytes=N preview="..."
//	    [1] toolresult id=call_1 bytes=N
//	  msg[1] role=assistant ...
//
// Tool definitions are summarized (name + first 80 chars of
// description) — full schemas would dominate the log.
func (d *DebugLog) Request(turn int, req provider.Request) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	schemaFlag := "none"
	if len(req.OutputJSONSchema) > 0 {
		schemaFlag = fmt.Sprintf("json_schema(%d props)", len(req.OutputJSONSchema))
	}
	d.writeRaw(fmt.Sprintf("[%s] +%s REQUEST turn=%d model=%s system_bytes=%d tools=%d messages=%d output_config=%s\n",
		d.stamp(), d.elapsed(), turn, req.Model, len(req.System), len(req.Tools), len(req.Messages), schemaFlag))
	if len(req.System) > 0 {
		d.writeRaw(fmt.Sprintf("  system: %s\n", truncate(req.System, 1024)))
	}
	for i, m := range req.Messages {
		totalBytes := 0
		for _, p := range m.Parts {
			totalBytes += partBytes(p)
		}
		d.writeRaw(fmt.Sprintf("  msg[%d] role=%s parts=%d total_bytes=%d\n",
			i, m.Role, len(m.Parts), totalBytes))
		for j, p := range m.Parts {
			d.writeRaw(fmt.Sprintf("    [%d] %s\n", j, summarizePart(p)))
		}
	}
}

// partBytes returns the rough payload size of a content part for
// the request-dump's total_bytes column. Used only for human
// orientation; not byte-precise.
func partBytes(p message.ContentPart) int {
	switch v := p.(type) {
	case message.TextContent:
		return len(v.Text)
	case message.ToolCall:
		return len(v.Name) + len(v.Input)
	case message.ToolResult:
		return len(v.Content)
	case message.ReasoningContent:
		return len(v.Thinking)
	}
	return 0
}

// summarizePart renders one part as a single line in the request
// dump. Includes type, size, and a truncated preview so the dump
// is grep-able for "Project overview" etc.
func summarizePart(p message.ContentPart) string {
	switch v := p.(type) {
	case message.TextContent:
		return fmt.Sprintf("text   bytes=%d preview=%q", len(v.Text), truncate(v.Text, 200))
	case message.ToolCall:
		return fmt.Sprintf("toolcall name=%s id=%s bytes=%d input=%s",
			v.Name, v.ID, len(v.Input), truncate(v.Input, 200))
	case message.ToolResult:
		return fmt.Sprintf("toolresult id=%s bytes=%d preview=%q",
			v.ToolCallID, len(v.Content), truncate(v.Content, 200))
	case message.ReasoningContent:
		// 500 chars (vs 200 for text) — reasoning is the diagnostic
		// payload for "why didn't it call a tool?" investigations,
		// and a 200-char preview routinely cuts off the decision
		// sentence ("I'll just give a general overview since..." etc).
		return fmt.Sprintf("reasoning bytes=%d preview=%q", len(v.Thinking), truncate(v.Thinking, 500))
	default:
		raw, _ := json.Marshal(v)
		return fmt.Sprintf("unknown bytes=%d preview=%q", len(raw), truncate(string(raw), 200))
	}
}

// Tool records a tool call dispatch. inputJSON is the model's raw
// tool input; we truncate it to 2KB so a giant grep result snippet
// in the input doesn't drown the log.
func (d *DebugLog) Tool(name, inputJSON string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeRaw(fmt.Sprintf("[%s] +%s TOOL %s %s\n",
		d.stamp(), d.elapsed(), name, truncate(inputJSON, 2048)))
}

// Routing records a single tool-routing decision: which project a
// file path resolved to, which projects a kai_grep walked, which DB a
// graph tool queried. The tools package emits these via
// tools.TraceRouting; the runner wires it up. Used to diagnose
// multi-root failures where the agent calls the right tool but the
// router lands somewhere unexpected.
func (d *DebugLog) Routing(msg string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeRaw(fmt.Sprintf("[%s] +%s ROUTE %s\n",
		d.stamp(), d.elapsed(), truncate(msg, 1024)))
}

// Text records assistant text emitted during a turn. We log the
// full text on completion (via OnAssistantText), not deltas — the
// per-character delta firehose would make the log unreadable.
func (d *DebugLog) Text(text string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeRaw(fmt.Sprintf("[%s] +%s TEXT %s\n",
		d.stamp(), d.elapsed(), truncate(text, 4096)))
}

// Turn records token usage at the end of a model turn. The
// cumulative numbers let the developer eyeball runaway loops:
// a healthy planner run plateaus around 5-15k tokens; a runaway
// climbs steadily past 100k.
func (d *DebugLog) Turn(tokensIn, tokensOut, tokensCached int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turn++
	d.writeRaw(fmt.Sprintf("[%s] +%s TURN %d  in=%d out=%d cached=%d\n",
		d.stamp(), d.elapsed(), d.turn, tokensIn, tokensOut, tokensCached))
}

// Retry records a transient-error backoff so a long wait isn't
// mistaken for a hang.
func (d *DebugLog) Retry(attempt int, delay time.Duration, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeRaw(fmt.Sprintf("[%s] +%s RETRY attempt=%d delay=%s err=%v\n",
		d.stamp(), d.elapsed(), attempt, delay, err))
}

// Errorf records a terminal error (parse failure, runaway cap
// tripped, agent-run error). Last entry before Close in a failed
// run.
func (d *DebugLog) Errorf(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeRaw(fmt.Sprintf("[%s] +%s ERROR %s\n",
		d.stamp(), d.elapsed(), fmt.Sprintf(format, args...)))
}

func (d *DebugLog) stamp() string {
	return time.Now().Format("15:04:05.000")
}

func (d *DebugLog) elapsed() string {
	return time.Since(d.t0).Round(time.Millisecond).String()
}

// writeRaw is the only path that touches the file. Best-effort —
// write errors are swallowed because the calling agent loop must
// not be blocked on a logging failure.
func (d *DebugLog) writeRaw(s string) {
	if d == nil || d.f == nil {
		return
	}
	_, _ = d.f.WriteString(s)
	_ = d.f.Sync() // flush so `tail -f` sees entries immediately
}

// truncate returns s capped at n bytes with a "...(N more)"
// suffix when truncation actually happened. Avoids splitting in
// the middle of a UTF-8 rune by walking back to a safe boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return fmt.Sprintf("%s...(%d more bytes)", s[:cut], len(s)-cut)
}
