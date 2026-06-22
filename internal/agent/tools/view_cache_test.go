package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestViewCache_ExactReissue keeps the round-22 behavior: a second
// view with identical (path, offset, limit) returns the cached
// notice when the file is unchanged.
func TestViewCache_ExactReissue(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("alpha\nbeta\ngamma\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()

	// First read populates the cache.
	r1, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"a.go","offset":0,"limit":10}`,
	})
	if r1.IsError || !strings.Contains(r1.Content, "1: alpha") {
		t.Fatalf("first read failed: %+v", r1)
	}

	// Exact re-issue hits the gate.
	r2, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"a.go","offset":0,"limit":10}`,
	})
	if !strings.Contains(r2.Content, "already viewed") {
		t.Errorf("expected cached notice on identical re-issue, got: %s", r2.Content)
	}
}

// TestViewCache_SubsetReissue is the TOK P1-3 widening: a request
// for a range fully contained in a prior range returns the cached
// notice instead of re-rendering. Previously the strict (path,
// offset, limit) key would have missed this and re-read.
func TestViewCache_SubsetReissue(t *testing.T) {
	ws := t.TempDir()
	body := strings.Repeat("line\n", 200)
	if err := os.WriteFile(filepath.Join(ws, "big.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()

	// Prior view: lines 0-99 (offset=0, limit=100).
	r1, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"big.go","offset":0,"limit":100}`,
	})
	if r1.IsError {
		t.Fatalf("first read failed: %+v", r1)
	}

	// Subsequent narrower view at lines 20-30 should be contained.
	r2, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"big.go","offset":20,"limit":10}`,
	})
	if !strings.Contains(r2.Content, "already viewed") {
		t.Errorf("expected cached notice on contained subset, got: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "fully contains") {
		t.Errorf("expected subset-specific phrasing, got: %s", r2.Content)
	}
}

// TestViewCache_NonOverlappingPagesThrough — a deeper page (range
// past what was previously viewed) should NOT trigger the gate.
// The agent might genuinely be reading new content.
func TestViewCache_NonOverlappingPagesThrough(t *testing.T) {
	ws := t.TempDir()
	body := strings.Repeat("line\n", 500)
	if err := os.WriteFile(filepath.Join(ws, "big.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()

	// Prior: lines 0-99.
	tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"big.go","offset":0,"limit":100}`,
	})

	// Now request lines 200-249 — not contained.
	r2, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"big.go","offset":200,"limit":50}`,
	})
	if strings.Contains(r2.Content, "already viewed") {
		t.Errorf("non-overlapping page should NOT be cached, got: %s", r2.Content)
	}
	// Should contain actual numbered lines from the page.
	if !strings.Contains(r2.Content, "201: line") {
		t.Errorf("expected real page content, got: %s", r2.Content)
	}
}

// TestViewCache_MtimeChangeResetsCache — if the file changes
// between views, the cache must be invalidated; the agent's prior
// excerpt is no longer authoritative.
func TestViewCache_MtimeChangeResetsCache(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "edited.go")
	if err := os.WriteFile(path, []byte("v1\nv1\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()
	tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"edited.go","offset":0,"limit":10}`,
	})

	// Force an mtime advance and rewrite.
	bump := func() {
		st, _ := os.Stat(path)
		_ = os.Chtimes(path, st.ModTime().Add(2*1e9), st.ModTime().Add(2*1e9))
	}
	if err := os.WriteFile(path, []byte("v2\nv2\nv2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bump()

	r2, _ := tool.Run(context.Background(), ToolCall{
		Input: `{"file_path":"edited.go","offset":0,"limit":10}`,
	})
	if strings.Contains(r2.Content, "already viewed") {
		t.Errorf("mtime change should invalidate cache, got cached notice: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "1: v2") {
		t.Errorf("expected fresh content after mtime change, got: %s", r2.Content)
	}
}
