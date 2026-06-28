package session

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// TestUserVisibleHistory_DropsSystemAndSummarizesToolClusters pins
// the architectural shape promised by 2026-05-26 spec #3: a chat
// agent resuming a planner-task session sees user prompts +
// assistant prose + a one-line summary per tool-call cluster, not
// the raw planning-JSON / tool-result transcript.
func TestUserVisibleHistory_DropsSystemAndSummarizesToolClusters(t *testing.T) {
	store := openTestDB(t)
	sess, err := New(store, "planner", "/tmp/work", "m")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Mixed session: user prompt, planner system message, assistant
	// tool calls (view + grep), tool results, assistant prose.
	for _, m := range []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "fix the title bar to show cwd"}}},
		{Role: message.RoleSystem, Parts: []message.ContentPart{message.TextContent{Text: "PRIOR PLAN FAILED…"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "view", Input: "{}"},
			message.ToolCall{ID: "2", Name: "kai_grep", Input: "{}"},
		}},
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "1", Content: "file contents…"},
			message.ToolResult{ToolCallID: "2", Content: "grep hits…"},
		}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "Done — TitleBar updated."}}},
	} {
		if err := sess.AppendMessage(m, 0, 0); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := sess.UserVisibleHistory()
	if err != nil {
		t.Fatalf("UserVisibleHistory: %v", err)
	}
	// Expect: user prompt + 1 synthesized cluster line + final assistant prose.
	if len(got) != 3 {
		t.Fatalf("want 3 messages, got %d:\n%v", len(got), dumpMessages(got))
	}
	if got[0].Role != message.RoleUser || !strings.Contains(got[0].Text(), "fix the title bar") {
		t.Errorf("msg[0] should be user prompt; got %v %q", got[0].Role, got[0].Text())
	}
	if got[1].Role != message.RoleAssistant || !strings.Contains(got[1].Text(), "[tools:") {
		t.Errorf("msg[1] should be synthesized tool cluster line; got %v %q", got[1].Role, got[1].Text())
	}
	if !strings.Contains(got[1].Text(), "view") || !strings.Contains(got[1].Text(), "kai_grep") {
		t.Errorf("cluster line should name both tools, got: %q", got[1].Text())
	}
	if got[2].Role != message.RoleAssistant || !strings.Contains(got[2].Text(), "TitleBar updated") {
		t.Errorf("msg[2] should be final assistant prose; got %v %q", got[2].Role, got[2].Text())
	}
}

// TestUserVisibleHistory_BucketsRepeatedTools verifies the "view × 3"
// counting form — relevant for planner runs that read many files in
// one cluster.
func TestUserVisibleHistory_BucketsRepeatedTools(t *testing.T) {
	store := openTestDB(t)
	sess, err := New(store, "planner", "/tmp/work", "m")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := sess.AppendMessage(message.Message{
		Role: message.RoleAssistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "view"},
			message.ToolCall{ID: "2", Name: "view"},
			message.ToolCall{ID: "3", Name: "view"},
			message.ToolCall{ID: "4", Name: "kai_grep"},
		},
	}, 0, 0); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := sess.AppendMessage(message.Message{
		Role: message.RoleUser,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "1", Content: "..."},
		},
	}, 0, 0); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := sess.UserVisibleHistory()
	if err != nil {
		t.Fatalf("UserVisibleHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 cluster line, got %d", len(got))
	}
	text := got[0].Text()
	if !strings.Contains(text, "view × 3") {
		t.Errorf("expected 'view × 3' bucketing, got: %q", text)
	}
	if !strings.Contains(text, "kai_grep") {
		t.Errorf("expected kai_grep in line, got: %q", text)
	}
}

// TestUserVisibleHistory_PreservesAssistantPureProse — a chat-only
// session shouldn't be transformed; conversational turns flow through
// untouched.
func TestUserVisibleHistory_PreservesAssistantPureProse(t *testing.T) {
	store := openTestDB(t)
	sess, err := New(store, "chat", "/tmp/work", "m")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, m := range []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "what does kai do?"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "kai is a semantic-graph code tool."}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "thanks"}}},
	} {
		if err := sess.AppendMessage(m, 0, 0); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, err := sess.UserVisibleHistory()
	if err != nil {
		t.Fatalf("UserVisibleHistory: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 messages, got %d", len(got))
	}
	if got[1].Text() != "kai is a semantic-graph code tool." {
		t.Errorf("assistant prose should pass through verbatim, got: %q", got[1].Text())
	}
}

// TestUserVisibleHistory_EmptySession returns nil/empty cleanly.
func TestUserVisibleHistory_EmptySession(t *testing.T) {
	store := openTestDB(t)
	sess, err := New(store, "chat", "/tmp/work", "m")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := sess.UserVisibleHistory()
	if err != nil {
		t.Fatalf("UserVisibleHistory: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty session should return empty slice, got %d messages", len(got))
	}
}

func dumpMessages(ms []message.Message) string {
	var b strings.Builder
	for i, m := range ms {
		b.WriteString("  [")
		b.WriteString(string(rune('0' + i)))
		b.WriteString("] ")
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(m.Text())
		b.WriteString("\n")
	}
	return b.String()
}
