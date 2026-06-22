// Package telemetry is the TUI's re-export of kai-cli/internal/telemetry.
package telemetry

import engine "kai/internal/telemetry"

var (
	NewEvent    = engine.NewEvent
	ReportError = engine.ReportError
	SetVersion  = engine.SetVersion
)
