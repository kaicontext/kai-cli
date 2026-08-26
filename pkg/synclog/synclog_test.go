package synclog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneRemovesOldLogs(t *testing.T) {
	kaiDir := t.TempDir()
	dir := filepath.Join(kaiDir, "sync-log")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	old := time.Now().AddDate(0, 0, -30).Format("2006-01-02") + ".jsonl"
	recent := time.Now().Format("2006-01-02") + ".jsonl"
	for _, name := range []string{old, recent} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	removed := Prune(kaiDir, 7)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Fatalf("recent log should survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Fatalf("old log should be removed")
	}
}

func TestPruneMissingDir(t *testing.T) {
	if got := Prune(t.TempDir(), 7); got != 0 {
		t.Fatalf("Prune on missing dir = %d, want 0", got)
	}
}
