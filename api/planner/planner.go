// Package planner is the TUI's re-export of kai-cli/internal/planner.
// Phase 1 of the TUI API extraction.
//
// Function re-exports use value assignments (var X = engine.X)
// instead of wrappers — that way signatures stay in sync with the
// engine automatically.
package planner

import engine "github.com/kaicontext/kai-engine/planner"

type WorkPlan = engine.WorkPlan
type AgentTask = engine.AgentTask
type Config = engine.Config
type Completer = engine.Completer
type PlannerAgent = engine.PlannerAgent

var (
	ErrTooVague      = engine.ErrTooVague
	Plan             = engine.Plan
	Replan           = engine.Replan
	OpenChatDebugLog = engine.OpenChatDebugLog
)
