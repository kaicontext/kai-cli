package views

import (
	"strings"
)

// formatToolEvent renders a tool-dispatch line for a ChatActivityEvent with
// Kind=="tool". Modeled on the neighboring diff/bash/gate arms in the
// ChatActivityMsg switch — reuses styleDim from the existing style
// vocabulary so tool dispatches read as a peer of those events.
//
// Producers (planner/agent runner) populate Summary with a short
// "tool: args" string such as "bash: ls -la". We split on the first ": "
// so the tool name renders plain and the args render dim.
func formatToolEvent(ev ChatActivityEvent, width int) string {
	summary := strings.TrimSpace(ev.Summary)
	// Defensive strip: some producers (the orchestrator bridge in
	// app.go's OnActivity handler is the canonical case) already
	// include "→ " in Summary. Without this strip we double-prepend
	// and the activity feed renders "→ → spawn-name: ..." — visible
	// in 2026-05-14 dogfood after the new "tool" switch arm landed.
	summary = strings.TrimPrefix(summary, "→ ")
	if summary == "" {
		summary = "tool"
	}
	tool, args := summary, ""
	if i := strings.Index(summary, ": "); i > 0 {
		tool = summary[:i]
		args = strings.TrimSpace(summary[i+2:])
	}

	head := "→ " + tool
	if args == "" {
		return head
	}
	// Budget the args line to the viewport so long shell commands don't
	// wrap awkwardly. Approximate visible head width as len("→ ") + len(tool).
	if width > 0 {
		visibleHead := len("→ ") + len(tool) + len(": ")
		budget := width - visibleHead
		if budget > 8 && len(args) > budget {
			args = args[:budget-1] + "…"
		}
	}
	return head + styleDim.Render(": "+args)
}
