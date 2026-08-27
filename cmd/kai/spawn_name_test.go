package main

import (
	"strings"
	"testing"
)

// The workspace name is the session identity: it keys the live-sync
// channel, the CRDT room, the ws.<name>.* refs, and (via kai ship)
// the PR branch. These tests pin that no path can ever regress to a
// shared constant like the old "spawn-1".
func TestResolveWorkspaceBase(t *testing.T) {
	// --session derives s-<first 8 of the sanitized uuid>.
	got, err := resolveWorkspaceBase("3f2a9c1e-77b4-4d2e-9a10-000011112222", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s-3f2a9c1e" {
		t.Fatalf("session base = %q, want s-3f2a9c1e", got)
	}

	// Explicit --ws-name wins over --session.
	got, err = resolveWorkspaceBase("3f2a9c1e-77b4-4d2e-9a10-000011112222", "Review-Fix")
	if err != nil {
		t.Fatal(err)
	}
	if got != "review-fix" {
		t.Fatalf("explicit base = %q, want review-fix", got)
	}

	// No inputs: random, unique, s- prefixed.
	a, err := resolveWorkspaceBase("", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolveWorkspaceBase("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "s-") || len(a) != 10 {
		t.Fatalf("random base = %q, want s-<8 hex>", a)
	}
	if a == b {
		t.Fatalf("two invocations produced the same base %q — collision class the rename exists to kill", a)
	}

	// Garbage-only input errors rather than silently minting "".
	if _, err := resolveWorkspaceBase("", "///"); err == nil {
		t.Fatal("expected error for unusable --ws-name")
	}
}

func TestWorkspaceNameFor(t *testing.T) {
	if got := workspaceNameFor("s-3f2a9c1e", 1); got != "s-3f2a9c1e" {
		t.Fatalf("n=1: %q", got)
	}
	if got := workspaceNameFor("s-3f2a9c1e", 3); got != "s-3f2a9c1e-3" {
		t.Fatalf("n=3: %q", got)
	}
}
