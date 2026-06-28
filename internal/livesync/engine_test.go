package livesync

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/authorship"
	"kai/pkg/synclog"
)

// --- persisted sync-state ---

func newStateEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{kaiDir: t.TempDir()}
}

func TestSyncState_SaveAndLoad(t *testing.T) {
	e := newStateEngine(t)

	if _, ok := e.LoadState(); ok {
		t.Fatal("expected no state on a fresh kaiDir")
	}

	e.SaveState([]string{"a.go", "b.go"})

	st, ok := e.LoadState()
	if !ok {
		t.Fatal("expected state after save")
	}
	if !st.Enabled {
		t.Error("expected Enabled=true")
	}
	if len(st.Files) != 2 || st.Files[0] != "a.go" || st.Files[1] != "b.go" {
		t.Errorf("expected [a.go b.go], got %v", st.Files)
	}
	if st.LastSeq != 0 {
		t.Errorf("expected LastSeq=0 on first save, got %d", st.LastSeq)
	}
}

func TestSyncState_SaveSyncSeqAdvances(t *testing.T) {
	e := newStateEngine(t)
	e.SaveState([]string{"a.go"})

	e.saveSyncSeq(42)

	st, ok := e.LoadState()
	if !ok {
		t.Fatal("expected state")
	}
	if st.LastSeq != 42 {
		t.Errorf("expected LastSeq=42, got %d", st.LastSeq)
	}
	if len(st.Files) != 1 || st.Files[0] != "a.go" {
		t.Errorf("expected files preserved, got %v", st.Files)
	}
}

func TestSyncState_SaveStatePreservesLastSeq(t *testing.T) {
	e := newStateEngine(t)
	e.SaveState([]string{"a.go"})
	e.saveSyncSeq(99)

	e.SaveState([]string{"c.go"})

	st, ok := e.LoadState()
	if !ok {
		t.Fatal("expected state")
	}
	if st.LastSeq != 99 {
		t.Errorf("expected LastSeq=99 to survive SaveState, got %d", st.LastSeq)
	}
	if len(st.Files) != 1 || st.Files[0] != "c.go" {
		t.Errorf("expected new files [c.go], got %v", st.Files)
	}
}

func TestSyncState_Clear(t *testing.T) {
	e := newStateEngine(t)
	e.SaveState([]string{"a.go"})
	e.saveSyncSeq(7)

	e.ClearState()

	if _, ok := e.LoadState(); ok {
		t.Error("expected LoadState to fail after clear")
	}
	if _, err := os.Stat(e.statePath()); !os.IsNotExist(err) {
		t.Errorf("expected sync-state.json to be gone, stat err=%v", err)
	}
}

func TestSyncState_FileFormat(t *testing.T) {
	// Lock the on-disk JSON shape so we don't break compatibility with a
	// sync-state.json written by `kai live on` or an older binary.
	e := newStateEngine(t)
	e.SaveState([]string{"x.go"})
	e.saveSyncSeq(13)

	raw, err := os.ReadFile(filepath.Join(e.kaiDir, "sync-state.json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", got["enabled"])
	}
	if got["last_seq"] != float64(13) {
		t.Errorf("expected last_seq=13, got %v", got["last_seq"])
	}
	files, _ := got["files"].([]interface{})
	if len(files) != 1 || files[0] != "x.go" {
		t.Errorf("expected files=[x.go], got %v", got["files"])
	}
}

// --- peer-attribution checkpoints ---

func peerCheckpointEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	return &Engine{
		kaiDir:   dir,
		cpWriter: authorship.NewCheckpointWriter(dir, "test-session"),
	}
}

func TestWritePeerCheckpoint_FreshFileWritesWholeRange(t *testing.T) {
	e := peerCheckpointEngine(t)

	newC := []byte("line one\nline two\nline three\n")
	e.writePeerCheckpoint("src/foo.go", nil, newC, "claude-code", "modify")

	cps, err := authorship.ReadPendingCheckpoints(e.kaiDir)
	if err != nil {
		t.Fatalf("read checkpoints: %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(cps))
	}
	cp := cps[0]
	if cp.Agent != "claude-code" || cp.AuthorType != "ai" || !cp.PeerOrigin || cp.Action != "modify" {
		t.Errorf("unexpected checkpoint: %+v", cp)
	}
	if cp.StartLine != 1 || cp.EndLine != 4 {
		t.Errorf("expected lines 1-4, got %d-%d", cp.StartLine, cp.EndLine)
	}
}

func TestWritePeerCheckpoint_OnlyDiffRangeAttributed(t *testing.T) {
	e := peerCheckpointEngine(t)

	old := []byte("a\nb\nc\nd\ne\n")
	newC := []byte("a\nb\nX\nY\ne\n") // lines 3 and 4 changed
	e.writePeerCheckpoint("src/foo.go", old, newC, "cursor", "modify")

	cps, _ := authorship.ReadPendingCheckpoints(e.kaiDir)
	if len(cps) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(cps))
	}
	if cps[0].StartLine != 3 || cps[0].EndLine != 4 {
		t.Errorf("expected lines 3-4, got %d-%d", cps[0].StartLine, cps[0].EndLine)
	}
}

