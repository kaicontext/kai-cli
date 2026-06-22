package authorship

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// CheckpointWriter manages writing checkpoint files to disk.
// Checkpoints are lightweight JSON files stored outside SQLite
// to avoid WAL contention during rapid edits.
type CheckpointWriter struct {
	kaiDir    string
	sessionID string
	seq       atomic.Int64
}

// NewCheckpointWriter creates a writer for the given session.
func NewCheckpointWriter(kaiDir, sessionID string) *CheckpointWriter {
	return &CheckpointWriter{
		kaiDir:    kaiDir,
		sessionID: sessionID,
	}
}

// checkpointsDir returns the path to the checkpoints directory for this session.
func (w *CheckpointWriter) checkpointsDir() string {
	return filepath.Join(w.kaiDir, "checkpoints", w.sessionID)
}

// Write atomically writes a checkpoint to disk and returns the sequence number.
func (w *CheckpointWriter) Write(cp CheckpointRecord) (int64, error) {
	dir := w.checkpointsDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, fmt.Errorf("creating checkpoints dir: %w", err)
	}

	seq := w.seq.Add(1)
	cp.Version = 1
	cp.SessionID = w.sessionID

	data, err := json.Marshal(cp)
	if err != nil {
		return 0, fmt.Errorf("marshaling checkpoint: %w", err)
	}

	filename := fmt.Sprintf("%06d.json", seq)
	target := filepath.Join(dir, filename)
	tmp := target + ".tmp"

	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return 0, fmt.Errorf("writing checkpoint: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("renaming checkpoint: %w", err)
	}

	return seq, nil
}

// ReadPendingCheckpoints reads all checkpoint files from all sessions under .kai/checkpoints/.
// Returns them sorted by timestamp.
func ReadPendingCheckpoints(kaiDir string) ([]CheckpointRecord, error) {
	cpDir := filepath.Join(kaiDir, "checkpoints")
	if _, err := os.Stat(cpDir); os.IsNotExist(err) {
		return nil, nil // no checkpoints yet
	}

	sessions, err := os.ReadDir(cpDir)
	if err != nil {
		return nil, fmt.Errorf("reading checkpoints dir: %w", err)
	}

	var all []CheckpointRecord
	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		sessDir := filepath.Join(cpDir, sess.Name())
		files, err := os.ReadDir(sessDir)
		if err != nil {
			continue
		}

		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sessDir, f.Name()))
			if err != nil {
				continue
			}
			var cp CheckpointRecord
			if err := json.Unmarshal(data, &cp); err != nil {
				continue
			}
			all = append(all, cp)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp < all[j].Timestamp
	})

	return all, nil
}

// ClearProcessedCheckpoints removes all checkpoint files after consolidation.
func ClearProcessedCheckpoints(kaiDir string) error {
	cpDir := filepath.Join(kaiDir, "checkpoints")
	if _, err := os.Stat(cpDir); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(cpDir)
}

// CountPendingCheckpoints returns the total number of pending checkpoint files.
func CountPendingCheckpoints(kaiDir string) int {
	cpDir := filepath.Join(kaiDir, "checkpoints")
	sessions, err := os.ReadDir(cpDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, sess := range sessions {
		if !sess.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(cpDir, sess.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".json") {
				count++
			}
		}
	}
	return count
}

// SessionCount returns how many checkpoint files exist for a given session.
func SessionCount(kaiDir, sessionID string) int {
	sessDir := filepath.Join(kaiDir, "checkpoints", sessionID)
	files, err := os.ReadDir(sessDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			count++
		}
	}
	return count
}
