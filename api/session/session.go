// Package session is the TUI's re-export of kai-cli/internal/agent/session.
package session

import engine "kai/internal/agent/session"

type Store = engine.Store

var (
	New           = engine.New
	Resume        = engine.Resume
	EnsureSchema  = engine.EnsureSchema
	LookupMode    = engine.LookupMode
	SaveMode      = engine.SaveMode
)
