package session

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kaicontext/kai-engine/message"
)

// dbAdapter wraps *sql.DB to satisfy the Store interface (which is
// defined to match graph.DB's exposed methods). Tests use this rather
// than spinning up a graph.DB to avoid the surrounding kai schema
// migration churn — only the session tables are relevant here.
type dbAdapter struct{ *sql.DB }

func openTestDB(t *testing.T) Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := &dbAdapter{db}
	if err := EnsureSchema(store); err != nil {
		t.Fatalf("ensuring schema: %v", err)
	}
	return store
}

func TestEnsureSchema_Idempotent(t *testing.T) {
	store := openTestDB(t)
	// Calling again must not error or duplicate tables.
	if err := EnsureSchema(store); err != nil {
		t.Errorf("second EnsureSchema call: %v", err)
	}
	// Confirm the agent_sessions table exists by inserting + reading.
	if _, err := New(store, "task", "/ws", "claude-sonnet-4-6"); err != nil {
		t.Errorf("insert after second schema call failed: %v", err)
	}
}

func TestNew_PersistsSession(t *testing.T) {
	store := openTestDB(t)
	s, err := New(store, "add-rate-limit", "/tmp/spawn-1", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.ID == "" {
		t.Error("expected non-empty session id")
	}
	if s.Status != StatusActive {
		t.Errorf("status: %s", s.Status)
	}
	if s.TaskName != "add-rate-limit" || s.Workspace != "/tmp/spawn-1" {
		t.Errorf("fields: %+v", s)
	}

	// Resume by id should read back the same row.
	got, err := Resume(store, s.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.TaskName != s.TaskName || got.Workspace != s.Workspace {
		t.Errorf("resumed mismatch: %+v vs %+v", got, s)
	}
}

func TestResume_MissingReturnsErrNotFound(t *testing.T) {
	store := openTestDB(t)
	_, err := Resume(store, "no-such-id")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestAppendAndHistory_RoundTrip(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "task", "/ws", "claude")

	turns := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{ID: "c1", Name: "view", Input: `{"file_path":"x.go"}`, Type: "tool_use", Finished: true},
		}, Finished: message.FinishReasonToolUse},
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "c1", Name: "view", Content: "alpha\nbeta\n"},
		}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.TextContent{Text: "found two lines."},
		}, Finished: message.FinishReasonEndTurn},
	}
	for i, m := range turns {
		// Just put some token counts on assistant turns to test the
		// total-tokens aggregator.
		var tIn, tOut int
		if m.Role == message.RoleAssistant {
			tIn = 100 * (i + 1)
			tOut = 50 * (i + 1)
		}
		if err := s.AppendMessage(m, tIn, tOut); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	got, err := s.History()
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != len(turns) {
		t.Fatalf("history length: got %d want %d", len(got), len(turns))
	}
	for i := range turns {
		if got[i].Role != turns[i].Role {
			t.Errorf("turn %d role: got %s want %s", i, got[i].Role, turns[i].Role)
		}
		if len(got[i].Parts) != len(turns[i].Parts) {
			t.Errorf("turn %d parts len: got %d want %d", i, len(got[i].Parts), len(turns[i].Parts))
		}
	}

	// Verify type-discriminated parts round-tripped correctly.
	if tc, ok := got[1].Parts[1].(message.ToolCall); !ok {
		t.Errorf("turn 1 part 1: expected ToolCall, got %T", got[1].Parts[1])
	} else if tc.Name != "view" || tc.ID != "c1" {
		t.Errorf("ToolCall round-trip: %+v", tc)
	}
	if tr, ok := got[2].Parts[0].(message.ToolResult); !ok {
		t.Errorf("turn 2 part 0: expected ToolResult, got %T", got[2].Parts[0])
	} else if tr.Content != "alpha\nbeta\n" {
		t.Errorf("ToolResult content: %q", tr.Content)
	}

	// Total token counts must aggregate.
	var totalIn, totalOut int
	if err := store.QueryRow(
		`SELECT total_tokens_in, total_tokens_out FROM agent_sessions WHERE id = ?`, s.ID,
	).Scan(&totalIn, &totalOut); err != nil {
		t.Fatalf("token totals: %v", err)
	}
	if totalIn == 0 || totalOut == 0 {
		t.Errorf("expected non-zero totals, got in=%d out=%d", totalIn, totalOut)
	}
}

