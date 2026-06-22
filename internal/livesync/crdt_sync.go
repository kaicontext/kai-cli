package livesync

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kaicontext/kai-core/crdt"
	"kai/pkg/synclog"
)

// crdt_sync.go is the op-transport path (KAI_CRDT_SYNC=1). Instead of pushing
// whole-file bytes and relying on the server's semantic 3-way merge, each client
// keeps a per-file RGA Doc, turns local file edits into ops (Reconcile), and
// converges by integrating peers' ops. The server is a dumb ordered relay.
//
// Why client-side: correct ops can only be generated where the causal context
// lives. Reconstructing a diff from whole-file bytes at a central point can't
// tell "peer never saw this line" from "peer deleted it" — the last-write loss
// the RGA exists to eliminate.

// crdtSite is this replica's globally-unique RGA site id.
func (e *Engine) crdtSite() string { return e.syncAgent }

// docFor returns the RGA Doc for relPath, creating it via deterministic genesis
// from the given disk bytes if absent. Deterministic genesis means two clients
// that independently seed the same file produce identical genesis ops, so they
// converge without duplicating content. Returns the doc and the genesis ops to
// push (nil if the doc already existed).
func (e *Engine) docFor(relPath string, diskBytes []byte) (*crdt.Doc, []crdt.Op) {
	e.docsMu.Lock()
	defer e.docsMu.Unlock()
	if d, ok := e.docs[relPath]; ok {
		return d, nil
	}
	d := crdt.Genesis(e.crdtSite(), diskBytes)
	e.docs[relPath] = d
	return d, crdt.GenesisOps(diskBytes)
}

// hasDoc reports whether a Doc already exists for relPath.
func (e *Engine) hasDoc(relPath string) bool {
	e.docsMu.Lock()
	defer e.docsMu.Unlock()
	_, ok := e.docs[relPath]
	return ok
}

// pushOpsForChange diffs relPath's current disk bytes against its Doc, applies
// the resulting ops locally, and pushes them. Used in crdtMode by PushChanges.
func (e *Engine) pushOpsForChange(relPath string) {
	absPath := filepath.Join(e.workDir, relPath)
	content, err := os.ReadFile(absPath)
	if err != nil || len(content) > 512*1024 {
		return
	}

	existed := e.hasDoc(relPath)
	doc, genesis := e.docFor(relPath, content)

	var ops []crdt.Op
	if !existed {
		// First time we've seen this file: the genesis ops ARE the content. Push
		// them so peers (and late joiners replaying the ledger) get the baseline.
		ops = genesis
	} else {
		// Diff the doc's materialized view against disk → minimal ops. This also
		// mutates the doc to match disk.
		e.docsMu.Lock()
		ops = crdt.Reconcile(doc, content)
		e.docsMu.Unlock()
	}
	if len(ops) == 0 {
		return
	}

	opsJSON, merr := crdt.MarshalOps(ops)
	if merr != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] op marshal failed for %s: %v\n", relPath, merr)
		return
	}
	syncPath := toGitRelativePath(e.workDir, relPath)
	if err := e.client.SyncPushOps(e.syncAgent, e.channelID, syncPath, base64.StdEncoding.EncodeToString(opsJSON)); err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] op push failed for %s: %v\n", syncPath, err)
		return
	}
	// Disk already equals doc.Materialize() (the local edit IS the disk state);
	// record it as the synced base so we don't echo it back as a change.
	e.setBase(relPath, content)
	fmt.Fprintf(os.Stderr, "[kai-sync] pushed %d op(s) for %s\n", len(ops), syncPath)
	e.log.Write(synclog.SyncLogEntry{
		Event:     synclog.EventPush,
		File:      syncPath,
		Agent:     e.syncAgent,
		Channel:   e.channelID,
		Timestamp: time.Now().UnixMilli(),
		Detail:    fmt.Sprintf("%d ops", len(ops)),
	})
}

// migrateUnconverted converts whole-file workspaces to op-transport on the
// first run after the cutover: for each tracked file that has content but no
// Doc yet (no op history in the ledger), it pushes a deterministic genesis as
// the file's ops baseline. Because genesis is content-derived, two peers
// migrating the same file concurrently produce identical ops (idempotent), and
// a file already migrated by a peer arrives via replay with a Doc, so it's
// skipped here. Lazy migration on first edit (pushOpsForChange) covers any file
// not in the tracked set.
func (e *Engine) migrateUnconverted(files []string) {
	migrated := 0
	for _, rel := range files {
		if e.hasDoc(rel) {
			continue // already op-based (replay rebuilt its Doc)
		}
		abs := filepath.Join(e.workDir, rel)
		content, err := os.ReadFile(abs)
		if err != nil || len(content) == 0 || len(content) > 512*1024 {
			continue
		}
		_, genesis := e.docFor(rel, content)
		if len(genesis) == 0 {
			continue
		}
		opsJSON, merr := crdt.MarshalOps(genesis)
		if merr != nil {
			continue
		}
		syncPath := toGitRelativePath(e.workDir, rel)
		if err := e.client.SyncPushOps(e.syncAgent, e.channelID, syncPath, base64.StdEncoding.EncodeToString(opsJSON)); err != nil {
			continue
		}
		e.setBase(rel, content)
		migrated++
	}
	if migrated > 0 {
		fmt.Fprintf(os.Stderr, "[kai-sync] migrated %d file(s) to op-transport\n", migrated)
	}
}

