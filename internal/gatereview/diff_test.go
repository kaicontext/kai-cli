package gatereview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/util"
)

// setupDB stands up a real (file-backed) graph DB with the minimal
// schema the snapshot/file lookup paths exercise. Mirrors
// internal/workspace/rebase_test.go's helper so the gatereview tests
// don't need to import that test-only symbol.
func setupDB(t *testing.T) (*graph.DB, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "kai-gatereview-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	dbPath := filepath.Join(tmpDir, "test.db")
	objPath := filepath.Join(tmpDir, "objects")
	if err := os.MkdirAll(objPath, 0755); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("objects dir: %v", err)
	}
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("open db: %v", err)
	}
	schema := `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS nodes (id BLOB PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS edges (src BLOB NOT NULL, type TEXT NOT NULL, dst BLOB NOT NULL, at BLOB, created_at INTEGER NOT NULL, PRIMARY KEY (src, type, dst, at));
CREATE TABLE IF NOT EXISTS refs (name TEXT PRIMARY KEY, target_id BLOB NOT NULL, target_kind TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS slugs (target_id BLOB PRIMARY KEY, slug TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS logs (kind TEXT NOT NULL, seq INTEGER NOT NULL, id BLOB NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (kind, seq));
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("schema: %v", err)
	}
	return db, func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}
}

// insertSnap writes each path's content to the object store, inserts a
// File node carrying the resulting digest, and links the snapshot to
// each file via HAS_FILE. Returns the snapshot ID. Mirrors what real
// snapshot.Creator.CreateSnapshot does, minus the parsing/edge work
// gatereview doesn't care about.
//
// label is included in the snapshot payload purely to make the
// content-addressed node ID unique across calls. Without it, two
// successive insertSnap calls in the same millisecond hash to the
// same node ID (both payloads would carry only createdAt) and the
// second INSERT OR IGNORE silently merges the file sets — a real
// flake we hit during the gatereview smoke pass.
func insertSnap(t *testing.T, db *graph.DB, label string, files map[string]string) []byte {
	t.Helper()
	tx, err := db.BeginTx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var fileIDs [][]byte
	for path, content := range files {
		digest, err := db.WriteObject([]byte(content))
		if err != nil {
			t.Fatalf("write blob for %s: %v", path, err)
		}
		fid, err := db.InsertNode(tx, graph.KindFile, map[string]interface{}{
			"path":   path,
			"digest": digest,
		})
		if err != nil {
			t.Fatalf("insert file: %v", err)
		}
		fileIDs = append(fileIDs, fid)
	}
	snapID, err := db.InsertNode(tx, graph.KindSnapshot, map[string]interface{}{
		"createdAt": util.NowMs(),
		"label":     label,
	})
	if err != nil {
		t.Fatalf("insert snap: %v", err)
	}
	for _, fid := range fileIDs {
		if err := db.InsertEdge(tx, snapID, graph.EdgeHasFile, fid, nil); err != nil {
			t.Fatalf("edge: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return snapID
}

// markHeld stamps the gate-verdict fields onto a snapshot so it
// matches what `kai integrate` would have produced. The targetSnapshot
// field is the only payload key HeldSnapshotDiff strictly needs.
func markHeld(t *testing.T, db *graph.DB, snapID, targetID []byte) {
	t.Helper()
	node, err := db.GetNode(snapID)
	if err != nil {
		t.Fatalf("get snap: %v", err)
	}
	node.Payload["targetSnapshot"] = util.BytesToHex(targetID)
	node.Payload["gateVerdict"] = "review"
	node.Payload["gateBlastRadius"] = float64(2)
	node.Payload["gateReasons"] = []interface{}{"blast radius 2 > auto threshold 0"}
	node.Payload["gateTouches"] = []interface{}{"a.go", "b.go"}
	if err := db.UpdateNodePayload(snapID, node.Payload); err != nil {
		t.Fatalf("update payload: %v", err)
	}
}

func TestHeldSnapshotDiff_AddedModifiedDeleted(t *testing.T) {
	db, cleanup := setupDB(t)
	defer cleanup()

	baseID := insertSnap(t, db, "base", map[string]string{
		"unchanged.go": "package x\n",
		"modified.go":  "package x\n\nfunc Old() {}\n",
		"deleted.go":   "package x\n\n// will be deleted\n",
	})
	headID := insertSnap(t, db, "head", map[string]string{
		"unchanged.go": "package x\n",
		"modified.go":  "package x\n\nfunc New() {}\n",
		"added.go":     "package x\n\nfunc Brand() {}\n",
	})
	markHeld(t, db, headID, baseID)

	heldNode, err := db.GetNode(headID)
	if err != nil {
		t.Fatalf("get held: %v", err)
	}

	patch, err := HeldSnapshotDiff(db, heldNode)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if patch == "" {
		t.Fatal("expected non-empty diff")
	}

	// Added file: header + new content with `+` prefix.
	if !strings.Contains(patch, "+++ b/added.go") {
		t.Errorf("missing added-file header in patch:\n%s", patch)
	}
	if !strings.Contains(patch, "+func Brand() {}") {
		t.Errorf("added function not in patch:\n%s", patch)
	}

	// Modified file: both - and + lines for the changed function.
	if !strings.Contains(patch, "-func Old() {}") {
		t.Errorf("removed line missing:\n%s", patch)
	}
	if !strings.Contains(patch, "+func New() {}") {
		t.Errorf("added line missing:\n%s", patch)
	}

	// Deleted file: header + minus lines.
	if !strings.Contains(patch, "+++ /dev/null") {
		t.Errorf("missing deleted-file marker:\n%s", patch)
	}
	if !strings.Contains(patch, "-// will be deleted") {
		t.Errorf("deleted content not flagged:\n%s", patch)
	}

	// Unchanged file should NOT show up in the patch — diffs only
	// describe deltas, and HeldSnapshotDiff explicitly skips
	// equal-digest entries before invoking diffmatchpatch.
	if strings.Contains(patch, "unchanged.go") {
		t.Errorf("unchanged file leaked into patch:\n%s", patch)
	}
}

// TestHeldSnapshotDiff_BinaryDoesNotStarveSource reproduces the run-6
// bug: a build artifact (the compiled binary) was captured into the
// held snapshot and, rendered byte-by-byte ahead of the real source
// change, blew past the review's 24KB patch cap and truncated the
// actual fix out. The diff must (1) summarize the binary instead of
// rendering it, (2) keep the modified source change, and (3) stay
// small enough that the source survives truncation.
func TestHeldSnapshotDiff_BinaryDoesNotStarveSource(t *testing.T) {
	db, cleanup := setupDB(t)
	defer cleanup()

	// A NUL byte makes content read as binary; pad it large so a
	// byte-for-byte render would obliterate the patch budget.
	bigBinary := string(make([]byte, 200_000)) // 200KB of NUL

	baseID := insertSnap(t, db, "base", map[string]string{
		"todo.go": "package x\n\nfunc RemainingCount() {}\n",
	})
	headID := insertSnap(t, db, "head", map[string]string{
		"todo.go":      "package x\n\nfunc RemainingCountFixed() {}\n",
		"todoapp":      bigBinary, // build artifact, added
		"todo_test.go": "package x\n\nfunc TestRemainingCount(t *testing.T) {}\n",
	})
	markHeld(t, db, headID, baseID)

	node, _ := db.GetNode(headID)
	patch, err := HeldSnapshotDiff(db, node)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}

	// The binary is summarized, not rendered.
	if !strings.Contains(patch, "Binary file") {
		t.Errorf("binary file not summarized:\n%s", patch)
	}
	if strings.Count(patch, "\x00") != 0 {
		t.Errorf("raw binary bytes leaked into patch")
	}
	// The real source change survives and is reviewable.
	if !strings.Contains(patch, "+func RemainingCountFixed() {}") {
		t.Errorf("modified source change missing from patch:\n%s", patch)
	}
	// The whole patch stays well under the 24KB review cap, so nothing
	// real gets truncated.
	if len(patch) > 24000 {
		t.Errorf("patch is %d bytes — binary was not excluded", len(patch))
	}
	// Modified source is emitted before added files, so truncation (if
	// it ever happens) bites added files, not the fix under review.
	if strings.Index(patch, "todo.go") > strings.Index(patch, "todo_test.go") {
		t.Errorf("modified file should be ordered before added files:\n%s", patch)
	}
}

func TestHeldSnapshotDiff_MissingTarget(t *testing.T) {
	db, cleanup := setupDB(t)
	defer cleanup()

	// A snapshot with no targetSnapshot payload — what you'd get from
	// a snapshot that didn't go through `kai integrate`. The diff path
	// must error with a recognizable message rather than panicking.
	snapID := insertSnap(t, db, "orphan", map[string]string{"a.go": "x"})
	node, _ := db.GetNode(snapID)
	if _, err := HeldSnapshotDiff(db, node); err == nil {
		t.Fatal("expected error for missing targetSnapshot, got nil")
	} else if !strings.Contains(err.Error(), "targetSnapshot") {
		t.Fatalf("error should mention targetSnapshot: %v", err)
	}
}

func TestHeldSnapshotDiff_NoChanges(t *testing.T) {
	// Edge case: held snapshot is identical to its target (shouldn't
	// happen in practice but we shouldn't crash). Returns an empty
	// string so callers can render "no differences."
	db, cleanup := setupDB(t)
	defer cleanup()

	files := map[string]string{"a.go": "package a\n"}
	baseID := insertSnap(t, db, "identical-base", files)
	headID := insertSnap(t, db, "identical-head", files)
	markHeld(t, db, headID, baseID)

	node, _ := db.GetNode(headID)
	patch, err := HeldSnapshotDiff(db, node)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if patch != "" {
		t.Errorf("expected empty patch for identical snapshots, got:\n%s", patch)
	}
}

// TestAppendLineDiff_HunksNotWholeFile is the regression test for the
// gate-review truncation bug: a small change in a large file must
// produce a small hunked diff, not the entire file rendered as context.
func TestAppendLineDiff_HunksNotWholeFile(t *testing.T) {
	// 800-line file; change exactly one line in the middle.
	var before, after strings.Builder
	for i := 0; i < 800; i++ {
		line := fmt.Sprintf("line %d\n", i)
		before.WriteString(line)
		if i == 400 {
			after.WriteString("line 400 CHANGED\n")
		} else {
			after.WriteString(line)
		}
	}

	var sb strings.Builder
	appendLineDiff(&sb, before.String(), after.String())
	out := sb.String()

	gotLines := strings.Count(out, "\n")
	// One change + 3 lines context each side + 1 hunk header = ~8 lines.
	// Anything near 800 means the whole file leaked through as context.
	if gotLines > 20 {
		t.Fatalf("expected a small hunked diff, got %d lines:\n%s", gotLines, out)
	}
	if !strings.Contains(out, "+line 400 CHANGED") || !strings.Contains(out, "-line 400") {
		t.Fatalf("diff missing the actual change:\n%s", out)
	}
	if !strings.Contains(out, "@@ ") {
		t.Fatalf("diff has no @@ hunk header:\n%s", out)
	}
	// The change must survive a 24KB-style clip — i.e. it's near the top.
	if idx := strings.Index(out, "CHANGED"); idx > 200 {
		t.Fatalf("change is %d bytes deep — would be truncated by the review cap", idx)
	}
}

// TestAppendLineDiff_MultipleHunks: distant changes produce separate
// hunks, not one giant span swallowing everything between them.
func TestAppendLineDiff_MultipleHunks(t *testing.T) {
	var before, after strings.Builder
	for i := 0; i < 300; i++ {
		line := fmt.Sprintf("L%d\n", i)
		before.WriteString(line)
		if i == 10 || i == 250 {
			after.WriteString(fmt.Sprintf("L%d EDIT\n", i))
		} else {
			after.WriteString(line)
		}
	}
	var sb strings.Builder
	appendLineDiff(&sb, before.String(), after.String())
	out := sb.String()
	if n := strings.Count(out, "@@ "); n != 2 {
		t.Fatalf("expected 2 hunks for 2 distant changes, got %d:\n%s", n, out)
	}
	if strings.Count(out, "\n") > 30 {
		t.Fatalf("two 1-line changes produced an oversized diff:\n%s", out)
	}
}
