package views

import (
	"testing"
	"time"
)

// TestMaybeAutoEscalate_SuppressedWhileToolInFlight pins the fix for the
// "agent idle for 4m0s — auto-escalating via kai_consult" false-trip: a
// silent long-running tool (e.g. a first-use kai_search backfill) leaves
// lastActivity stale, but the model isn't stalled — it's waiting on its
// tool. While toolInFlight, the model-escalation must not fire.
func TestMaybeAutoEscalate_SuppressedWhileToolInFlight(t *testing.T) {
	past := time.Now().Add(-2 * autoEscalateAfter) // well past the idle threshold

	r := &REPL{services: &PlannerServices{}, lastActivity: past, toolInFlight: true}
	if cmd := r.maybeAutoEscalate(); cmd != nil {
		t.Fatalf("expected NO escalation while a tool is in flight, got a command")
	}

	// Sanity: the one-shot/zero-time guards still short-circuit when
	// nothing has happened yet (no false escalation on a fresh REPL).
	fresh := &REPL{services: &PlannerServices{}}
	if cmd := fresh.maybeAutoEscalate(); cmd != nil {
		t.Fatalf("expected nil on a fresh REPL (zero lastActivity), got a command")
	}
}

