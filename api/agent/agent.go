// Package agent is the TUI's re-export of kai-cli/internal/agent.
package agent

import engine "kai/internal/agent"

type Mode = engine.Mode
type Hooks = engine.Hooks
type Options = engine.Options
type Result = engine.Result

const (
	ModeUnknown      = engine.ModeUnknown
	ModeCoding       = engine.ModeCoding
	ModePlanning     = engine.ModePlanning
	ModeReview       = engine.ModeReview
	ModeDebug        = engine.ModeDebug
	ModeConversation = engine.ModeConversation
)

var (
	ParseMode        = engine.ParseMode
	ResolveMode      = engine.ResolveMode
	DetectMode       = engine.DetectMode
	Run              = engine.Run
	IsReasoningModel = engine.IsReasoningModel
)
