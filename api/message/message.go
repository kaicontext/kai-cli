// Package message is the TUI's re-export of kai-cli/internal/agent/message.
package message

import engine "github.com/kaicontext/kai-engine/message"

type Message = engine.Message
type ContentPart = engine.ContentPart
type TextContent = engine.TextContent
type ToolCall = engine.ToolCall
type FinishReason = engine.FinishReason

const (
	RoleSystem          = engine.RoleSystem
	RoleUser            = engine.RoleUser
	RoleAssistant       = engine.RoleAssistant
	FinishReasonEndTurn = engine.FinishReasonEndTurn
)
