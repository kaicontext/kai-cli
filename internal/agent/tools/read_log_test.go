package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogRead_AppendsRowsWithHeader pins the TOK-instr format —
// header on first write, row per call thereafter. The cap-sizing
// analysis depends on parsing this file post-hoc, so format stability
// matters.
func TestLogRead_AppendsRowsWithHeader(t *testing.T) {
	dir := t.TempDir()

	logRead(dir, "src/a.go", 0, 100, 250, 100, 4000, true)
	logRead(dir, "src/b.go", 50, 40, 200, 40, 1600, false)

	data, err := os.ReadFile(filepath.Join(dir, "read-log.csv"))
	if err != nil {
		t.Fatalf("read-log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 1 header + 2 rows = 3 lines, got %d:\n%s", len(lines), data)
	}
	if !strings.HasPrefix(lines[0], "ts,path,offset,limit,total_lines,lines_read,bytes,est_tokens,whole_file") {
		t.Errorf("header missing/wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"src/a.go"`) || !strings.HasSuffix(lines[1], "true") {
		t.Errorf("row 1 wrong: %q", lines[1])
	}
	if !strings.Contains(lines[2], `"src/b.go"`) || !strings.HasSuffix(lines[2], "false") {
		t.Errorf("row 2 wrong: %q", lines[2])
	}
	// est_tokens column = bytes/4.
	if !strings.Contains(lines[1], ",1000,") {
		t.Errorf("expected est_tokens=1000 (4000/4) in row 1: %q", lines[1])
	}
}

// TestLogRead_EmptyKaiDirNoOps — instrumentation is best-effort;
// callers that don't have a kaiDir (test scaffolds, ephemeral tool
// runs) get a silent no-op instead of an error.
func TestLogRead_EmptyKaiDirNoOps(t *testing.T) {
	// Should not panic, should not create files anywhere.
	logRead("", "anything", 0, 100, 100, 100, 100, true)
}

// TestLogRead_HeaderOnlyOnce — opening an existing log appends
// without re-emitting the header. Critical for CSV parseability.
func TestLogRead_HeaderOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	logRead(dir, "a.go", 0, 10, 10, 10, 100, true)
	logRead(dir, "b.go", 0, 10, 10, 10, 100, true)
	logRead(dir, "c.go", 0, 10, 10, 10, 100, true)
	data, _ := os.ReadFile(filepath.Join(dir, "read-log.csv"))
	count := strings.Count(string(data), "ts,path,offset,limit")
	if count != 1 {
		t.Errorf("want exactly 1 header line in log, got %d:\n%s", count, data)
	}
}
