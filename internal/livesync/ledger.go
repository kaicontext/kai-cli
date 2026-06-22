package livesync

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/kaicontext/kai-core/merge"
)

// SyncedEntry records, for one path, the content needed to later subtract a
// peer's live-synced contribution from a `kai capture`:
//   - BaseDigest: my version of the file just before the FIRST peer edit
//     landed this session (the common ancestor). "" means the file did not
//     exist locally (peer-created).
//   - PeerDigest: the latest content the sync engine wrote to disk for this
//     path (peer's contribution, possibly already merged with my edits).
//
// Both digests address blobs in the object store, so a separate `kai capture`
// process can read them back.
type SyncedEntry struct {
	BaseDigest string `json:"base_digest"`
	PeerDigest string `json:"peer_digest"`
}

// ledgerFileName is where the per-repo live-synced ledger lives.
const ledgerFileName = "live-synced.json"

func ledgerPath(kaiDir string) string { return filepath.Join(kaiDir, ledgerFileName) }

// LoadSyncedLedger reads the live-synced ledger. ok=false when it's missing or
// empty, so callers can skip the peer-exclusion path entirely.
func LoadSyncedLedger(kaiDir string) (map[string]SyncedEntry, bool) {
	data, err := os.ReadFile(ledgerPath(kaiDir))
	if err != nil {
		return nil, false
	}
	var m map[string]SyncedEntry
	if json.Unmarshal(data, &m) != nil || len(m) == 0 {
		return nil, false
	}
	return m, true
}

func writeSyncedLedger(kaiDir string, m map[string]SyncedEntry) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(ledgerPath(kaiDir), data, 0644)
}

// recordPeerContribution notes that `peerContent` (now on disk at path) was
// influenced by a peer, with `local` being my version immediately before this
// apply. Blobs are written to the object store so capture can reconstruct.
// The earliest base for a path is preserved across multiple applies; the peer
// digest tracks the latest written content.
func (e *Engine) recordPeerContribution(path string, local, peerContent []byte) {
	if e.db == nil {
		return
	}
	peerDigest, err := e.db.WriteObject(peerContent)
	if err != nil {
		return
	}
	e.ledgerMu.Lock()
	defer e.ledgerMu.Unlock()
	if e.ledger == nil {
		if m, ok := LoadSyncedLedger(e.kaiDir); ok {
			e.ledger = m
		} else {
			e.ledger = make(map[string]SyncedEntry)
		}
	}
	entry, exists := e.ledger[path]
	if !exists {
		// First peer edit since the last reconcile — pin my pre-peer base.
		baseDigest := ""
		if local != nil {
			if d, err := e.db.WriteObject(local); err == nil {
				baseDigest = d
			}
		}
		entry.BaseDigest = baseDigest
	}
	entry.PeerDigest = peerDigest
	e.ledger[path] = entry
	writeSyncedLedger(e.kaiDir, e.ledger)
}

// ReconstructLocal returns "my version" of a file: the current on-disk content
// with the peer's contribution reverted, keeping my own edits. It is pure (no
// I/O) so `kai capture` can call it after loading blobs.
//
//   - current == peerContent (I never edited since the peer)      → myBase
//     (revert the peer entirely; nil myBase means drop the file)
//   - otherwise (I edited after the peer)                         → 3-way
//     merge with base=peerContent, mine=current, theirs=myBase, which keeps
//     my post-peer edits and reverts the peer's hunks
//
// ok=false signals the caller should keep `current` whole (true conflict, or
// a peer-created file I've since edited with no base to revert to).
func ReconstructLocal(myBase, peerContent, current []byte, path string) (result []byte, ok bool) {
	if bytesEqual(current, peerContent) {
		// Untouched by me since the peer wrote it → my version is the base.
		return myBase, true
	}
	if myBase == nil {
		// Peer-created file I've since edited — no clean base to revert to.
		return nil, false
	}
	lang := detectSyncLang(path)
	if lang != "" {
		if res, err := merge.Merge3Way(peerContent, current, myBase, lang); err == nil && res.Success {
			if merged, ok := res.Files["file"]; ok {
				return merged, true
			}
		}
	} else {
		if merged, ok := naiveLineMerge3(peerContent, current, myBase); ok {
			return merged, true
		}
	}
	return nil, false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
