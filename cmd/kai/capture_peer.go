package main

import (
	"bytes"
	"os"
	"path/filepath"

	"kai/internal/filesource"
	"kai/internal/graph"
	"kai/internal/livesync"
)

// peerFilteredSource wraps a FileSource and rewrites or drops files so a
// default `kai capture` records only the caller's own changes — live-synced
// peer content is reverted out of each file (or excluded entirely). It does
// NOT touch disk: only the snapshot the capture builds is affected, so the
// peer's edits stay live on disk for the running sync session.
type peerFilteredSource struct {
	inner     filesource.FileSource
	overrides map[string][]byte // path -> "my version" content
	omit      map[string]bool   // path -> drop from snapshot entirely
}

func (s *peerFilteredSource) GetFiles() ([]*filesource.FileInfo, error) {
	files, err := s.inner.GetFiles()
	if err != nil {
		return nil, err
	}
	out := make([]*filesource.FileInfo, 0, len(files))
	for _, f := range files {
		if s.omit[f.Path] {
			continue
		}
		if c, ok := s.overrides[f.Path]; ok {
			nf := *f
			nf.Content = c
			nf.CachedDigest = "" // force re-hash of substituted content
			out = append(out, &nf)
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *peerFilteredSource) GetFile(path string) (*filesource.FileInfo, error) {
	if s.omit[path] {
		return nil, os.ErrNotExist
	}
	f, err := s.inner.GetFile(path)
	if err != nil {
		return nil, err
	}
	if c, ok := s.overrides[path]; ok {
		nf := *f
		nf.Content = c
		nf.CachedDigest = ""
		return &nf, nil
	}
	return f, nil
}

func (s *peerFilteredSource) Identifier() string { return s.inner.Identifier() }
func (s *peerFilteredSource) SourceType() string { return s.inner.SourceType() }

// buildPeerExclusion reads the live-synced ledger and, for each ledgered path,
// reconstructs the caller's own version (reverting the peer's contribution).
// Returns the override/omit maps and how many files were affected (0 = nothing
// to exclude, caller skips wrapping).
func buildPeerExclusion(db *graph.DB, kaiDirPath, capturePath string) (overrides map[string][]byte, omit map[string]bool, n int) {
	ledger, ok := livesync.LoadSyncedLedger(kaiDirPath)
	if !ok {
		return nil, nil, 0
	}
	overrides = make(map[string][]byte)
	omit = make(map[string]bool)
	for path, entry := range ledger {
		current, err := os.ReadFile(filepath.Join(capturePath, filepath.FromSlash(path)))
		if err != nil {
			continue // file gone locally; nothing to reconstruct
		}
		var myBase []byte
		if entry.BaseDigest != "" {
			myBase, _ = db.ReadObject(entry.BaseDigest)
		}
		peer, err := db.ReadObject(entry.PeerDigest)
		if err != nil {
			continue
		}
		result, ok := livesync.ReconstructLocal(myBase, peer, current, path)
		if !ok {
			continue // conflict / no base — keep the file whole (include)
		}
		if result == nil {
			omit[path] = true // peer-only, untouched by me → exclude
			n++
			continue
		}
		if !bytes.Equal(result, current) {
			overrides[path] = result
			n++
		}
	}
	return overrides, omit, n
}

// applyPeerExclusion wraps source so default capture excludes live-synced peer
// changes. Returns the (possibly wrapped) source and the number of files
// affected. No-op (returns source unchanged, n=0) when there's no ledger.
func applyPeerExclusion(source filesource.FileSource, db *graph.DB, kaiDirPath, capturePath string) (filesource.FileSource, int) {
	ov, omit, n := buildPeerExclusion(db, kaiDirPath, capturePath)
	if n == 0 {
		return source, 0
	}
	return &peerFilteredSource{inner: source, overrides: ov, omit: omit}, n
}
