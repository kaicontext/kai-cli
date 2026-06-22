package orchestrator

import (
	"context"
	"strings"
	"testing"

	"kai/internal/planner"
)

// TestExecute_RejectsEmptyPlan: the orchestrator must not silently
// no-op on an empty plan; that would mask planner bugs.
func TestExecute_RejectsEmptyPlan(t *testing.T) {
	_, err := Execute(context.Background(), &planner.WorkPlan{}, Config{}, nil, "/tmp", "/tmp/.kai")
	if err == nil || !strings.Contains(err.Error(), "empty plan") {
		t.Fatalf("expected empty-plan error, got %v", err)
	}
}

func TestExecute_RejectsNilDB(t *testing.T) {
	plan := &planner.WorkPlan{Agents: []planner.AgentTask{{Name: "x", Prompt: "p"}}}
	_, err := Execute(context.Background(), plan, Config{}, nil, "/tmp", "/tmp/.kai")
	if err == nil || !strings.Contains(err.Error(), "nil db") {
		t.Fatalf("expected nil-db error, got %v", err)
	}
}

// runOneAgent without a Provider should set ExitErr with a clear
// message — this catches the most common misconfiguration (forgot to
// log in / forgot to set up planner services).
func TestRunOneAgent_RejectsMissingProvider(t *testing.T) {
	run := &AgentRun{Task: planner.AgentTask{Name: "x", Prompt: "p"}}
	runOneAgent(context.Background(), run, Config{}, "/tmp")
	if run.ExitErr == nil || !strings.Contains(run.ExitErr.Error(), "AgentProvider is nil") {
		t.Fatalf("expected provider-required error, got %v", run.ExitErr)
	}
}

// TestShouldIgnoreObserver moved here when the observer was removed
// in Slice 6 — its only consumer is now the absorb walk.
func TestShouldIgnoreObserver(t *testing.T) {
	cases := map[string]bool{
		"":                true,
		".":               true,
		".kai/db.sqlite":  true,
		".git/HEAD":       true,
		"node_modules/x":  false, // governed by .gitignore now, not hardcoded
		"src/main.go":     false,
		"README.md":       false,
		"my.kai/keep.txt": false, // only literal `.kai/` prefix
	}
	for in, want := range cases {
		if got := shouldIgnoreObserver(in); got != want {
			t.Errorf("shouldIgnoreObserver(%q): got %v, want %v", in, got, want)
		}
	}
}
