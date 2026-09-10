package main

import (
	"path/filepath"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// A bare `kai ship` inside a registered spawn takes the server path; the
// same command in a plain checkout stays local; --local and --server
// override the tree, and asking for both is refused.
func TestShipUseServer_DefaultsToServerInASpawn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spawn := filepath.Join(t.TempDir(), "kai-desktop")
	checkout := t.TempDir()
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SourceRepo: "/src/kai-desktop", Durable: true}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		cwd           string
		server, local bool
		want          bool
	}{
		{"spawn, no flags", spawn, false, false, true},
		{"checkout, no flags", checkout, false, false, false},
		{"spawn, --local", spawn, false, true, false},
		{"checkout, --server", checkout, true, false, true},
	}
	for _, c := range cases {
		got, err := shipUseServer(c.cwd, c.server, c.local)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: server=%v, want %v", c.name, got, c.want)
		}
	}
	if _, err := shipUseServer(spawn, true, true); err == nil {
		t.Fatal("--server with --local should be refused")
	}
}

// The spawn matcher is what the mode default, the session trailer and the
// server base all read; a spawn registered under a symlinked path must
// still match its resolved spelling.
func TestShipSpawnEntry_MatchesResolvedSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	if err := spawnpkg.Add(spawnpkg.Entry{Path: dir, SessionID: "sid-1", Durable: true}); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := shipSpawnEntry(resolved); e == nil || e.SessionID != "sid-1" {
		t.Fatalf("resolved path should match the registered spawn, got %+v", e)
	}
	if e := shipSpawnEntry(t.TempDir()); e != nil {
		t.Fatalf("unregistered dir matched a spawn: %+v", e)
	}
}
