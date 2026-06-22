package session

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
)

// busyStore wraps Store and returns SQLITE_BUSY for the first
// `busyTimes` Exec calls, then falls through to the real store.
// Simulates the race that happens when N parallel agents all call
// session.New on the same SQLite file at the same instant — first
// one wins the write lock, the rest get back SQLITE_BUSY until the
// lock clears.
type busyStore struct {
	Store
	mu        sync.Mutex
	busyTimes int
}

func (b *busyStore) Exec(query string, args ...any) (sql.Result, error) {
	b.mu.Lock()
	if b.busyTimes > 0 {
		b.busyTimes--
		b.mu.Unlock()
		return nil, errors.New("database is locked (5)")
	}
	b.mu.Unlock()
	return b.Store.Exec(query, args...)
}

// TestNew_RetriesOnSQLiteBusy is the regression for the
// 2026-05-15 dogfood: three agents spawned in parallel by the
// orchestrator raced for the agent_sessions write lock; one won,
// two died with "database is locked (5)" because session.New had
// no busy-retry. AppendMessage already had retry; New did not.
// This test simulates a single-attempt busy hit and verifies the
// retry catches it.
func TestNew_RetriesOnSQLiteBusy(t *testing.T) {
	inner := openTestDB(t)
	// First Exec gets BUSY, second succeeds. With retry the call
	// completes; without retry it fails at attempt 0.
	store := &busyStore{Store: inner, busyTimes: 1}

	s, err := New(store, "task", "/ws", "model")
	if err != nil {
		t.Fatalf("New should have retried past one busy hit, got: %v", err)
	}
	if s == nil || s.ID == "" {
		t.Fatal("expected a valid session after retry")
	}
}

// TestNew_GivesUpAfterMaxRetries confirms that a permanently-busy
// store DOES eventually surface the error — we don't want an
// infinite retry loop if something else has the DB locked for
// real (a long-running parent transaction, fs corruption, etc.).
func TestNew_GivesUpAfterMaxRetries(t *testing.T) {
	inner := openTestDB(t)
	store := &busyStore{Store: inner, busyTimes: maxAppendRetries + 5}

	_, err := New(store, "task", "/ws", "model")
	if err == nil {
		t.Fatal("expected New to surface SQLITE_BUSY after exhausting retries")
	}
	if !isSQLiteBusy(err) {
		t.Errorf("expected SQLITE_BUSY error wrapped, got: %v", err)
	}
}

// TestEnd_RetriesOnSQLiteBusy is the second concurrent-finish
// race: when N agents complete around the same time, their
// session.End calls UPDATE the same agent_sessions table.
// Without busy-retry, the second one would error and leave the
// session marked active forever (status row never updated).
func TestEnd_RetriesOnSQLiteBusy(t *testing.T) {
	inner := openTestDB(t)
	store := &busyStore{Store: inner, busyTimes: 0}
	s, err := New(store, "task", "/ws", "model")
	if err != nil {
		t.Fatalf("setup: New: %v", err)
	}
	// Inject a busy hit on the End UPDATE.
	store.mu.Lock()
	store.busyTimes = 1
	store.mu.Unlock()

	if err := s.End(StatusEnded); err != nil {
		t.Fatalf("End should have retried past a single busy hit, got: %v", err)
	}
	if s.Status != StatusEnded {
		t.Errorf("session status not updated to ended: %q", s.Status)
	}
}

// TestSaveMode_RetriesOnSQLiteBusy covers the third hot-path
// UPDATE — SaveMode is called on every mode-routing decision in
// chat dispatch.
func TestSaveMode_RetriesOnSQLiteBusy(t *testing.T) {
	inner := openTestDB(t)
	store := &busyStore{Store: inner, busyTimes: 0}
	s, err := New(store, "task", "/ws", "model")
	if err != nil {
		t.Fatalf("setup: New: %v", err)
	}
	store.mu.Lock()
	store.busyTimes = 1
	store.mu.Unlock()

	if err := SaveMode(store, s.ID, "Coding"); err != nil {
		t.Fatalf("SaveMode should have retried past a single busy hit, got: %v", err)
	}
}