// applyOps integrates a peer op-batch into relPath's Doc and writes the
// materialized result to disk. The single receive path for crdtMode, shared by
// the SSE handler and replay catch-up.
func (e *Engine) applyOps(relPath, absPath string, opsJSON []byte, agent string) {
	ops, err := crdt.UnmarshalOps(opsJSON)
	if err != nil || len(ops) == 0 {
		return
	}

	// Ensure a Doc exists. Crucially, seed it EMPTY (not from local disk): the
	// incoming ops carry the sender's genesis + edits and rebuild the exact
	// content. Genesis-from-disk only ever happens on the PUSH side, for a file
	// the local user originates. Seeding a receiver's Doc from its own (possibly
	// stale) disk would mint a divergent genesis and corrupt convergence — that
	// was the "lost newline between functions" bug.
	if !e.hasDoc(relPath) {
		e.docsMu.Lock()
		if _, ok := e.docs[relPath]; !ok {
			e.docs[relPath] = crdt.New(e.crdtSite())
		}
		e.docsMu.Unlock()
	}

	// My on-disk version BEFORE integrating this peer batch — the base that
	// kai capture later subtracts the peer's contribution against. nil = the
	// file didn't exist locally (peer-created).
	preLocal, _ := os.ReadFile(absPath)

	e.docsMu.Lock()
	doc := e.docs[relPath]
	for _, op := range ops {
		doc.Integrate(op)
	}
	merged := doc.Materialize()
	e.docsMu.Unlock()

	// Skip the write if disk already matches (avoids a watcher feedback loop).
	// This is also the SELF-ECHO path: the server broadcasts a push back to its
	// sender, whose own ops re-integrate idempotently → merged == disk → we
	// return here and do NOT record a peer contribution against ourselves.
	if bytes.Equal(preLocal, merged) {
		e.setBase(relPath, merged)
		return
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] mkdir failed for %s: %v\n", relPath, err)
		return
	}
	e.markSyncWritten(relPath)
	if err := os.WriteFile(absPath, merged, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[kai-sync] write failed for %s: %v\n", relPath, err)
		return
	}
	e.setBase(relPath, merged)
	// Post-apply bookkeeping — mirror the whole-file applySyncContent path so
	// op-transport has the same behavior:
	//   - writePeerCheckpoint: attribute the changed line ranges to the peer
	//     agent so `kai blame` credits them (not the local user).
	//   - recordPeerContribution: write the live-synced ledger so `kai capture`
	//     (default) subtracts the peer's edits. Without these two, op-transport
	//     made peerA's and peerB's captures identical and mis-attributed blame.
	e.writePeerCheckpoint(relPath, preLocal, merged, agent, "modify")
	e.recordPeerContribution(relPath, preLocal, merged)
	fmt.Fprintf(os.Stderr, "[kai-sync] applied %d op(s) to %s from %s\n", len(ops), relPath, agent)
	e.log.Write(synclog.SyncLogEntry{
		Event:     synclog.EventReceive,
		File:      relPath,
		Agent:     e.syncAgent,
		PeerAgent: agent,
		Channel:   e.channelID,
		Timestamp: time.Now().UnixMilli(),
		Detail:    fmt.Sprintf("%d ops integrated", len(ops)),
	})
}

// --- Doc snapshot persistence (Phase C: avoid replay-from-genesis on restart) ---
//
// Each file's RGA Doc is snapshotted under <kaiDir>/crdt/. On restart the engine
// restores the Docs and replays only the ops since the last saved seq, instead
// of rebuilding from seq 0. Persistence is purely an optimization — a missing or
// stale snapshot just falls back to a full replay (ops are idempotent), so we
// never need a migration and a crash never corrupts state.

type crdtDocFile struct {
	Path string          `json:"path"`
	Doc  json.RawMessage `json:"doc"`
}

func (e *Engine) crdtDir() string { return filepath.Join(e.kaiDir, "crdt") }

func (e *Engine) crdtDocPath(relPath string) string {
	name := hex.EncodeToString(blake3Sum([]byte(relPath))) + ".json"
	return filepath.Join(e.crdtDir(), name)
}

// saveCRDTDoc persists one file's Doc snapshot. Best-effort.
func (e *Engine) saveCRDTDoc(relPath string) {
	e.docsMu.Lock()
	doc := e.docs[relPath]
	e.docsMu.Unlock()
	if doc == nil {
		return
	}
	snap, err := doc.Snapshot()
	if err != nil {
		return
	}
	wrapped, err := json.Marshal(crdtDocFile{Path: relPath, Doc: snap})
	if err != nil {
		return
	}
	if err := os.MkdirAll(e.crdtDir(), 0o755); err != nil {
		return
	}
	tmp := e.crdtDocPath(relPath) + ".tmp"
	if err := os.WriteFile(tmp, wrapped, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, e.crdtDocPath(relPath)) // atomic replace
}

// saveAllCRDTDocs persists every tracked Doc. Called periodically and on Stop.
func (e *Engine) saveAllCRDTDocs() {
	e.docsMu.Lock()
	paths := make([]string, 0, len(e.docs))
	for p := range e.docs {
		paths = append(paths, p)
	}
	e.docsMu.Unlock()
	for _, p := range paths {
		e.saveCRDTDoc(p)
	}
}

// loadCRDTDocs restores persisted Doc snapshots into e.docs. Returns the number
// restored (0 ⇒ cold start, caller should replay from seq 0).
func (e *Engine) loadCRDTDocs() int {
	entries, err := os.ReadDir(e.crdtDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.crdtDir(), ent.Name()))
		if err != nil {
			continue
		}
		var wrapped crdtDocFile
		if err := json.Unmarshal(data, &wrapped); err != nil || wrapped.Path == "" {
			continue
		}
		doc, err := crdt.RestoreDoc(wrapped.Doc)
		if err != nil {
			continue
		}
		e.docsMu.Lock()
		e.docs[wrapped.Path] = doc
		e.docsMu.Unlock()
		n++
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "[kai-sync] restored %d CRDT doc snapshot(s)\n", n)
	}
	return n
}
