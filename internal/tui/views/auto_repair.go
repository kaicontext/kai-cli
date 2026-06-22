// Auto-repair for classifier-tagged errors. The errors package
// returns a UserError whose Kind names a known recoverable
// failure (e.g. "preflight.missing_blobs" — the snapshot
// references blobs that aren't in the object store anymore).
// This file owns the side of that contract that actually does
// the recovery: a tea.Cmd that runs the fix in the background
// and emits a typed Msg when finished.
//
// Without this wiring the UserError.AutoRepair field was a
// dead promise — the message "Reindexing the workspace…"
// rendered to the user but nothing was actually reindexing.
// The May-5 dogfood session caught it: "does it ever finish
// rebuilding the workspace?"
package views

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// AutoRepairDoneMsg lands when a background recovery finishes
// (success or failure). The REPL handler clears the transient
// "Reindexing…" line and writes a brief outcome line to the
// scrollback.
type AutoRepairDoneMsg struct {
	// Kind mirrors the UserError.Kind that triggered the
	// repair — e.g. "preflight.missing_blobs" or
	// "preflight.no_snapshots".
	Kind string
	// Err is non-nil if the repair itself failed. The repair
	// tried to fix a problem; if it can't, we have to be
	// honest about it.
	Err error
	// Output is the captured stdout+stderr from the repair
	// command, trimmed. Surfaced only on failure (success
	// just shows the headline).
	Output string
	// Elapsed is how long the repair took. Surfaced so a
	// suspiciously long repair stands out.
	Elapsed time.Duration
}

// runAutoRepair returns a tea.Cmd that runs the recovery for
// the given UserError kind. Currently handles
// "preflight.missing_blobs" by shelling out to `kai capture`
// in the workspace root — same path the user would take
// manually, kept that way deliberately so behavior is easy to
// reason about.
//
// Returns nil for unknown kinds. The caller should also nil-
// guard before invoking this as a tea.Cmd — Bubble Tea is
// fine with nil cmds in tea.Batch.
func runAutoRepair(s *PlannerServices, kind string) tea.Cmd {
	if s == nil || s.MainRepo == "" {
		return nil
	}
	if !isAutoRepairableKind(kind) {
		return nil
	}
	return autoRepairCapture(s, kind)
}

// isAutoRepairableKind reports whether a UserError.Kind is handled
// by background auto-repair (i.e. workspace infrastructure that
// kai recovers from automatically without user intervention).
// Shared with planner_memory.go: failures in this set must NOT be
// recorded as "PRIOR PLAN EXECUTION FAILED" because they're
// orthogonal to whether the plan was correct — the workspace
// just needed reindexing.
func isAutoRepairableKind(kind string) bool {
	switch kind {
	case "preflight.missing_blobs", "preflight.no_snapshots":
		// Both failure modes are fixed by `kai capture`:
		// missing_blobs rebuilds object-store entries from the
		// working tree; no_snapshots creates the first snapshot
		// the orchestrator's spawn step needs as a base.
		return true
	default:
		return false
	}
}

// autoRepairMissingBlobs runs `kai capture` to rebuild the
// object store from the working tree (the fix for "snapshot
// references a blob the object store doesn't have anymore" —
// typically caused by a wiped `.kai/objects/` or a snapshot
// from a deleted branch).
//
// Multi-root awareness: when MainRepo is a container directory
// holding sibling project roots (the user's `~/projects/kai`
// case), running `kai capture` at the container level either
// captures nothing meaningful or smushes everything into one
// fake project. So we capture each Project root individually
// when Projects is set with >1 entry. Single-root workspaces
// keep the original behavior (capture in MainRepo).
//
// Bounded per-root at 60s; total wall-clock can be N*60s in
// the worst case but in practice each root finishes in seconds.
// We capture sequentially rather than in parallel to avoid
// stepping on shared object-store paths if any root happens to
// share a `.kai/` (rare but harmless to be conservative).
func autoRepairMissingBlobs(s *PlannerServices) tea.Cmd {
	return autoRepairCapture(s, "preflight.missing_blobs")
}

func autoRepairCapture(s *PlannerServices, kind string) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		binary, err := os.Executable()
		if err != nil || binary == "" {
			return AutoRepairDoneMsg{
				Kind:    kind,
				Err:     fmt.Errorf("locating kai binary: %w", err),
				Elapsed: time.Since(start),
			}
		}
		msgLabel := "auto-repair: missing blobs"
		if kind == "preflight.no_snapshots" {
			msgLabel = "auto-repair: initial snapshot"
		}

		// Pick the set of directories to capture in.
		// Prefer per-root capture in a multi-root workspace;
		// fall back to MainRepo otherwise.
		var dirs []string
		if s.Projects != nil && len(s.Projects.Projects()) > 1 {
			for _, p := range s.Projects.Projects() {
				if p == nil || p.Path == "" {
					continue
				}
				dirs = append(dirs, p.Path)
			}
		}
		if len(dirs) == 0 {
			dirs = []string{s.MainRepo}
		}

		var firstErr error
		var allOut strings.Builder
		for _, dir := range dirs {
			// 60s used to be the cap; that turned out to be
			// right at the edge for moderate Go projects
			// (~272 files = ~55s of analysis). The May-2026
			// repro showed capture hitting the timeout mid-
			// analysis and the user reading "auto-repair
			// failed" when it was actually still progressing.
			// 5min is generous enough to cover cold-cache
			// captures of large repos (the kai monorepo
			// itself takes ~2min cold) without making the
			// happy-path wait noticeably longer (it still
			// returns the moment capture exits).
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			c := exec.CommandContext(ctx, binary, "capture", "-m", msgLabel)
			c.Dir = dir
			out, runErr := c.CombinedOutput()
			cancel()

			label := dir
			if rel, rErr := filepath.Rel(s.MainRepo, dir); rErr == nil && rel != "" && !strings.HasPrefix(rel, "..") {
				label = rel
			}
			if runErr != nil && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", label, runErr)
			}
			fmt.Fprintf(&allOut, "── %s ──\n%s\n", label, strings.TrimSpace(string(out)))
		}

		return AutoRepairDoneMsg{
			Kind:    kind,
			Err:     firstErr,
			Output:  strings.TrimSpace(allOut.String()),
			Elapsed: time.Since(start),
		}
	}
}
