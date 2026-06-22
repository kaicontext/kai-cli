package views

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"kai/api/session"
	errpkg "kai/internal/tui/errors"
)

// (dbAdapter is declared in repl_dispatch_test.go in this package.)

func openTestSessionStore(t *testing.T) session.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := &dbAdapter{db}
	if err := session.EnsureSchema(store); err != nil {
		t.Fatalf("ensuring schema: %v", err)
	}
	return store
}

// TestRecordExecuteFailureForPlanner_AppendsSystemMessage pins
// the May-2026 fix: when a "go" plan fails, a system message
// with the classified failure must land in the planner's
// session so the next planner Run picks it up. Without this,
// the user reported seeing "the same plan three times" — the
// planner had no signal that the previous one didn't work.
func TestRecordExecuteFailureForPlanner_AppendsSystemMessage(t *testing.T) {
	store := openTestSessionStore(t)
	sess, err := session.New(store, "planner", "/tmp/work", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}

	ue := errpkg.UserError{
		Kind:     "planner.too_vague",
		Headline: "Request too vague to plan",
		Action:   "Add more detail and try again.",
		Severity: errpkg.Block,
	}

	recordExecuteFailureForPlanner(store, sess.ID, ue)

	resumed, err := session.Resume(store, sess.ID)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	hist, err := resumed.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("expected 1 message, got %d", len(hist))
	}
	got := hist[0].Text()
	for _, want := range []string{
		"PRIOR PLAN EXECUTION FAILED",
		"planner.too_vague",
		"Request too vague",
		"do NOT re-propose the same agents verbatim",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system message missing %q\nfull text:\n%s", want, got)
		}
	}
}

// TestRecordExecuteFailureForPlanner_SkipsAutoRepairableKinds pins
// the 2026-05-25 dogfood fix: auto-repairable infrastructure
// failures (missing_blobs, no_snapshots) must NOT be recorded as
// "PRIOR PLAN EXECUTION FAILED". The workspace recovers in the
// background; telling the planner its plan failed makes it
// discard prior reasoning on the next turn and restart
// investigation, which manifested as the planner re-deriving the
// same svelte fix for 6+ minutes after every snapshot reindex.
func TestRecordExecuteFailureForPlanner_SkipsAutoRepairableKinds(t *testing.T) {
	store := openTestSessionStore(t)
	sess, err := session.New(store, "planner", "/tmp/work", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}

	for _, kind := range []string{"preflight.missing_blobs", "preflight.no_snapshots"} {
		recordExecuteFailureForPlanner(store, sess.ID, errpkg.UserError{
			Kind:     kind,
			Headline: "should not poison the planner session",
			Severity: errpkg.Block,
		})
	}

	resumed, err := session.Resume(store, sess.ID)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	hist, err := resumed.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("auto-repairable kinds should not append messages, got %d:\n%s",
			len(hist), hist[0].Text())
	}
}

// TestRecordExecuteFailureForPlanner_NilSafe: missing store or
// empty session id must no-op silently — these inputs are
// realistic during very early TUI bootstrap and we never want
// the planner-memory hook to be the thing that crashes the REPL.
func TestRecordExecuteFailureForPlanner_NilSafe(t *testing.T) {
	ue := errpkg.UserError{Kind: "x"}
	// Should not panic.
	recordExecuteFailureForPlanner(nil, "session-id", ue)
	store := openTestSessionStore(t)
	recordExecuteFailureForPlanner(store, "", ue)
}
