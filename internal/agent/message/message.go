// Package message defines the wire-format types for an in-progress
// kai agent conversation: roles, content parts, tool calls/results,
// finish reasons. Derived from OpenCode's `internal/message/`
// (MIT-licensed; see ../NOTICE.md and ../LICENSE-OpenCode.md).
//
// Differences from upstream:
//   - The DB-backed `Message` struct from OpenCode's `message.go` is
//     not carried here. Slice 5 will add a Kai-native session
//     persistence layer; until then messages live only in memory in
//     the agent runner.
//   - `BinaryContent.String(provider)` from upstream takes an
//     OpenCode `models.ModelProvider`; we drop that overload and
//     keep base64 only. Image input isn't on the v1 path.
//   - `ContentPart.isPart()` marker preserved so a single slice can
//     hold heterogeneous content (text, reasoning, tool-call, etc.).
package message

import (
	"encoding/base64"
	"time"
)

// Role is the speaker of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// FinishReason is why the model stopped producing tokens. The runner
// reads this to decide whether to keep looping (tool_use) or stop
// (end_turn / max_tokens / error / canceled).
type FinishReason string

const (
	FinishReasonEndTurn   FinishReason = "end_turn"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonToolUse   FinishReason = "tool_use"
	FinishReasonCanceled  FinishReason = "canceled"
	FinishReasonError     FinishReason = "error"
	FinishReasonUnknown   FinishReason = "unknown"
	// FinishReasonTurnCap is the runner's "I've hit the configured
	// turn cap" signal. Distinct from Error so callers (REPL /
	// orchestrator) can offer to continue rather than treat it as a
	// hard failure — turn caps are checkpoints, not crashes.
	FinishReasonTurnCap FinishReason = "turn_cap"
)

// ContentPart is the marker interface for the variants below. Use a
// type switch in the runner to route each part to the right place
// (text → user-visible, ToolCall → dispatch).
type ContentPart interface {
	isPart()
}

// TextContent is plain assistant or user text.
type TextContent struct {
	Text string `json:"text"`
}

func (TextContent) isPart()        {}
func (t TextContent) String() string { return t.Text }

// ReasoningContent is the model's chain-of-thought (when the provider
// surfaces it). Ignored for tool-call dispatch; preserved in the
// transcript for debugging.
type ReasoningContent struct {
	Thinking string `json:"thinking"`
}

func (ReasoningContent) isPart()        {}
func (r ReasoningContent) String() string { return r.Thinking }

// BinaryContent is base64-encodable bytes attached to a user message
// (e.g. an image). Not on the v1 path; included for forward compat.
type BinaryContent struct {
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

func (BinaryContent) isPart() {}

// Base64 returns the data URI form. Provider-specific formatting (e.g.
// OpenAI's `data:<mime>;base64,...` prefix) is the provider adapter's
// concern, not the message type's.
func (b BinaryContent) Base64() string {
	return base64.StdEncoding.EncodeToString(b.Data)
}

// ToolCall is the model asking the runner to invoke a tool.
type ToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    string `json:"input"`
	Type     string `json:"type"`
	Finished bool   `json:"finished"`
}

func (ToolCall) isPart() {}

// ToolResult is the runner's reply to a ToolCall, fed back to the
// model on the next turn.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`
}

func (ToolResult) isPart() {}

// Message is one turn in the conversation. Keeping this struct lean
// (no DB ID, no created_at metadata yet) until Slice 5 wires
// persistence — at that point this becomes the in-memory shape and a
// separate `session.StoredMessage` carries DB columns.
type Message struct {
	Role     Role          `json:"role"`
	Parts    []ContentPart `json:"parts"`
	Time     time.Time     `json:"time,omitempty"`
	Model    string        `json:"model,omitempty"`
	Finished FinishReason  `json:"finished,omitempty"`
}

// Text returns concatenated text from all TextContent parts. Useful
// when a caller just wants the model's user-facing answer and not the
// tool-call gymnastics.
func (m Message) Text() string {
	var out string
	for _, p := range m.Parts {
		if t, ok := p.(TextContent); ok {
			out += t.Text
		}
	}
	return out
}

// ToolCalls returns just the ToolCall parts, in order. The runner
// loops on these to dispatch tools.
func (m Message) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, p := range m.Parts {
		if tc, ok := p.(ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}
