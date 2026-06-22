package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDiffEdges_AddedOnly(t *testing.T) {
	old := []EdgeDelta{}
	new := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"},
		{Src: "aaa", Type: "CALLS", Dst: "ccc"},
	}

	added, removed := diffEdges(old, new)
	if len(added) != 2 {
		t.Errorf("expected 2 added, got %d", len(added))
	}
	if len(removed) != 0 {
		t.Errorf("expected 0 removed, got %d", len(removed))
	}
}

func TestDiffEdges_RemovedOnly(t *testing.T) {
	old := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"},
	}
	new := []EdgeDelta{}

	added, removed := diffEdges(old, new)
	if len(added) != 0 {
		t.Errorf("expected 0 added, got %d", len(added))
	}
	if len(removed) != 1 {
		t.Errorf("expected 1 removed, got %d", len(removed))
	}
}

func TestDiffEdges_Mixed(t *testing.T) {
	old := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"},
		{Src: "aaa", Type: "CALLS", Dst: "ccc"},
	}
	new := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"}, // unchanged
		{Src: "aaa", Type: "CALLS", Dst: "ddd"},   // new
	}

	added, removed := diffEdges(old, new)
	if len(added) != 1 {
		t.Errorf("expected 1 added, got %d", len(added))
	}
	if added[0].Dst != "ddd" {
		t.Errorf("expected added edge dst=ddd, got %s", added[0].Dst)
	}
	if len(removed) != 1 {
		t.Errorf("expected 1 removed, got %d", len(removed))
	}
	if removed[0].Dst != "ccc" {
		t.Errorf("expected removed edge dst=ccc, got %s", removed[0].Dst)
	}
}

func TestDiffEdges_Identical(t *testing.T) {
	edges := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"},
		{Src: "ccc", Type: "CALLS", Dst: "ddd"},
	}

	added, removed := diffEdges(edges, edges)
	if len(added) != 0 {
		t.Errorf("expected 0 added, got %d", len(added))
	}
	if len(removed) != 0 {
		t.Errorf("expected 0 removed, got %d", len(removed))
	}
}

func TestDiffEdges_BothEmpty(t *testing.T) {
	added, removed := diffEdges(nil, nil)
	if len(added) != 0 {
		t.Errorf("expected 0 added, got %d", len(added))
	}
	if len(removed) != 0 {
		t.Errorf("expected 0 removed, got %d", len(removed))
	}
}

func TestRecordActivity(t *testing.T) {
	w := &Watcher{}

	w.recordActivity("src/main.go", "modified")
	w.recordActivity("src/lib.go", "created")

	entries := w.GetActivity()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Path != "src/main.go" {
		t.Errorf("expected path src/main.go, got %s", entries[0].Path)
	}
	if entries[0].Operation != "modified" {
		t.Errorf("expected op modified, got %s", entries[0].Operation)
	}
	if entries[1].Path != "src/lib.go" {
		t.Errorf("expected path src/lib.go, got %s", entries[1].Path)
	}
	if entries[1].Operation != "created" {
		t.Errorf("expected op created, got %s", entries[1].Operation)
	}
}

func TestRecordActivity_Expiry(t *testing.T) {
	w := &Watcher{}

	// Manually inject an old entry
	w.activityMu.Lock()
	w.activity = append(w.activity, ActivityEntry{
		Path:      "old_file.go",
		Operation: "modified",
		Timestamp: time.Now().Add(-10 * time.Minute),
	})
	w.activityMu.Unlock()

	// Add a fresh entry
	w.recordActivity("new_file.go", "created")

	entries := w.GetActivity()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (old one expired), got %d", len(entries))
	}
	if entries[0].Path != "new_file.go" {
		t.Errorf("expected new_file.go, got %s", entries[0].Path)
	}
}

func TestRecordEdgeDelta(t *testing.T) {
	w := &Watcher{}

	added := []EdgeDelta{
		{Src: "aaa", Type: "IMPORTS", Dst: "bbb"},
	}
	removed := []EdgeDelta{
		{Src: "ccc", Type: "CALLS", Dst: "ddd"},
	}

	w.recordEdgeDelta("src/main.go", added, removed)

	updates := w.flushEdgeDeltas()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].File != "src/main.go" {
		t.Errorf("expected file src/main.go, got %s", updates[0].File)
	}
	if len(updates[0].AddedEdges) != 1 {
		t.Errorf("expected 1 added edge, got %d", len(updates[0].AddedEdges))
	}
	if len(updates[0].RemovedEdges) != 1 {
		t.Errorf("expected 1 removed edge, got %d", len(updates[0].RemovedEdges))
	}

	// Second flush should be empty
	updates2 := w.flushEdgeDeltas()
	if len(updates2) != 0 {
		t.Errorf("expected 0 updates after flush, got %d", len(updates2))
	}
}

