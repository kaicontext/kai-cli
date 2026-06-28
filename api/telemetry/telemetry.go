// Package telemetry is the TUI's re-export of kai-cli/internal/telemetry.
package telemetry

import engine "github.com/kaicontext/kai-engine/telemetry"

var (
	NewEvent    = engine.NewEvent
	ReportError = engine.ReportError
	SetVersion  = engine.SetVersion
)