func TestEnd_MarksTerminalStatus(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "task", "/ws", "claude")
	if err := s.End(StatusEnded); err != nil {
		t.Fatalf("End: %v", err)
	}
	got, err := Resume(store, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusEnded {
		t.Errorf("status: %s", got.Status)
	}

	// Calling End again is idempotent (no error).
	if err := s.End(StatusEnded); err != nil {
		t.Errorf("second End call: %v", err)
	}
}

// TestAppendMessage_OrdinalsContiguous: claims-the-next-ordinal logic
// must produce 0,1,2,... without gaps. Ordinal is what makes History
// stable-ordered, so a gap or duplicate would silently corrupt the
// transcript.
func TestAppendMessage_OrdinalsContiguous(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "t", "/ws", "claude")
	for i := 0; i < 5; i++ {
		_ = s.AppendMessage(message.Message{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: "x"}},
		}, 0, 0)
	}
	rows, err := store.Query(
		`SELECT ordinal FROM agent_messages WHERE session_id = ? ORDER BY ordinal`, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		_ = rows.Scan(&n)
		got = append(got, n)
	}
	want := []int{0, 1, 2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ordinals: got %v want %v", got, want)
	}
}

func TestEncodeDecodeParts_AllVariants(t *testing.T) {
	parts := []message.ContentPart{
		message.TextContent{Text: "a"},
		message.ReasoningContent{Thinking: "b"},
		message.ToolCall{ID: "1", Name: "view", Input: `{"f":1}`, Type: "tool_use"},
		message.ToolResult{ToolCallID: "1", Name: "view", Content: "ok"},
	}
	body, err := encodeParts(parts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeParts(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(parts) {
		t.Fatalf("len: got %d want %d", len(got), len(parts))
	}
	if _, ok := got[0].(message.TextContent); !ok {
		t.Error("part 0 not TextContent")
	}
	if _, ok := got[1].(message.ReasoningContent); !ok {
		t.Error("part 1 not ReasoningContent")
	}
	if _, ok := got[2].(message.ToolCall); !ok {
		t.Error("part 2 not ToolCall")
	}
	if _, ok := got[3].(message.ToolResult); !ok {
		t.Error("part 3 not ToolResult")
	}
}

func TestSaveAndLookupMode_RoundTrip(t *testing.T) {
	store := openTestDB(t)
	s, err := New(store, "chat", "/ws", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Fresh session has no recorded mode.
	got, err := LookupMode(store, s.ID)
	if err != nil {
		t.Fatalf("LookupMode fresh: %v", err)
	}
	if got != "" {
		t.Errorf("fresh session prev_mode = %q, want empty", got)
	}
	if err := SaveMode(store, s.ID, "debug"); err != nil {
		t.Fatalf("SaveMode: %v", err)
	}
	got, err = LookupMode(store, s.ID)
	if err != nil {
		t.Fatalf("LookupMode after save: %v", err)
	}
	if got != "debug" {
		t.Errorf("LookupMode = %q, want debug", got)
	}
	// Overwrite with a new mode.
	if err := SaveMode(store, s.ID, "planning"); err != nil {
		t.Fatalf("SaveMode overwrite: %v", err)
	}
	got, _ = LookupMode(store, s.ID)
	if got != "planning" {
		t.Errorf("after overwrite = %q, want planning", got)
	}
}

func TestLookupMode_MissingAndEmpty(t *testing.T) {
	store := openTestDB(t)
	got, err := LookupMode(store, "no-such-id")
	if err != nil {
		t.Fatalf("missing id error: %v", err)
	}
	if got != "" {
		t.Errorf("missing id = %q, want empty", got)
	}
	got, err = LookupMode(store, "")
	if err != nil || got != "" {
		t.Errorf("empty id = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestSaveMode_EmptyIDIsNoop(t *testing.T) {
	store := openTestDB(t)
	if err := SaveMode(store, "", "coding"); err != nil {
		t.Errorf("SaveMode with empty id should be no-op, got %v", err)
	}
}

func TestFindRecent_HappyPath(t *testing.T) {
	store := openTestDB(t)
	s, err := New(store, "task", "/ws/a", "model")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.AppendMessage(message.Message{
		Role:  message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	}, 1, 0); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	got, err := FindRecent(store, "/ws/a", time.Hour)
	if err != nil {
		t.Fatalf("FindRecent: %v", err)
	}
	if got == nil {
		t.Fatal("expected a recent session, got nil")
	}
	if got.ID != s.ID {
		t.Errorf("ID = %s, want %s", got.ID, s.ID)
	}
	if got.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", got.MessageCount)
	}
	if got.LastMessageAge < 0 || got.LastMessageAge > 5*time.Second {
		t.Errorf("LastMessageAge = %s (should be ~0s)", got.LastMessageAge)
	}
}

func TestFindRecent_SkipsEmptySession(t *testing.T) {
	store := openTestDB(t)
	if _, err := New(store, "task", "/ws/a", "model"); err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := FindRecent(store, "/ws/a", time.Hour)
	if err != nil {
		t.Fatalf("FindRecent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for session with zero messages, got %+v", got)
	}
}

func TestFindRecent_FiltersByWorkspace(t *testing.T) {
	store := openTestDB(t)
	s, err := New(store, "task", "/ws/a", "model")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s.AppendMessage(message.Message{
		Role:  message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	}, 0, 0)
	// Different workspace — should miss.
	got, err := FindRecent(store, "/ws/b", time.Hour)
	if err != nil {
		t.Fatalf("FindRecent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for different workspace, got %+v", got)
	}
}

func TestTruncateAfterLastUser_RemovesAssistantAndTools(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "task", "/ws", "claude")

	msgs := []message.Message{
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "first ask"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "first reply"}}, Finished: message.FinishReasonEndTurn},
		{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "second ask"}}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{
			message.TextContent{Text: "bad reply"},
			message.ToolCall{ID: "c1", Name: "view", Input: `{}`, Type: "tool_use", Finished: true},
		}, Finished: message.FinishReasonToolUse},
		{Role: message.RoleUser, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "c1", Name: "view", Content: "x"},
		}},
		{Role: message.RoleAssistant, Parts: []message.ContentPart{message.TextContent{Text: "more bad"}}, Finished: message.FinishReasonEndTurn},
	}
	for _, m := range msgs {
		if err := s.AppendMessage(m, 0, 0); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	n, err := s.TruncateAfterLastUser()
	if err != nil {
		t.Fatalf("TruncateAfterLastUser: %v", err)
	}
	// Expected: the trailing assistant ("more bad") is deleted. The
	// tool-result user message is the last user turn, so everything
	// up to and including it stays.
	if n != 1 {
		t.Errorf("deleted count: got %d want 1", n)
	}
	hist, err := s.History()
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 5 {
		t.Fatalf("history len: got %d want 5", len(hist))
	}
	if hist[len(hist)-1].Role != message.RoleUser {
		t.Errorf("last surviving role: got %s want user", hist[len(hist)-1].Role)
	}
}

