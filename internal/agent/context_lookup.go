package agent

import (
	"context"
	"fmt"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/session"
	"kai/internal/agent/tools"
)

// Graph-powered context injection — runner-side wiring. When
// Options.InjectedContext is non-empty, the runner inserts a
// synthetic context_lookup tool_use + tool_result pair into the
// transcript immediately after the user prompt. The model sees a
// "lookup already happened" affordance: read the pre-resolved
// entry-point chain instead of guessing at where to search first.
//
// Why a synthetic tool result and not just a user message:
//   - Self-attribution: prose injected as the model's own claim
//     ("I looked up...") can trigger downstream confabulation.
//     Tool calls are structured signals the model parses as
//     machine-generated.
//   - Caching: the tool_use/result pair caches per-request without
//     inflating the system-prompt cache key (which would change
//     every request and break shared caching across turns).
//   - Provider compatibility: Anthropic and OpenAI both accept
//     tool_use + tool_result without the model having authored
//     them, as long as the tool is in the registered tool list.
//
// The context_lookup tool itself is a no-op (see contextLookupTool).
// Listed so the provider's schema check passes; the model can
// technically invoke it but the description tells it not to and the
// returned response is uninformative.

const (
	// contextLookupToolName is the synthetic tool's registered
	// name. Kept stable so the tool_use ID generated below
	// references a tool the registry recognizes.
	contextLookupToolName = "context_lookup"

	// contextLookupCallID is the tool_use_id pinned on the
	// injected call. Constant because there's only ever one
	// per-run; the runner doesn't loop on this.
	contextLookupCallID = "ctx_lookup_1"
)

// injectContextLookup appends a synthetic assistant turn with a
// context_lookup tool_use, then a user turn with the matching
// tool_result containing the pre-resolved context body. Mutates the
// supplied history slice via the pointer. Persists both turns when a
// session is non-nil so a resume sees the same injection.
func injectContextLookup(body string, history *[]message.Message, sess *session.Session) {
	assistant := message.Message{
		Role: message.RoleAssistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       contextLookupCallID,
				Name:     contextLookupToolName,
				Input:    "{}",
				Type:     "tool_use",
				Finished: true,
			},
		},
	}
	user := message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: contextLookupCallID,
				Name:       contextLookupToolName,
				Content:    body,
			},
		},
	}
	*history = append(*history, assistant, user)

	if sess != nil {
		// Best-effort persistence. A failure here would leave the
		// in-memory history correct but the session row missing
		// the synthetic pair on resume — not catastrophic; the
		// caller's planner re-injects on the next run.
		_ = sess.AppendMessage(assistant, 0, 0)
		_ = sess.AppendMessage(user, 0, 0)
	}
}

// contextLookupTool is the registry entry for the synthetic
// context_lookup tool. The Run method returns a polite "don't call
// me" string so a confused model that does invoke it gets clear
// feedback rather than a tool-not-found error.
type contextLookupTool struct{}

func (contextLookupTool) Info() tools.ToolInfo {
	return tools.ToolInfo{
		Name: contextLookupToolName,
		Description: "Pre-resolved entry points injected by the harness. " +
			"Do NOT call this tool — its result appears automatically when the " +
			"harness detects code-shaped tokens in the user's message. Calling it " +
			"manually returns nothing useful; search via kai_grep / kai_context / " +
			"view instead.",
		// Parameters is a flat property map (see other tools' Info()
		// methods). Wrapping it in {"type":"object","properties":{}}
		// produces a malformed schema that Anthropic rejects with a
		// silent 400 — the runner then logs in=0 out=0 and the chat
		// dispatcher fails with "assistant returned no text" (which
		// is exactly what happened on the first dogfood of v0.22.0).
		Parameters: map[string]any{},
	}
}

func (contextLookupTool) Run(_ context.Context, _ tools.ToolCall) (tools.ToolResponse, error) {
	return tools.ToolResponse{
		Content: fmt.Sprintf("%s is a harness-only tool. Use kai_grep, kai_context, view, or other search tools to look things up.",
			contextLookupToolName),
	}, nil
}
