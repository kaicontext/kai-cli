// Package memstat is the TUI's re-export of kai-cli/internal/memstat.
package memstat

import engine "kai/internal/memstat"

var (
	Log               = engine.Log
	LogBurst          = engine.LogBurst
	StartIdleSampler  = engine.StartIdleSampler
)
