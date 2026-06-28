package agent

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

func TestBudgetSuffix_Format(t *testing.T) {
	got := budgetSuffix(38, 50, 1, 37)
	want := "\n\n[turn 38/50 · edits: 1 · reads: 37]"
	if got != want {
		t.Errorf("budgetSuffix: got %q, want %q", got, want)
	}
}

func TestAppendBudgetSuffix_OnlyMutatesToolResults(t *testing.T) {
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "a", Name: "view", Content: "file contents"},
		message.TextContent{Text: "narration"},
		message.ToolResult{ToolCallID: "b", Name: "kai_grep", Content: "hits"},
	}
	suffix := "\n\n[turn 2/50 · edits: 0 · reads: 2]"
	appendBudgetSuffix(parts, suffix)

	tr0 := parts[0].(message.ToolResult)
	if !strings.HasSuffix(tr0.Content, suffix) {
		t.Errorf("first ToolResult missing suffix: %q", tr0.Content)
	}
	if tx, ok := parts[1].(message.TextContent); !ok || tx.Text != "narration" {
		t.Errorf("TextContent should be untouched, got %v", parts[1])
	}
	tr2 := parts[2].(message.ToolResult)
	if !strings.HasSuffix(tr2.Content, suffix) {
		t.Errorf("second ToolResult missing suffix: %q", tr2.Content)
	}
}