func TestTruncateAfterLastUser_NoUserMessageIsNoOp(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "task", "/ws", "claude")
	// Only an assistant message — refusing to truncate is the safer
	// default than wiping the session entirely.
	_ = s.AppendMessage(message.Message{
		Role:     message.RoleAssistant,
		Parts:    []message.ContentPart{message.TextContent{Text: "hi"}},
		Finished: message.FinishReasonEndTurn,
	}, 0, 0)
	n, err := s.TruncateAfterLastUser()
	if err != nil {
		t.Fatalf("TruncateAfterLastUser: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 deletions on no-user session, got %d", n)
	}
	hist, _ := s.History()
	if len(hist) != 1 {
		t.Errorf("history wiped: got len %d want 1", len(hist))
	}
}

func TestTruncateAfterLastUser_NothingToTrim(t *testing.T) {
	store := openTestDB(t)
	s, _ := New(store, "task", "/ws", "claude")
	_ = s.AppendMessage(message.Message{
		Role:  message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "only"}},
	}, 0, 0)
	n, err := s.TruncateAfterLastUser()
	if err != nil {
		t.Fatalf("TruncateAfterLastUser: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 deletions when nothing after user, got %d", n)
	}
}

func TestFindRecent_SkipsEndedSessions(t *testing.T) {
	store := openTestDB(t)
	s, err := New(store, "task", "/ws/a", "model")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s.AppendMessage(message.Message{
		Role:  message.RoleUser,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	}, 0, 0)
	if err := s.End(StatusEnded); err != nil {
		t.Fatalf("End: %v", err)
	}
	got, err := FindRecent(store, "/ws/a", time.Hour)
	if err != nil {
		t.Fatalf("FindRecent: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for ended session, got %+v", got)
	}
}