func TestWritePeerCheckpoint_NoChangeAndEmptyAgentAndNilWriter(t *testing.T) {
	e := peerCheckpointEngine(t)
	same := []byte("identical\nfile\n")
	e.writePeerCheckpoint("src/foo.go", same, same, "claude-code", "modify")
	e.writePeerCheckpoint("src/foo.go", nil, []byte("hi\n"), "", "modify")
	cps, _ := authorship.ReadPendingCheckpoints(e.kaiDir)
	if len(cps) != 0 {
		t.Errorf("expected 0 checkpoints (no-change + empty-agent), got %d", len(cps))
	}

	// nil writer must not panic.
	nilE := &Engine{kaiDir: t.TempDir()}
	nilE.writePeerCheckpoint("src/foo.go", nil, []byte("hi\n"), "claude-code", "modify")
}

// --- 3-way line merge ---

func TestNaiveLineMerge3(t *testing.T) {
	base := []byte("a\nb\nc\nd\ne\n")

	// Disjoint edits: local changes line 2, incoming changes line 4 → merge.
	local := []byte("a\nB\nc\nd\ne\n")
	incoming := []byte("a\nb\nc\nD\ne\n")
	merged, ok := naiveLineMerge3(base, local, incoming)
	if !ok {
		t.Fatal("expected disjoint edits to merge")
	}
	if want := []byte("a\nB\nc\nD\ne\n"); !bytes.Equal(merged, want) {
		t.Errorf("merge mismatch:\n got %q\nwant %q", merged, want)
	}

	// Overlapping edits on the same line → conflict (ok=false).
	l2 := []byte("a\nX\nc\nd\ne\n")
	i2 := []byte("a\nY\nc\nd\ne\n")
	if _, ok := naiveLineMerge3(base, l2, i2); ok {
		t.Error("expected overlapping edits to conflict")
	}

	// One side unchanged → take the other.
	if got, ok := naiveLineMerge3(base, base, incoming); !ok || !bytes.Equal(got, incoming) {
		t.Errorf("expected incoming when local==base, ok=%v got=%q", ok, got)
	}
}

// TestApplySyncContent_ConcurrentMergeNoClobber covers the concurrent-edit
// bug: with a properly seeded common base, a peer's disjoint edit must
// 3-way merge into local edits (not clobber them), and the base must advance
// to the merged result so the next round converges too.
func TestApplySyncContent_ConcurrentMergeNoClobber(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{workDir: dir, kaiDir: dir, log: synclog.NewSyncLogWriter(dir)}

	base := []byte("a\nb\nc\n")
	e.setBase("notes.txt", base) // common ancestor (seeded at sync start)

	// Local added a line at the end (our own edit, pushed — base stays at base).
	local := []byte("a\nb\nc\nL\n")
	abs := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(abs, local, 0644); err != nil {
		t.Fatal(err)
	}

	// Peer's concurrent edit added a line at the start.
	incoming := []byte("I\na\nb\nc\n")
	e.applySyncContent("notes.txt", abs, incoming, "peer")

	got, _ := os.ReadFile(abs)
	want := []byte("I\na\nb\nc\nL\n")
	if !bytes.Equal(got, want) {
		t.Errorf("concurrent merge clobbered/diverged:\n got %q\nwant %q", got, want)
	}

	// Base advances to the server canonical we merged against (incoming), not
	// the merged result — so our follow-up push tells the server which
	// canonical our edit was based on and it folds us in.
	e.baseMu.RLock()
	gotBase := e.base["notes.txt"]
	e.baseMu.RUnlock()
	if !bytes.Equal(gotBase, incoming) {
		t.Errorf("base should be the canonical (incoming):\n got %q\nwant %q", gotBase, incoming)
	}
}

func TestReconstructLocal(t *testing.T) {
	myBase := []byte("a\nb\nc\nd\ne\n")
	peer := []byte("a\nb\nC\nd\ne\n")    // peer changed line 3: c→C
	current := []byte("a\nb\nC\nd\nE\n") // peer's C + my edit line 5: e→E

	// Mixed file, disjoint edits → keep my line, revert peer's line.
	got, ok := ReconstructLocal(myBase, peer, current, "notes.txt")
	if !ok {
		t.Fatal("expected disjoint reconstruct to succeed")
	}
	if want := []byte("a\nb\nc\nd\nE\n"); !bytes.Equal(got, want) {
		t.Errorf("reconstruct:\n got %q\nwant %q", got, want)
	}

	// Peer-only (I never edited since the peer) → my version is the base.
	if got, ok := ReconstructLocal(myBase, peer, peer, "notes.txt"); !ok || !bytes.Equal(got, myBase) {
		t.Errorf("peer-only should return myBase, ok=%v got=%q", ok, got)
	}

	// Peer-created file, untouched by me → drop (nil, true).
	if got, ok := ReconstructLocal(nil, peer, peer, "new.txt"); !ok || got != nil {
		t.Errorf("peer-created untouched should be (nil,true), got (%q,%v)", got, ok)
	}

	// Peer-created file I've since edited → can't revert (nil, false) → keep whole.
	if _, ok := ReconstructLocal(nil, peer, current, "new.txt"); ok {
		t.Error("peer-created+edited should be ok=false (keep whole)")
	}

	// Overlapping edits (both changed line 3) → conflict, ok=false.
	myEditSameLine := []byte("a\nb\nX\nd\ne\n")
	if _, ok := ReconstructLocal(myBase, peer, myEditSameLine, "notes.txt"); ok {
		t.Error("overlapping edits should conflict (ok=false)")
	}
}
