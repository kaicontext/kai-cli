// Package tools defines the Tool interface that the kai agent loop
// dispatches to. Derived from OpenCode's `internal/llm/tools/tools.go`
// (MIT-licensed; see ../NOTICE.md and ../LICENSE-OpenCode.md).
//
// Differences from upstream:
//   - Package name changed from `tools` (under OpenCode) to `tools`
//     under `kai/internal/agent/`.
//   - The OpenCode session/message context keys (SessionIDContextKey,
//     MessageIDContextKey) are kept; kai's runner uses them the same
//     way once session persistence lands (Slice 5). Until then they
//     are populated with empty strings and tools must tolerate that.
package tools

import (
	"context"
	"encoding/json"
)

// ToolInfo describes a tool to the model. Parameters and Required are
// JSON-Schema-shaped so the LLM can reason about call shape without
// extra wrapping.
type ToolInfo struct {
	Name        string
	Description string
	Parameters  map[string]any
	Required    []string
}

type toolResponseType string

type (
	sessionIDContextKey string
	messageIDContextKey string
)

const (
	ToolResponseTypeText  toolResponseType = "text"
	ToolResponseTypeImage toolResponseType = "image"

	// SessionIDContextKey / MessageIDContextKey carry conversation
	// identifiers through tool calls. Empty values are valid until
	// Slice 5 wires session persistence.
	SessionIDContextKey sessionIDContextKey = "session_id"
	MessageIDContextKey messageIDContextKey = "message_id"
)

// ToolResponse is what a tool returns to the agent loop. Metadata is
// opaque JSON the runner forwards to the model alongside Content.
type ToolResponse struct {
	Type     toolResponseType `json:"type"`
	Content  string           `json:"content"`
	Metadata string           `json:"metadata,omitempty"`
	IsError  bool             `json:"is_error"`
}

// NewTextResponse builds a successful text response.
func NewTextResponse(content string) ToolResponse {
	return ToolResponse{
		Type:    ToolResponseTypeText,
		Content: content,
	}
}

// WithResponseMetadata attaches structured metadata as a JSON string
// alongside the text content. Marshaling failure leaves the response
// unchanged — better to return the content than to swallow it.
func WithResponseMetadata(response ToolResponse, metadata any) ToolResponse {
	if metadata != nil {
		metadataBytes, err := json.Marshal(metadata)
		if err != nil {
			return response
		}
		response.Metadata = string(metadataBytes)
	}
	return response
}

// NewTextErrorResponse marks the response as an error so the agent
// loop can react (typically: re-prompt the model with the error).
func NewTextErrorResponse(content string) ToolResponse {
	return ToolResponse{
		Type:    ToolResponseTypeText,
		Content: content,
		IsError: true,
	}
}

// ToolCall is the model's request to invoke a tool. Input is a JSON
// string the model produced to match the tool's parameter schema.
type ToolCall struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"`
}

// BaseTool is the minimum contract every kai agent tool implements.
// Run executes the tool with a parsed input payload and returns the
// model-facing response.
type BaseTool interface {
	Info() ToolInfo
	Run(ctx context.Context, params ToolCall) (ToolResponse, error)
}

// GetContextValues extracts session + message IDs from the context.
// Returns empty strings if the keys are missing — callers must treat
// empty as "no session yet", not as an error condition.
func GetContextValues(ctx context.Context) (sessionID, messageID string) {
	s := ctx.Value(SessionIDContextKey)
	m := ctx.Value(MessageIDContextKey)
	if s == nil {
		return "", ""
	}
	if m == nil {
		return s.(string), ""
	}
	return s.(string), m.(string)
}
