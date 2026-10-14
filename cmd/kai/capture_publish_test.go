package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
)

// A snapshot that can't be published as snap.latest must fail the command,
// not print a warning and report success: a capture nothing points at is not
// a capture you can get back to.
func TestCreateSnapshotFromDir_FailsWhenRefPublishFails(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func() *graph.DB {
		t.Helper()
		d := t.TempDir()
		db, err := graph.Open(filepath.Join(d, "db.sqlite"), filepath.Join(d, "objects"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}

	// Control: publishing works.
	ok := open()
	if _, err := createSnapshotFromDir(ok, src); err != nil {
		t.Fatalf("control capture failed: %v", err)
	}
	if _, err := ref.NewRefManager(ok).Get("snap.latest"); err != nil {
		t.Fatalf("control capture did not publish snap.latest: %v", err)
	}

	// Every ref write fails.
	bad := open()
	ref.NewRefManager(bad) // create the tables the triggers attach to
	for _, stmt := range []string{
		`CREATE TRIGGER refs_no_insert BEFORE INSERT ON refs BEGIN SELECT RAISE(FAIL, 'ref store unavailable'); END`,
		`CREATE TRIGGER refs_no_update BEFORE UPDATE ON refs BEGIN SELECT RAISE(FAIL, 'ref store unavailable'); END`,
	} {
		if _, err := bad.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_, err := createSnapshotFromDir(bad, src)
	if err == nil {
		t.Fatal("capture reported success although snap.latest could not be published")
	}
	if !strings.Contains(err.Error(), "could not be published as snap.latest") {
		t.Errorf("error should say the snapshot wasn't published: %v", err)
	}
}
