package views

import (
	"strings"
	"testing"

	"kai/api/message"
	"kai/api/session"
	"kai/internal/orchestrator"
)

// TestRecentTurnsForTriage_PicksLastUserAssistantOnly verifies the
// helper drops system messages and tool-result-only turns, returning
// just the conversational thread in chronological order. This is the
// signal triage needs to resolve continuation requests like "fix it
// in the sidebar" — without it, triage was bouncing those as
// TrackClarify (the 2026-05-26 dogfood symptom).
func TestRecentTurnsForTriage_PicksLastUserAssistantOnly(t *testing.T) {
	store := openTestSessionStore(t)
	sess, err := session.New(store, "planner", "/tmp/work", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	// Build a small mixed history: system + user + assistant +
	// system (e.g. PRIOR PLAN FAILED) + user + assistant.
	for _, m := range []message.Message{
		{Role: message.RoleSystem, Parts: []message.ContentPart{message.TextContent{Text: "system noise"}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "show current directory in TitleBar"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "Done — TitleBar.svelte updated to display cwd basename."}}},
		{Role: message.RoleSystem, Parts: []message.ContentPart{message.TextContent{Text: "more system noise"}}},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "also fix it in the sidebar"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "Will mirror the TitleBar change in Sidebar.svelte."}}},
	} {
		if err := sess.AppendMessage(m, 0, 0); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	s := &PlannerServices{
		OrchestratorCfg: orchestrator.Config{AgentSessionStore: store},
	}
	got := recentTurnsForTriage(s, sess.ID)
	if len(got) != 4 {
		t.Fatalf("want 4 user/assistant turns, got %d:\n%v", len(got), got)
	}
	// Chronological order — oldest first.
	expectedPrefixes := []string{
		"user: show current directory",
		"assistant: Done — TitleBar",
		"user: also fix it in the sidebar",
		"assistant: Will mirror the TitleBar",
	}
	for i, want := range expectedPrefixes {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("turn[%d] = %q, want prefix %q", i, got[i], want)
		}
	}
}

// TestRecentTurnsForTriage_NilSafe — missing session, empty id, or
// nil store all return nil so triage falls back to its prior
// stateless behavior cleanly.
func TestRecentTurnsForTriage_NilSafe(t *testing.T) {
	if got := recentTurnsForTriage(nil, "id"); got != nil {
		t.Errorf("nil services should return nil, got %v", got)
	}
	if got := recentTurnsForTriage(&PlannerServices{}, ""); got != nil {
		t.Errorf("empty sessionID should return nil, got %v", got)
	}
	s := &PlannerServices{
		OrchestratorCfg: orchestrator.Config{AgentSessionStore: nil},
	}
	if got := recentTurnsForTriage(s, "some-id"); got != nil {
		t.Errorf("nil store should return nil, got %v", got)
	}
}

// TestRecentTurnsForTriage_TruncatesLongText caps per-message text
// so a wall-of-tool-output assistant turn can't blow up the triage
// prompt budget.
func TestRecentTurnsForTriage_TruncatesLongText(t *testing.T) {
	store := openTestSessionStore(t)
	sess, err := session.New(store, "planner", "/tmp/work", "model")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	long := strings.Repeat("A", 2000)
	if err := sess.AppendMessage(message.Message{
		Role:  message.RoleAssistant,
		Parts: []message.ContentPart{message.TextContent{Text: long}},
	}, 0, 0); err != nil {
		t.Fatalf("append: %v", err)
	}
	s := &PlannerServices{
		OrchestratorCfg: orchestrator.Config{AgentSessionStore: store},
	}
	got := recentTurnsForTriage(s, sess.ID)
	if len(got) != 1 {
		t.Fatalf("want 1 turn, got %d", len(got))
	}
	if len(got[0]) > 800 { // ~600 chars text + "assistant: " prefix + ellipsis
		t.Errorf("truncation failed; got %d chars", len(got[0]))
	}
	if !strings.HasSuffix(got[0], "…") {
		t.Errorf("expected ellipsis suffix on truncated text, got: ...%q", got[0][len(got[0])-30:])
	}
}
