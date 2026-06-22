package errors

import (
	"kai/api/telemetry"
)

// Report is the single chokepoint that pipes a classified error
// into both the local errors.log AND PostHog telemetry. Callers
// (the TUI render sites) invoke it AFTER they've decided what
// to render — Report doesn't change UI state, only persists
// observability.
//
// workspace is the directory used to find .kai/ for the local
// log. When empty (no workspace context), the local log is
// skipped but telemetry still fires.
//
// autoRepaired tells us whether the heal succeeded. Tracked
// separately from severity because a Block-severity error
// CAN auto-repair (e.g. provider 401 → silent token refresh)
// without changing the user-facing render.
//
// Telemetry runs only if the user has it enabled (default on,
// per package telemetry's IsEnabled()). Local log always runs.
// Both are best-effort — Report never returns errors and never
// blocks the caller.
func Report(workspace string, ue UserError, autoRepaired bool) {
	if ue.Kind == "" || ue.Kind == "none" {
		return
	}
	LogLocal(workspace, ue, autoRepaired)
	telemetry.ReportError(
		ue.Kind,
		ue.Headline,
		ue.LogContext,
		autoRepaired,
		severityName(ue.Severity),
		ue.Context,
	)
}
