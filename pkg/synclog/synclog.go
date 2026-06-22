// Package synclog records sync events (push, receive, merge, conflict, skip)
// for diagnostics and conflict resolution auditing.
package synclog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Event types for sync log entries.
const (
	EventPush     = "push"
	EventReceive  = "receive"
	EventMerge    = "merge"
	EventConflict = "conflict"
	EventSkip     = "skip"
)

// SyncLogEntry records a single sync event.
type SyncLogEntry struct {
	Event     string `json:"event"`
	File      string `json:"file"`
	Agent     string `json:"agent"`
	PeerAgent string `json:"peer_agent,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Detail    string `json:"detail,omitempty"`
}

// SyncLogWriter appends sync log entries to a JSONL file.
type SyncLogWriter struct {
	dir string
}

// NewSyncLogWriter creates a writer that appends to .kai/sync-log/.
func NewSyncLogWriter(kaiDir string) *SyncLogWriter {
	return &SyncLogWriter{dir: filepath.Join(kaiDir, "sync-log")}
}

// Write appends a sync log entry to the current log file.
func (w *SyncLogWriter) Write(entry SyncLogEntry) {
	if w == nil {
		return
	}
	if err := os.MkdirAll(w.dir, 0755); err != nil {
		return
	}
	name := time.Now().Format("2006-01-02") + ".jsonl"
	f, err := os.OpenFile(filepath.Join(w.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s\n", data)
}

// CountPendingSyncLogs returns the number of unprocessed sync log files.
func CountPendingSyncLogs(kaiDir string) int {
	dir := filepath.Join(kaiDir, "sync-log")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	return count
}
