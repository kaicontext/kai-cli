// Per-read instrumentation. Appends one CSV row per view tool result
// to <KaiDir>/read-log.csv. Sprint 0 from the 2026-05-26 master
// hardening spec — TOK-instr. Without per-read data we can't aim
// the large-read guard or size the cap correctly (the run-58
// analysis was retrofit from peak-token grep over planner-debug.log,
// which is fragile and only works for planner runs).
//
// Format: append-only CSV with header on first write per file.
// Columns:
//   ts          — RFC3339 timestamp
//   path        — workspace-relative path the agent requested
//   offset      — 1-based line offset the agent passed (0 = top)
//   limit       — line limit the agent passed
//   total_lines — total lines in the file
//   lines_read  — lines actually returned (after offset + limit + truncation)
//   bytes       — byte length of the returned text body
//   est_tokens  — ~bytes/4 estimate (good enough for sizing decisions)
//   whole_file  — true when the read returned the file in full
//
// Bounded best-effort: if the log can't be opened, the read still
// returns normally. Instrumentation must not be load-bearing.
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var readLogMu sync.Mutex

// logRead writes one row to <kaiDir>/read-log.csv. kaiDir is the
// project's .kai directory; the caller passes Set.Primary().KaiDir
// (or its multi-root analog). Empty kaiDir → silent no-op.
func logRead(kaiDir, relPath string, offset, limit, totalLines, linesRead, bytes int, wholeFile bool) {
	if kaiDir == "" {
		return
	}
	readLogMu.Lock()
	defer readLogMu.Unlock()

	path := filepath.Join(kaiDir, "read-log.csv")
	// Establish whether to write the header (file didn't exist or was empty).
	needHeader := false
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		needHeader = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if needHeader {
		_, _ = f.WriteString("ts,path,offset,limit,total_lines,lines_read,bytes,est_tokens,whole_file\n")
	}
	estTokens := bytes / 4
	// Quote path defensively in case of commas (rare but possible).
	row := fmt.Sprintf("%s,%q,%d,%d,%d,%d,%d,%d,%t\n",
		time.Now().UTC().Format(time.RFC3339),
		relPath, offset, limit, totalLines, linesRead, bytes, estTokens, wholeFile,
	)
	_, _ = f.WriteString(row)
}