func TestRecordEdgeDelta_Accumulates(t *testing.T) {
	w := &Watcher{}

	w.recordEdgeDelta("src/main.go",
		[]EdgeDelta{{Src: "a", Type: "IMPORTS", Dst: "b"}},
		nil)

	w.recordEdgeDelta("src/main.go",
		[]EdgeDelta{{Src: "a", Type: "CALLS", Dst: "c"}},
		[]EdgeDelta{{Src: "a", Type: "IMPORTS", Dst: "d"}})

	updates := w.flushEdgeDeltas()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update (accumulated), got %d", len(updates))
	}
	if len(updates[0].AddedEdges) != 2 {
		t.Errorf("expected 2 added edges, got %d", len(updates[0].AddedEdges))
	}
	if len(updates[0].RemovedEdges) != 1 {
		t.Errorf("expected 1 removed edge, got %d", len(updates[0].RemovedEdges))
	}
}

func TestRecordEdgeDelta_SkipsEmpty(t *testing.T) {
	w := &Watcher{}

	w.recordEdgeDelta("src/main.go", nil, nil)

	updates := w.flushEdgeDeltas()
	if len(updates) != 0 {
		t.Errorf("expected 0 updates for empty delta, got %d", len(updates))
	}
}

func TestRecordEdgeDelta_MultipleFiles(t *testing.T) {
	w := &Watcher{}

	w.recordEdgeDelta("src/a.go",
		[]EdgeDelta{{Src: "a", Type: "IMPORTS", Dst: "b"}}, nil)
	w.recordEdgeDelta("src/b.go",
		nil, []EdgeDelta{{Src: "c", Type: "CALLS", Dst: "d"}})

	updates := w.flushEdgeDeltas()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
}

func TestRecordActivity_Concurrent(t *testing.T) {
	w := &Watcher{}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.recordActivity("file.go", "modified")
		}(i)
	}
	wg.Wait()

	entries := w.GetActivity()
	if len(entries) != 100 {
		t.Errorf("expected 100 entries, got %d", len(entries))
	}
}

func TestFlushEdgeDeltas_Concurrent(t *testing.T) {
	w := &Watcher{}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.recordEdgeDelta("file.go",
				[]EdgeDelta{{Src: "a", Type: "IMPORTS", Dst: "b"}}, nil)
		}(i)
	}
	wg.Wait()

	updates := w.flushEdgeDeltas()
	if len(updates) != 1 {
		t.Fatalf("expected 1 file entry, got %d", len(updates))
	}
	if len(updates[0].AddedEdges) != 50 {
		t.Errorf("expected 50 accumulated edges, got %d", len(updates[0].AddedEdges))
	}
}

func TestShouldIgnore_SkipsGitAndKai(t *testing.T) {
	tmpDir := t.TempDir()
	w := &Watcher{workDir: tmpDir}

	if !w.shouldIgnore(".kai/db.sqlite", filepath.Join(tmpDir, ".kai/db.sqlite")) {
		t.Error("should ignore .kai files")
	}
	if !w.shouldIgnore(".git/HEAD", filepath.Join(tmpDir, ".git/HEAD")) {
		t.Error("should ignore .git files")
	}
}

func TestShouldIgnore_AllowsSourceFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a .go file so DetectLang works
	goFile := filepath.Join(tmpDir, "main.go")
	os.WriteFile(goFile, []byte("package main"), 0644)

	w := &Watcher{workDir: tmpDir}

	if w.shouldIgnore("main.go", goFile) {
		t.Error("should not ignore .go files")
	}
}

func TestShouldIgnore_AllowsUnknownExtensions(t *testing.T) {
	// shouldIgnore only filters .kai/, .git/, and ignore-matcher patterns.
	// Unknown extensions like .dat pass through shouldIgnore — they're
	// filtered later by isParseableLang in handleCreateOrModify.
	tmpDir := t.TempDir()

	datFile := filepath.Join(tmpDir, "data.dat")
	os.WriteFile(datFile, []byte("data"), 0644)

	w := &Watcher{workDir: tmpDir}

	if w.shouldIgnore("data.dat", datFile) {
		t.Error("shouldIgnore should pass unknown extensions (filtered later)")
	}
}

func TestIsParseableLang(t *testing.T) {
	tests := []struct {
		lang     string
		expected bool
	}{
		{"go", true},
		{"ts", true},
		{"js", true},
		{"py", true},
		{"rs", true},
		{"rb", true},
		{"php", true},
		{"cs", true},
		{"sql", true},
		{"blob", false},
		{"text", false},
		{"json", false},
		{"", false},
	}

	for _, tt := range tests {
		result := isParseableLang(tt.lang)
		if result != tt.expected {
			t.Errorf("isParseableLang(%q) = %v, want %v", tt.lang, result, tt.expected)
		}
	}
}
