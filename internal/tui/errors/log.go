package errors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"kai/api/kaipath"
)

// Local error log writer. Every classified error appends one
// JSON-line entry to <kaiDir>/errors.log so devs can `tail -f`
// it. Independent from telemetry — even users with telemetry
// disabled still get the local log (it never leaves their
// machine, and it's the difference between "you said something
// went wrong, can you reproduce?" and "let me see your last 5
// failures right now").
//
// File location: <kaiDir>/errors.log where kaiDir is whatever
// kaipath.Resolve picks (.kai/ or .git/kai/). Best-effort —
// write failures are swallowed; the user-facing operation
// completes regardless of whether the log entry landed.

// errorLogEntry is the on-disk shape. Fields chosen to match
// what telemetry's PostHog event carries so the two stay
// trivially queryable side-by-side.
type errorLogEntry struct {
	Timestamp    string         `json:"ts"`
	KaiVersion   string         `json:"kai_version"`
	OS           string         `json:"os"`
	Arch         string         `json:"arch"`
	Kind         string         `json:"kind"`
	Headline     string         `json:"headline"`
	RawMessage   string         `json:"raw_message"`
	AutoRepaired bool           `json:"auto_repaired"`
	Severity     string         `json:"severity"`
	Context      map[string]any `json:"context,omitempty"`
}

var (
	logMu      sync.Mutex
	kaiVersion = "dev" // overridden via SetVersion
)

// SetVersion records the running kai-cli version so log entries
// carry it. Called from cmd/kai's init (mirrors the existing
// telemetry.SetVersion pattern).
func SetVersion(v string) {
	if v != "" {
		kaiVersion = v
	}
}

// LogLocal appends ue to the local errors.log. workspace is the
// directory used to find the .kai/ data dir. Best-effort.
func LogLocal(workspace string, ue UserError, autoRepaired bool) {
	if ue.Kind == "" || ue.Kind == "none" {
		return
	}
	dir := kaipath.Resolve(workspace)
	if dir == "" {
		return // no kai dir resolvable; skip silently
	}
	// CRITICAL: only write into a kai data directory that ALREADY
	// exists. Eager MkdirAll here used to scatter rogue .kai/
	// directories at unexpected paths — surfaced 2026-05-11 when
	// the banner fix changed workspaceFor to return InvokedFrom
	// (the user's invocation dir). For a multi-root parent like
	// ~/projects/kai, kaipath.Resolve fell through to
	// ~/projects/kai/.kai/ and MkdirAll created it on the first
	// error log write. Logging is best-effort; if no .kai exists
	// at the workspace, just skip rather than fabricate one.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	entry := errorLogEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		KaiVersion:   kaiVersion,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Kind:         ue.Kind,
		Headline:     ue.Headline,
		RawMessage:   ue.LogContext,
		AutoRepaired: autoRepaired,
		Severity:     severityName(ue.Severity),
		Context:      ue.Context,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(dir, "errors.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	_, _ = f.Write([]byte("\n"))
}

func severityName(s Severity) string {
	switch s {
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Block:
		return "block"
	default:
		return "unknown"
	}
}
