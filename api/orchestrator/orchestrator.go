// Package orchestrator is the TUI's re-export of kai-cli/internal/orchestrator.
package orchestrator

import engine "github.com/kaicontext/kai-engine/orchestrator"

type Config = engine.Config
type Result = engine.Result
type VerifyResult = engine.VerifyResult

var (
	Execute         = engine.Execute
	VerifyWorkspace = engine.VerifyWorkspace
)
