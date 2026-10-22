package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/kaipath"
)

// The session-ingest hook wrote <root>/.kai/loops beside a .git/kai store,
// which split ~/Desktop/riddle into two databases. Now the loop record goes
// into the one store, and a legacy .git/kai store is moved to .kai first.
func TestLoopRecordLandsInTheOneStore(t *testing.T) {
	t.Setenv("KAI_DIR", "")
	root := t.TempDir()
	legacy := filepath.Join(root, ".git", "kai")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, kaipath.StoreFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	writeLoopRecord(&loopRecord{ID: "s1", Cwd: root})

	if _, err := os.Stat(filepath.Join(root, ".kai", "loops", "s1.json")); err != nil {
		t.Fatalf("loop record not in .kai: %v", err)
	}
	if got := kaipath.Stores(root); len(got) != 1 || got[0] != filepath.Join(root, ".kai") {
		t.Fatalf("stores after ingest = %v, want only .kai", got)
	}
}

func TestSplitStoreMessageNamesBothStores(t *testing.T) {
	root := t.TempDir()
	stores := []string{filepath.Join(root, ".kai"), filepath.Join(root, ".git", "kai")}
	msg := splitStoreMessage(stores)
	for _, s := range stores {
		if !strings.Contains(msg, filepath.Join(s, kaipath.StoreFile)) {
			t.Errorf("message does not name %s:\n%s", s, msg)
		}
	}
	if !strings.Contains(msg, "kai store merge") {
		t.Errorf("message gives no way out:\n%s", msg)
	}
}

func TestSplitStoreExemptKeepsDiagnosisAvailable(t *testing.T) {
	for _, path := range []string{"kai store merge", "kai doctor", "kai log ingest", "kai __complete", "kai __completeNoDesc"} {
		if !splitStoreExempt[path] {
			t.Errorf("%s must run on a split project", path)
		}
	}
	// `kai merge` shares its name with `kai store merge`; exemption is by path.
	for _, path := range []string{"kai capture", "kai push", "kai status", "kai merge", "kai log"} {
		if splitStoreExempt[path] {
			t.Errorf("%s must refuse on a split project", path)
		}
	}
}

// A store whose refs table can't be read must not look like an empty,
// never-pushed store when the user is choosing which one to keep.
func TestDescribeStoreReportsUnreadableRefs(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, kaipath.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE nodes (kind TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if got := describeStore(dir); !strings.HasPrefix(got, "unreadable: ") {
		t.Fatalf("describeStore without a refs table = %q, want unreadable", got)
	}
}
