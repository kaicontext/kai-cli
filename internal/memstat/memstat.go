// Package memstat captures local memory snapshots and appends them
// to ~/.kai/memory-stats.log so post-mortem investigation has data
// when the OS SIGKILLs kai for memory pressure ("kai code [killed]"
// with no other diagnostic). The PostHog telemetry in
// internal/telemetry is for opt-in usage analytics; this package is
// always-on and local-only, lossy by design — a single failed
// write does not propagate to the caller.
//
// Snapshot fields:
//   - RSS:     resident set size from getrusage(RUSAGE_SELF)
//   - HeapInUse, HeapAlloc, Sys: from runtime.MemStats (Go-side)
//   - NumGC:   GC cycles completed (a runaway GC budget often
//              precedes an OOM kill)
//
// 2026-05-14 dogfood: a 'kai code' invocation was SIGKILL'd after
// the user came back to an idle terminal. Without this log we had
// no way to know whether kai had been at 200MB or 3GB when the
// kernel reached for it.
package memstat

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Snapshot is a single in-process memory observation.
type Snapshot struct {
	Time        time.Time
	RSSBytes    int64  // resident set size from getrusage
	HeapAlloc   uint64 // bytes allocated and still in use (Go heap)
	HeapInUse   uint64 // bytes in in-use spans
	Sys         uint64 // total bytes obtained from the OS
	NumGC       uint32 // completed GC cycles
	NumGoroutines int
}

// Capture takes a snapshot of the current process's memory state.
// Safe to call concurrently — runtime.ReadMemStats acquires the
// stop-the-world lock internally.
func Capture() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	var ru unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &ru)
	rss := int64(ru.Maxrss)
	// On Linux ru_maxrss is in kilobytes; on Darwin/BSD it's in
	// bytes. Detect by checking GOOS at runtime (cheap, no build
	// tags needed for the single-binary kai distribution).
	if runtime.GOOS == "linux" {
		rss *= 1024
	}

	return Snapshot{
		Time:          time.Now(),
		RSSBytes:      rss,
		HeapAlloc:     ms.HeapAlloc,
		HeapInUse:     ms.HeapInuse,
		Sys:           ms.Sys,
		NumGC:         ms.NumGC,
		NumGoroutines: runtime.NumGoroutine(),
	}
}

// FormatLine renders a snapshot as a single tab-separated log line.
// Stable format so future tooling can read the log without parsing
// JSON: timestamp | reason | RSS(MB) | HeapInUse(MB) | Sys(MB) |
// goroutines | gc-cycles.
func FormatLine(s Snapshot, reason string) string {
	return fmt.Sprintf(
		"%s\treason=%s\trss=%dMB\theap_inuse=%dMB\tsys=%dMB\tgoroutines=%d\tgc=%d",
		s.Time.UTC().Format(time.RFC3339),
		reason,
		s.RSSBytes/(1024*1024),
		int64(s.HeapInUse)/(1024*1024),
		int64(s.Sys)/(1024*1024),
		s.NumGoroutines,
		s.NumGC,
	)
}

// Log appends a snapshot line to ~/.kai/memory-stats.log with the
// given reason ("boot", "turn", "capture-pre", "idle-30s", etc.).
// Best-effort: nil home dir, missing parent, or write failure all
// silently drop the entry — telemetry must never bring down kai.
func Log(reason string) {
	path, ok := logPath()
	if !ok {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	snap := Capture()
	_, _ = f.WriteString(FormatLine(snap, reason) + "\n")
}

// LogBurst writes one snapshot per delay step, prefixing each
// reason with the running offset (e.g. "ctrl-c+0s", "ctrl-c+5s").
// Used to capture short, high-resolution windows around moments
// likely to spike memory — cancel propagation, large captures,
// session flushes — that the 60s idle sampler will miss. Fire-and-
// forget; returns immediately and runs in its own goroutine.
//
// 2026-05-14 dogfood: Ctrl+C-driven OOM. Idle sampler showed kai
// at 143MB at 05:59; next sample at 06:00 was already a tui-boot
// from the new process. A burst on ctrl-c would have caught what
// happened in that 60s gap.
func LogBurst(reason string, delays ...time.Duration) {
	go func() {
		start := time.Now()
		for _, d := range delays {
			if d > 0 {
				time.Sleep(d)
			}
			off := time.Since(start).Round(time.Second)
			Log(fmt.Sprintf("%s+%ds", reason, int(off.Seconds())))
		}
	}()
}

// StartIdleSampler launches a goroutine that captures a snapshot
// every interval until done is closed. Used by the TUI to catch
// background memory growth during long idle stretches — exactly
// the window where macOS chose kai for OOM kill in the 2026-05-14
// dogfood. Interval below 5s spams the log; 30s-2m is the sane
// range. Returns immediately.
func StartIdleSampler(interval time.Duration, done <-chan struct{}) {
	if interval < time.Second {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				Log("idle")
			}
		}
	}()
}

var logMu sync.Mutex

// logPath returns the destination log path. ~/.kai/memory-stats.log
// matches the existing convention for per-user kai diagnostics
// (planner-debug.log, tui-panic.log lives in the same dir).
func logPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".kai", "memory-stats.log"), true
}
