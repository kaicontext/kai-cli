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
//
// What goes to PostHog is the kind, the severity and whether the
// auto-repair worked — nothing else. LogContext is err.Error(),
// which for a file error is the full path and for a URL error the
// URL; and Headline is raw text on three rules (a provider's cap
// message, the build-regression lede, the multiroot first line).
// Both stay in the local log, where they are useful, and are never
// sent. The error_seen board groups on kind alone.
func Report(workspace string, ue UserError, autoRepaired bool) {
	report(workspace, ue, autoRepaired, telemetry.ReportError)
}

// report is Report with the telemetry call passed in, so a test can
// see what would be sent without swapping package state.
func report(workspace string, ue UserError, autoRepaired bool, send func(kind, headline, raw string, autoRepaired bool, severity string, ctx map[string]any)) {
	if ue.Kind == "" || ue.Kind == "none" {
		return
	}
	LogLocal(workspace, ue, autoRepaired)
	// Context is not sent either. Nothing populates it today; if a
	// whitelisted context is ever added, it has to be admitted here on
	// purpose, not forwarded by default.
	send(
		ue.Kind,
		"", // headline: may carry raw text; not sent
		"", // raw message: err.Error(); not sent
		autoRepaired,
		severityName(ue.Severity),
		nil,
	)
}
