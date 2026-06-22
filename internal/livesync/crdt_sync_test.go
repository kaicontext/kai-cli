package livesync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-core/crdt"
	"kai/pkg/synclog"
)

// newCRDTEngine builds a client-less engine in crdtMode for testing the op
// receive/generate glue directly (no network).
func newCRDTEngine(t *testing.T, site string) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	return &Engine{
		workDir:   dir,
		kaiDir:    dir,
		syncAgent: site,
		crdtMode:  true,
		docs:      make(map[string]*crdt.Doc),
		log:       synclog.NewSyncLogWriter(dir),
	}, dir
}

// genOps mirrors what pushOpsForChange produces: genesis ops on first sight of a
// file, else a Reconcile diff against the doc. It also advances the local doc,
// exactly as the push path does.
func genOps(e *Engine, rel, abs string) []byte {
	content, _ := os.ReadFile(abs)
	existed := e.hasDoc(rel)
	doc, genesis := e.docFor(rel, content)
	var ops []crdt.Op
	if !existed {
		ops = genesis
	} else {
		ops = crdt.Reconcile(doc, content)
	}
	data, _ := crdt.MarshalOps(ops)
	return data
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two engines converge through the op-transport glue: genesis, sequential
// edits both directions, and concurrent disjoint edits — all via the real
// applyOps receive path and the wire codec.
func TestCRDTEngineConvergence(t *testing.T) {
	eA, dirA := newCRDTEngine(t, "A")
	eB, dirB := newCRDTEngine(t, "B")

	appA := filepath.Join(dirA, "app.js")
	appB := filepath.Join(dirB, "app.js")

	// A creates the file → genesis ops → B applies.
	writeFile(t, appA, "function base() {\n  return 0;\n}\n")
	opsGenesis := genOps(eA, "app.js", appA)
	eB.applyOps("app.js", appB, opsGenesis, "A")

	got, _ := os.ReadFile(appB)
	if !bytes.Equal(got, []byte("function base() {\n  return 0;\n}\n")) {
		t.Fatalf("genesis didn't reach B: %q", got)
	}

	// B edits → A applies.
	writeFile(t, appB, "function base() {\n  return 1;\n}\n")
	opsB := genOps(eB, "app.js", appB)
	eA.applyOps("app.js", appA, opsB, "B")
	if a, _ := os.ReadFile(appA); !bytes.Equal(a, []byte("function base() {\n  return 1;\n}\n")) {
		t.Fatalf("B's edit didn't reach A: %q", a)
	}

	// Concurrent disjoint edits from the same base. Both append a different
	// function. Each generates ops against its own doc, then they exchange.
	base := "function base() {\n  return 1;\n}\n"
	writeFile(t, appA, base+"function aa() { return \"A\"; }\n")
	writeFile(t, appB, base+"function bb() { return \"B\"; }\n")
	opsA2 := genOps(eA, "app.js", appA)
	opsB2 := genOps(eB, "app.js", appB)

	eA.applyOps("app.js", appA, opsB2, "B")
	eB.applyOps("app.js", appB, opsA2, "A")

	finalA, _ := os.ReadFile(appA)
	finalB, _ := os.ReadFile(appB)
	if !bytes.Equal(finalA, finalB) {
		t.Fatalf("did not converge:\n A %q\n B %q", finalA, finalB)
	}
	for _, want := range []string{"function base()", "function aa()", "function bb()"} {
		if !bytes.Contains(finalA, []byte(want)) {
			t.Fatalf("converged content missing %q:\n%q", want, finalA)
		}
	}
}

// applyOps must mark the path sync-written so the local watcher doesn't echo the
// applied bytes straight back as a "local change" (feedback loop).
func TestCRDTApplyOpsMarksSyncWritten(t *testing.T) {
	eA, dirA := newCRDTEngine(t, "A")
	eB, dirB := newCRDTEngine(t, "B")

	appA := filepath.Join(dirA, "x.txt")
	appB := filepath.Join(dirB, "x.txt")
	writeFile(t, appA, "one\ntwo\n")
	eB.applyOps("x.txt", appB, genOps(eA, "x.txt", appA), "A")

	if !eB.IsSyncWritten("x.txt") {
		t.Fatal("applyOps did not mark x.txt sync-written; watcher would echo it back")
	}
}

// Doc snapshots persist across a restart: a fresh engine pointed at the same
// kaiDir restores the Doc (no replay-from-genesis) and continues converging.
func TestCRDTDocPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mk := func(site string) *Engine {
		return &Engine{
			workDir: dir, kaiDir: dir, syncAgent: site, crdtMode: true,
			docs: make(map[string]*crdt.Doc), log: synclog.NewSyncLogWriter(dir),
		}
	}

	app := filepath.Join(dir, "app.js")
	writeFile(t, app, "function f(){\n  return 1;\n}\n")

	// First engine: originate the file, then persist its Doc.
	e1 := mk("A")
	genOps(e1, "app.js", app) // builds + mutates e1's doc to match disk
	e1.saveAllCRDTDocs()
	want := e1.docs["app.js"].Materialize()

	// Second engine (simulating a restart): restore from disk, no genesis replay.
	e2 := mk("A")
	if n := e2.loadCRDTDocs(); n != 1 {
		t.Fatalf("expected 1 restored doc, got %d", n)
	}
	if !e2.hasDoc("app.js") {
		t.Fatal("doc not restored")
	}
	if got := e2.docs["app.js"].Materialize(); !bytes.Equal(got, want) {
		t.Fatalf("restored doc mismatch:\n got %q\nwant %q", got, want)
	}

	// The restored doc keeps working: a local edit reconciles to ops as usual,
	// and a peer that genesis-replayed the same history applies them identically.
	writeFile(t, app, "function f(){\n  return 2;\n}\n")
	moreOps := genOps(e2, "app.js", app)

	peerDir := t.TempDir()
	peer := &Engine{
		workDir: peerDir, kaiDir: peerDir, syncAgent: "B", crdtMode: true,
		docs: make(map[string]*crdt.Doc), log: synclog.NewSyncLogWriter(peerDir),
	}
	peerApp := filepath.Join(peerDir, "app.js")
	// Peer first received the original content (genesis), then the delta.
	peer.applyOps("app.js", peerApp, mustOps(t, e1Genesis(want)), "A")
	peer.applyOps("app.js", peerApp, moreOps, "A")
	if got, _ := os.ReadFile(peerApp); !bytes.Equal(got, e2.docs["app.js"].Materialize()) {
		t.Fatalf("peer did not converge with restored doc:\n peer %q\n e2   %q", got, e2.docs["app.js"].Materialize())
	}
}

