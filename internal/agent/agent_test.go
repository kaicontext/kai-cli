package agent

import (
	"context"
	"testing"

	"kai/internal/agent/message"
	"kai/internal/agent/tools"
)

// TestMessageVocabulary is a sanity check that the message package
// types serialize and the marker interfaces are wired. These are the
// types every downstream slice will use; if the shape ever drifts
// silently, this catches it.
func TestMessageVocabulary(t *testing.T) {
	m := message.Message{
		Role: message.RoleAssistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
			message.ToolCall{ID: "1", Name: "view", Input: `{"file_path":"x.go"}`},
		},
		Finished: message.FinishReasonToolUse,
	}

	if m.Text() != "hello" {
		t.Errorf("Message.Text() = %q, want %q", m.Text(), "hello")
	}
	calls := m.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "view" {
		t.Errorf("Message.ToolCalls() = %+v, want one call to 'view'", calls)
	}
}

// TestToolsContextValues exercises the shim that pulls session/message
// IDs out of the context. Empty values must not panic — until Slice 5
// the runner won't populate them.
func TestToolsContextValues(t *testing.T) {
	s, m := tools.GetContextValues(context.Background())
	if s != "" || m != "" {
		t.Errorf("expected empty strings for unset context, got %q/%q", s, m)
	}
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "sess-1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	s, m = tools.GetContextValues(ctx)
	if s != "sess-1" || m != "msg-1" {
		t.Errorf("expected sess-1/msg-1, got %q/%q", s, m)
	}
}

// TestToolResponseBuilders covers the small text-response helpers.
// Trivial today; pinning them prevents accidental breakage when
// Slice 1 starts using them in earnest.
func TestToolResponseBuilders(t *testing.T) {
	good := tools.NewTextResponse("ok")
	if good.Content != "ok" || good.IsError {
		t.Errorf("NewTextResponse: %+v", good)
	}
	bad := tools.NewTextErrorResponse("boom")
	if bad.Content != "boom" || !bad.IsError {
		t.Errorf("NewTextErrorResponse: %+v", bad)
	}
	withMeta := tools.WithResponseMetadata(good, map[string]string{"k": "v"})
	if withMeta.Metadata == "" {
		t.Error("WithResponseMetadata should set metadata field")
	}
}
