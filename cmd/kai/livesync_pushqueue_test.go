package main

import (
	"sort"
	"testing"
	"time"
)

// A local edit must reach the push side promptly — not on the watcher's 30s
// activity ticker, which made Mac→peer sync take ~20s.
func TestLivePushQueuePushesPromptly(t *testing.T) {
	q := newLivePushQueue(false)
	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 4)
	go q.Run(stop, func(p []string) { got <- p })

	start := time.Now()
	q.Add("kv.py")
	select {
	case p := <-got:
		if len(p) != 1 || p[0] != "kv.py" {
			t.Fatalf("pushed %v, want [kv.py]", p)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("push took %v, want well under 1s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edit was not pushed within 5s")
	}
}

// Paths queued before the pusher starts (edits while still connecting) go out
// in its first batch; repeated adds of one path coalesce.
func TestLivePushQueueKeepsEditsQueuedBeforeRun(t *testing.T) {
	q := newLivePushQueue(false)
	q.Add("a.py")
	q.Add("b.py")
	q.Add("a.py")

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 4)
	go q.Run(stop, func(p []string) { got <- p })

	select {
	case p := <-got:
		sort.Strings(p)
		if len(p) != 2 || p[0] != "a.py" || p[1] != "b.py" {
			t.Fatalf("pushed %v, want [a.py b.py]", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued edits were not pushed after Run started")
	}
}

// Checkpoint mode accumulates edits and pushes nothing until drained.
func TestLivePushQueueCheckpointHoldsUntilDrain(t *testing.T) {
	q := newLivePushQueue(true)
	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 4)
	go q.Run(stop, func(p []string) { got <- p })

	q.Add("kv.py")
	select {
	case p := <-got:
		t.Fatalf("checkpoint mode pushed %v before a checkpoint", p)
	case <-time.After(200 * time.Millisecond):
	}
	if p := q.Drain(); len(p) != 1 || p[0] != "kv.py" {
		t.Fatalf("Drain = %v, want [kv.py]", p)
	}
	if p := q.Drain(); len(p) != 0 {
		t.Fatalf("second Drain = %v, want empty", p)
	}
}