// e1Genesis returns the deterministic genesis ops for content (as the originator
// would have pushed them).
func e1Genesis(content []byte) []crdt.Op { return crdt.GenesisOps(content) }

func mustOps(t *testing.T, ops []crdt.Op) []byte {
	t.Helper()
	data, err := crdt.MarshalOps(ops)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A receiver whose disk holds STALE, non-identical content (e.g. a workspace
// polluted by earlier whole-file sync) must adopt the sender's genesis+edits
// and converge — NOT mint its own divergent genesis from stale bytes. This is
// the "lost newline between functions" regression: A has "greet}\n", B's stale
// disk has "greet}" (no newline); after A creates + edits, B must equal A.
func TestCRDTReceiverAdoptsSenderGenesisOverStaleDisk(t *testing.T) {
	eA, dirA := newCRDTEngine(t, "A")
	eB, dirB := newCRDTEngine(t, "B")

	appA := filepath.Join(dirA, "app.js")
	appB := filepath.Join(dirB, "app.js")

	// B has stale, newline-less content on disk from a prior session.
	writeFile(t, appB, "function greet(n){ return \"hi \" + n; }")

	// A originates the file (with a trailing newline) and pushes genesis.
	writeFile(t, appA, "function greet(n){ return \"hi \" + n; }\n")
	eB.applyOps("app.js", appB, genOps(eA, "app.js", appA), "A")

	// A appends greet2 and pushes the edit.
	writeFile(t, appA, "function greet(n){ return \"hi \" + n; }\nfunction greet2(n){ return \"again\" + n; }")
	eB.applyOps("app.js", appB, genOps(eA, "app.js", appA), "A")

	finalA, _ := os.ReadFile(appA)
	finalB, _ := os.ReadFile(appB)
	if !bytes.Equal(finalA, finalB) {
		t.Fatalf("receiver diverged over stale disk:\n A %q\n B %q", finalA, finalB)
	}
	if bytes.Contains(finalB, []byte("}function greet2")) {
		t.Fatalf("newline between functions lost on B: %q", finalB)
	}
}

// A second client that already has the file on disk (e.g. from clone) and then
// receives the genesis ops must NOT duplicate the content — deterministic
// genesis makes the integrate idempotent against a disk-seeded doc.
func TestCRDTNoDuplicationWithExistingDisk(t *testing.T) {
	eA, dirA := newCRDTEngine(t, "A")
	eB, dirB := newCRDTEngine(t, "B")

	content := "alpha\nbeta\ngamma\n"
	appA := filepath.Join(dirA, "f.txt")
	appB := filepath.Join(dirB, "f.txt")
	writeFile(t, appA, content)
	writeFile(t, appB, content) // B already has identical content (cloned)

	// A pushes genesis; B (which seeds its doc from its own identical disk)
	// integrates A's genesis ops.
	eB.applyOps("f.txt", appB, genOps(eA, "f.txt", appA), "A")

	got, _ := os.ReadFile(appB)
	if !bytes.Equal(got, []byte(content)) {
		t.Fatalf("content duplicated/garbled on B:\n got %q\nwant %q", got, content)
	}
}
