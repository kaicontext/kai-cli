package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/kaicontext/kai-engine/drift"
)

const (
	// gitWatchDebounce is the settle window after a burst of git ref
	// activity (a rebase rewrites many refs quickly) before converging.
	gitWatchDebounce = 1 * time.Second
	// gitPollInterval backstops fsnotify: hooks don't travel with clones,
	// editors do odd things to .git, and a missed event would otherwise
	// leave the daemon stale forever. One drift report every 30s is a
	// handful of git rev-parse/merge-base calls — noise-level cost.
	gitPollInterval = 30 * time.Second
)

// driftCatchUpPass converges the graph to git once: checkpointed catch-up
// when the graph is behind or diverged, manifest resync otherwise (a ref
// switch changes the resolution without needing processing). The DB is
// opened only for the duration of a catch-up so the daemon never holds the
// write lock while idle. Failures print and are retried by the next signal
// or poll tick — the daemon self-heals.
func driftCatchUpPass(w io.Writer) {
	rep := computeDriftReport()
	if rep == nil {
		return
	}
	if rep.Relationship != drift.RelBehind && rep.Relationship != drift.RelDiverged {
		syncDriftManifest()
		return
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(w, "  drift: catch-up skipped (db busy: %v)\n", err)
		return
	}
	defer db.Close()

	res, err := catchUpDrift(db, 0, nil)
	if err != nil {
		fmt.Fprintf(w, "  drift: catch-up stopped at last checkpoint (%d done): %v\n", res.Processed, err)
		return
	}
	if res.Processed > 0 {
		after := computeDriftReport()
		head := ""
		if after != nil {
			head = " at " + shortPrefix(after.GitHead)
		}
		fmt.Fprintf(w, "  drift: caught up %s — graph in sync%s\n", countNoun(res.Processed, "commit"), head)
	}
}

// gitPathRelevant reports whether a path inside .git signals that git's
// commit/ref state moved: HEAD and ORIG_HEAD (checkout, commit, merge),
// packed-refs (gc, fetch), anything under refs/. Deliberately excludes the
// index and object churn — staging a file is not drift.
func gitPathRelevant(gitDir, abs string) bool {
	rel, err := filepath.Rel(gitDir, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)
	switch rel {
	case "HEAD", "ORIG_HEAD", "packed-refs":
		return true
	}
	return strings.HasPrefix(rel, "refs/")
}

// watchGitState is the daemon's continuous catch-up loop: fsnotify on
// .git's HEAD and refs plus a slow poll, feeding a debounced single-flight
// worker that runs driftCatchUpPass. Blocks until stop closes.
func watchGitState(stop <-chan struct{}, repoRoot string, out io.Writer) {
	gitDir := filepath.Join(repoRoot, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		// Worktree (.git file) or no repo: poll-only fallback below still
		// works through computeDriftReport, which asks git, not the fs.
		gitDir = ""
	}

	dirty := make(chan struct{}, 1)
	signalDirty := func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}

	var fsw *fsnotify.Watcher
	if gitDir != "" {
		var err error
		fsw, err = fsnotify.NewWatcher()
		if err == nil {
			defer fsw.Close()
			// .git itself (HEAD, ORIG_HEAD, packed-refs live as direct
			// children) plus the refs tree; new ref directories (first
			// branch under refs/heads/feat/) are added as they appear.
			_ = fsw.Add(gitDir)
			refsDir := filepath.Join(gitDir, "refs")
			filepath.WalkDir(refsDir, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					_ = fsw.Add(p)
				}
				return nil
			})
			go func() {
				for {
					select {
					case <-stop:
						return
					case ev, ok := <-fsw.Events:
						if !ok {
							return
						}
						if ev.Op&fsnotify.Create != 0 {
							if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
								_ = fsw.Add(ev.Name)
							}
						}
						if gitPathRelevant(gitDir, ev.Name) {
							signalDirty()
						}
					case <-fsw.Errors:
						// non-fatal; the poll backstop covers gaps
					}
				}
			}()
		}
	}

	ticker := time.NewTicker(gitPollInterval)
	defer ticker.Stop()

	// Single-flight worker inline: debounce a burst, run one pass at a time.
	signalDirty() // initial convergence so the daemon starts honest
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			signalDirty()
		case <-dirty:
			t := time.NewTimer(gitWatchDebounce)
		settle:
			for {
				select {
				case <-stop:
					t.Stop()
					return
				case <-dirty:
					t.Reset(gitWatchDebounce)
				case <-t.C:
					break settle
				}
			}
			driftCatchUpPass(out)
		}
	}
}
