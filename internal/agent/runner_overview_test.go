package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
	"github.com/kaicontext/kai-engine/session"
)

// findOverview scans every TextContent across the request's
// messages for the project-overview marker phrase that
// graph_context.go's buildOverview() prepends. Returns
// (found, message-index, full-text-blob) so failing tests show
// what was actually sent.
//
// We assert on the marker phrase rather than specific tree
// content because the overview format may evolve (manifest
// digest, README excerpt, ...); the marker is the contract.
func findOverview(req provider.Request) (bool, int, string) {
	for i, m := range req.Messages {
		for _, p := range m.Parts {
			if t, ok := p.(message.TextContent); ok {
				if strings.Contains(t.Text, "Project overview (auto-injected by kai") {
					return true, i, t.Text
				}
			}
		}
	}
	return false, -1, ""
}

// writeFakeWorkspace gives BuildOverview enough material to
// produce a non-empty block: top-level dirs, a manifest-ish
// file (go.mod), and a README. Without these the overview
// returns "" and the test would pass for the wrong reason.
func writeFakeWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "kai-cli", "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "github.com/kaicontext/kai-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"README.md":         "# kai\nSemantic graph for code change.\n",
		"go.mod":            "module kai\n\ngo 1.22\n",
		"kai-cli/main.go":   "package main",
		"kai-cli/cmd/k.go":  "package cmd",
		"github.com/kaicontext/kai-core/types.go": "package core",
	} {
		full := filepath.Join(ws, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// TestFirstTurnOverview_FreshSession is the canonical case: brand-
// new chat, vague prompt ("yo"), no history. graphCtx must
// inject a Project overview block in the FIRST request so the
// model has something to ground itself in instead of inventing
// filenames (the May-3 "package.json on a Go project" failure).
func TestFirstTurnOverview_FreshSession(t *testing.T) {
	ws := writeFakeWorkspace(t)

	p := &fakeProvider{queue: []provider.Response{{
		Parts:        []message.ContentPart{message.TextContent{Text: "ok"}},
		FinishReason: message.FinishReasonEndTurn,
	}}}

	if _, err := Run(context.Background(), Options{
		Workspace: ws,
		Prompt:    "yo",
		Model:     "claude-sonnet-4-6",
		Provider:  p,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found, idx, blob := findOverview(p.last)
	if !found {
		t.Fatalf("expected first-turn overview in request, none found.\nMessages: %+v", p.last.Messages)
	}
	// Must land on the LAST USER message — not in system, not
	// some prior turn. Cache-friendly placement; also matches
	// the documented design (graphCtx blocks ride per-turn user
	// message tail via withPerTurnHints).
	if idx != len(p.last.Messages)-1 {
		t.Errorf("overview should land on last user message (idx %d), got idx %d",
			len(p.last.Messages)-1, idx)
	}
	if p.last.Messages[idx].Role != message.RoleUser {
		t.Errorf("overview must be on user message, got role %v", p.last.Messages[idx].Role)
	}
	// Must mention real entries from our fake workspace — proves
	// graphCtx ran against the right directory.
	if !strings.Contains(blob, "kai-cli") {
		t.Errorf("overview missing real workspace entries (kai-cli/): blob=%q", blob)
	}
}

// TestFirstTurnOverview_PlannerThenChatHandoff is the May-4 bug
// the runner-side fix targets: planner runs first on a fresh
// session, declares the prompt vague WITHOUT calling tools,
// then chat agent resumes the SAME session. Chat sees history
// > 1 (planner's text turns) but zero tool results — graphCtx
// must STILL fire so chat agent isn't blind.
//
// Before the fix, isFirstTurn returned false on any assistant
// turn, so chat agent on turn 2 would fall back to dir-name
// pattern matching ("Looks like the kai repo itself") with no
// actual file knowledge — exactly the hallucination we're
// trying to eliminate.
func TestFirstTurnOverview_PlannerThenChatHandoff(t *testing.T) {
	ws := writeFakeWorkspace(t)
	store := newTestSessionStore(t)

	// First invocation: 'planner' returns prose, no tools.
	plannerProv := &fakeProvider{queue: []provider.Response{{
		Parts:        []message.ContentPart{message.TextContent{Text: "Too vague — need more info"}},
		FinishReason: message.FinishReasonEndTurn,
	}}}
	plannerRes, err := Run(context.Background(), Options{
		Workspace:    ws,
		Prompt:       "yo",
		Model:        "claude-sonnet-4-6",
		Provider:     plannerProv,
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("planner Run: %v", err)
	}
	sessionID := plannerRes.SessionID
	if sessionID == "" {
		t.Fatal("planner Run produced no SessionID")
	}

	// Second invocation: 'chat' resumes the same session.
	chatProv := &fakeProvider{queue: []provider.Response{{
		Parts:        []message.ContentPart{message.TextContent{Text: "ok"}},
		FinishReason: message.FinishReasonEndTurn,
	}}}
	if _, err := Run(context.Background(), Options{
		Workspace:    ws,
		Prompt:       "what's here?",
		Model:        "claude-sonnet-4-6",
		Provider:     chatProv,
		SessionStore: store,
		SessionID:    sessionID,
	}); err != nil {
		t.Fatalf("chat Run: %v", err)
	}

	// Chat agent's request must ALSO have the overview, because
	// no tool results exist in the (now-resumed) history. This
	// is the regression guard: if isFirstTurn ever reverts to
	// the "no assistant turns" heuristic, this test fails.
	found, _, blob := findOverview(chatProv.last)
	if !found {
		t.Fatalf("chat agent (resumed session, no tool results in history) "+
			"must still get the overview injection.\nMessages: %+v", chatProv.last.Messages)
	}
	if !strings.Contains(blob, "kai-cli") {
		t.Errorf("chat agent overview missing real workspace entries: %q", blob)
	}
}

// TestFirstTurnOverview_SuppressedAfterToolResults is the "don't
// be noisy" case: a focused multi-turn agent that's already
// explored the codebase has tool_results in history. Re-injecting
// the overview on every new prompt would burn tokens and add
// confusing duplicate context. Injection must be suppressed.
func TestFirstTurnOverview_SuppressedAfterToolResults(t *testing.T) {
	ws := writeFakeWorkspace(t)
	store := newTestSessionStore(t)

	// First turn: model uses a tool. The 'view' tool result
	// will land in history, marking this conversation as
	// "exploration already done."
	prov := &fakeProvider{queue: []provider.Response{
		{
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    "c1",
					Name:  "view",
					Input: `{"file_path":"README.md"}`,
					Type:  "tool_use",
				},
			},
			FinishReason: message.FinishReasonToolUse,
		},
		{
			Parts:        []message.ContentPart{message.TextContent{Text: "saw the readme"}},
			FinishReason: message.FinishReasonEndTurn,
		},
	}}
	res, err := Run(context.Background(), Options{
		Workspace:    ws,
		Prompt:       "look at the readme",
		Model:        "claude-sonnet-4-6",
		Provider:     prov,
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Second invocation: same session, resume. Tool result in
	// history → injection should NOT fire.
	prov2 := &fakeProvider{queue: []provider.Response{{
		Parts:        []message.ContentPart{message.TextContent{Text: "follow-up answer"}},
		FinishReason: message.FinishReasonEndTurn,
	}}}
	if _, err := Run(context.Background(), Options{
		Workspace:    ws,
		Prompt:       "tell me more",
		Model:        "claude-sonnet-4-6",
		Provider:     prov2,
		SessionStore: store,
		SessionID:    res.SessionID,
	}); err != nil {
		t.Fatalf("Run resume: %v", err)
	}

	if found, _, blob := findOverview(prov2.last); found {
		t.Errorf("overview should be suppressed when history has tool results, got: %q", blob)
	}
}

// newTestSessionStore opens an in-memory SQLite-backed session
// store. Mirrors the helper in runner_test.go (dbAdapter), kept
// minimal so the overview tests can resume sessions without
// re-implementing boilerplate.
func newTestSessionStore(t *testing.T) session.Store {
	t.Helper()
	db, err := openSessionTestDB()
	if err != nil {
		t.Fatalf("session db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := session.EnsureSchema(dbAdapter{db}); err != nil {
		t.Fatalf("session schema: %v", err)
	}
	return dbAdapter{db}
}
