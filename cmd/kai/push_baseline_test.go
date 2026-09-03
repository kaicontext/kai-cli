package main

import (
	"errors"
	"testing"

	"github.com/kaicontext/kai-engine/remote"
)

func TestExpectedOldForPush_GuardedRefPresentsBaseline(t *testing.T) {
	base, live, mine := []byte("BASE"), []byte("LIVE"), []byte("MINE")
	// The overwrite path: remote moved since we synced. The server must
	// see our stale baseline, not the live value we just read.
	if got := expectedOldForPush("snap.latest", live, base, mine, false); string(got) != "BASE" {
		t.Fatalf("guarded ref must present the synced baseline, got %q", got)
	}
	// Never synced: nil baseline → the server refuses to replace an
	// existing ref (old == nil && current != nil).
	if got := expectedOldForPush("snap.latest", live, nil, mine, false); got != nil {
		t.Fatalf("never-synced clone must present nil, got %q", got)
	}
	// Creating the ref and a no-op re-push have nothing to protect.
	if got := expectedOldForPush("snap.latest", nil, base, mine, false); got != nil {
		t.Fatalf("absent remote ref must present nil, got %q", got)
	}
	if got := expectedOldForPush("snap.latest", live, base, live, false); string(got) != "LIVE" {
		t.Fatalf("no-op re-push must present live, got %q", got)
	}
	// Ancestry-checked yield over an ingest tip lands the swap.
	if got := expectedOldForPush("snap.latest", live, base, mine, true); string(got) != "LIVE" {
		t.Fatalf("yielded push must present live, got %q", got)
	}
	// Unguarded refs are unchanged.
	if got := expectedOldForPush("ws.s-1234.head", live, base, mine, false); string(got) != "LIVE" {
		t.Fatalf("unguarded ref must present live, got %q", got)
	}
}

func TestIngestYieldDecision(t *testing.T) {
	contains := map[string]bool{"abc1234": true}
	isAncestor := func(sha string) (bool, error) {
		if sha == "broken" {
			return false, errors.New("git failed")
		}
		return contains[sha], nil
	}
	if y, _ := ingestYieldDecision([]string{"abc1234"}, isAncestor); !y {
		t.Fatal("clone containing the tip commit must be allowed to replace the ingest tip")
	}
	if y, why := ingestYieldDecision([]string{"def5678"}, isAncestor); y || why == "" {
		t.Fatalf("clone missing the tip commit must be refused with a reason, got yield=%v %q", y, why)
	}
	if y, why := ingestYieldDecision(nil, isAncestor); y || why == "" {
		t.Fatalf("tip with no recorded commit must be refused, got yield=%v %q", y, why)
	}
	if y, _ := ingestYieldDecision([]string{"broken"}, isAncestor); y {
		t.Fatal("a git error is not a pass")
	}
	if y, _ := ingestYieldDecision([]string{"broken", "abc1234"}, isAncestor); !y {
		t.Fatal("any contained commit for the tip is enough")
	}
}

func TestShortShasForTarget(t *testing.T) {
	tip := []byte("TIP")
	refs := []*remote.RefEntry{
		{Name: "git.abc1234", Target: tip},
		{Name: "git.def5678", Target: []byte("OTHER")},
		{Name: "snap.latest", Target: tip},
		nil,
	}
	got := shortShasForTarget(refs, tip)
	if len(got) != 1 || got[0] != "abc1234" {
		t.Fatalf("want [abc1234], got %v", got)
	}
}
