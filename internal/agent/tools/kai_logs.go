// kai_logs: agent-callable tool to read recent output from the
// managed dev-server process kai is watching (see v0.32.0
// host_proc.go for the process model).
//
// Why this exists: prior to v0.32.1 the agent had no way to answer
// questions like "do you see the error?" — the managed process's
// output buffer was reachable only by the background scanner that
// emits error events. The agent literally couldn't query its
// contents on demand, so it would honestly say "I don't see an
// error" even when one was sitting in the buffer. kai_logs closes
// that gap with a simple read-only accessor.
//
// Boundaries:
//   - Read-only. No control over the process (use /stop for that).
//   - Returns at most logsMaxBytes of the most recent output —
//     enough for the model to read the error context without
//     blowing the token budget on the full ring buffer.
//   - Registers only when a ManagedProcLogger is configured. Tests
//     and non-TUI callers (orchestrator-spawned agents) get the
//     tool silently omitted, same pattern as kai_consult /
//     kai_web_search.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ManagedProcLogger is the interface the TUI implements to give
// kai_logs read access to the managed process state. Defined here
// (not in tui/views) so the tools package stays cycle-free — same
// pattern as the Sender interface for kai_consult.
type ManagedProcLogger interface {
	// RecentLogs returns (command, output, running). When no
	// managed process is active, running is false and command/
	// output are empty. The TUI implementation reads from the
	// ring buffer's Snapshot().
	RecentLogs() (command string, output string, running bool)
}

type kaiLogsTool struct {
	logger ManagedProcLogger
}

type kaiLogsParams struct {
	// Lines limits the result to the last N non-empty lines. Default
	// kaiLogsDefaultLines, capped at kaiLogsMaxLines. 0 = unbounded
	// (up to logsMaxBytes).
	Lines int `json:"lines"`
}

const (
	kaiLogsDefaultLines = 80
	kaiLogsMaxLines     = 500
	// logsMaxBytes caps the total response size regardless of Lines.
	// Prevents a chatty webpack-style log from blowing the model's
	// token budget on a single call.
	logsMaxBytes = 20000
)

func (t *kaiLogsTool) Info() ToolInfo {
	return ToolInfo{
		Name: "kai_logs",
		Description: "Returns recent output from the managed dev-server process kai is watching. " +
			"Use this to answer questions like 'do you see the error?' / 'is the build still working?' / " +
			"'what is the dev server saying?'. " +
			"Returns at most ~80 lines (configurable) of the most recent stdout/stderr. " +
			"Empty result means no managed process is running — that's the signal you'd tell the user.",
		Parameters: map[string]any{
			"lines": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Last N lines to return. Default %d, max %d. 0 = whatever fits in ~%dKB.", kaiLogsDefaultLines, kaiLogsMaxLines, logsMaxBytes/1000),
				"default":     kaiLogsDefaultLines,
			},
		},
		Required: []string{},
	}
}

func (t *kaiLogsTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if t.logger == nil {
		return NewTextErrorResponse("kai_logs: not configured (no managed process channel)"), nil
	}
	var p kaiLogsParams
	if call.Input != "" {
		if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
			return NewTextErrorResponse("kai_logs: invalid input json: " + err.Error()), nil
		}
	}
	command, output, running := t.logger.RecentLogs()
	if !running {
		return NewTextResponse("No managed process is currently running. Tell the user to start one (e.g. 'run it' for a dev server) before asking about its logs."), nil
	}
	lines := p.Lines
	if lines == 0 {
		lines = kaiLogsDefaultLines
	}
	if lines > kaiLogsMaxLines {
		lines = kaiLogsMaxLines
	}
	out := tailLines(output, lines)
	if len(out) > logsMaxBytes {
		// Truncate from the FRONT (keep the most recent content)
		// and prepend a marker so the model knows. The model's
		// usual ask is "what's wrong NOW" — newest matters most.
		out = "[... earlier output truncated ...]\n" + out[len(out)-logsMaxBytes:]
	}
	return NewTextResponse(fmt.Sprintf("Managed process: %s\n\n%s", command, out)), nil
}

// tailLines returns the last n non-empty lines from s, preserving
// chronological order. Empty lines are skipped from the count but
// preserved if they fall between kept lines (visually meaningful in
// build output).
func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	all := strings.Split(strings.TrimRight(s, "\n"), "\n")
	// Walk backwards counting non-empty lines until we have n, then
	// slice from that point. Cheap O(len(all)).
	kept := 0
	startIdx := len(all)
	for i := len(all) - 1; i >= 0; i-- {
		if strings.TrimSpace(all[i]) != "" {
			kept++
			if kept >= n {
				startIdx = i
				break
			}
		}
	}
	return strings.Join(all[startIdx:], "\n")
}
