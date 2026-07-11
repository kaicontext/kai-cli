package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/contract"
	"github.com/kaicontext/kai-engine/dirio"
	"github.com/kaicontext/kai-engine/ignore"
	"github.com/kaicontext/kai-engine/verify"
)

const (
	structuralTimeout = 2 * time.Minute
	// watchDebounce is the settle window after a burst of edits before the
	// deterministic checks re-run. A dogfood number, not a guess (open design
	// call in the spec) — start at 2s and tune.
	watchDebounce = 2 * time.Second

	// Semantic auto-trigger heuristic (Phase 3). The expensive graph-grounded
	// verify runs automatically only when it's worth it: structural green +
	// a material change since the last verify + rate-limited — EXCEPT the
	// first eligible run of a session is never throttled, so even a short
	// session gets one verify (don't miss the aha moment). All dogfood-tunable.
	semMinNewChangedLines = 8
	semThrottle           = 5 * time.Minute
)

// semWatchState tracks, per watch session, what was verified last so the
// heuristic can measure material change without an LLM.
type semWatchState struct {
	seen map[string]bool // changed-lines present at the last semantic run
	last time.Time       // zero = no semantic run yet this session
}

var (
	watchOff  bool
	watchOnce bool
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Run the continuous deterministic verification daemon for this session",
	Long: "Watches the working tree and re-runs the deterministic checks " +
		"(typecheck, tests) after each burst of edits settles, maintaining the " +
		"live structural verdict shown by 'kai status'. No LLM, no tokens. " +
		"Ctrl-C to stop. Use --once for a single pass.",
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().BoolVar(&watchOnce, "once", false, "run a single deterministic pass and exit")
	watchCmd.Flags().BoolVar(&watchOff, "off", false, "no-op (watch is foreground; Ctrl-C to stop)")
	rootCmd.AddCommand(watchCmd)
}

// runStructuralPass runs the deterministic checks once and applies the result
// to the global structural state and every in-flight contract. Shared by the
// daemon loop and `kai watch --once`. This is the only writer of verdicts in
// Phase 2 — it can never write `verified` (see verify.Verdict).
func runStructuralPass(ctx context.Context, store *contract.Store, dir string) (contract.CheckResult, error) {
	cr := verify.Continuous(ctx, dir, structuralTimeout)
	if err := store.SaveStructural(cr); err != nil {
		return cr, err
	}
	cs, err := store.List()
	if err != nil {
		return cr, err
	}
	for _, c := range cs {
		if c.Closed {
			continue
		}
		c.Continuous = cr
		c.Status = verify.Verdict(cr, true)
		if err := store.Save(c); err != nil {
			return cr, err
		}
	}
	return cr, nil
}

func runWatch(cmd *cobra.Command, args []string) error {
	if watchOff {
		fmt.Println("kai watch runs in the foreground; press Ctrl-C to stop it")
		return nil
	}
	store, err := contract.Open(kaiDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// fsnotify reports resolved (absolute, symlink-followed) paths, so the
	// watch root must match — resolve symlinks (macOS /var → /private).
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if resolved, rerr := filepath.EvalSymlinks(wd); rerr == nil {
		wd = resolved
	}

	if watchOnce {
		driftCatchUpPass(os.Stdout)
		cr, err := runStructuralPass(context.Background(), store, wd)
		if err != nil {
			return err
		}
		printStructuralLine(cr)
		return nil
	}

	// Lightweight, graph-independent file watch: the verify daemon only needs
	// a "something source-y changed → re-run checks" signal. (The shared
	// internal/watcher couples its change signal to graph updates and goes
	// silent in a repo with no snapshot — wrong for this.)
	matcher, _ := ignore.LoadFromDir(wd)
	skipDir := func(rel string) bool {
		base := filepath.Base(rel)
		if base == ".kai" || base == ".git" {
			return true
		}
		return rel != "." && matcher != nil && matcher.MatchSemantic(rel, true)
	}
	relOf := func(abs string) string {
		r, err := filepath.Rel(wd, abs)
		if err != nil {
			return ""
		}
		return filepath.ToSlash(r)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("starting watcher: %w", err)
	}
	defer fsw.Close()

	addTree := func(root string) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			rel := relOf(p)
			if rel == "" {
				return nil
			}
			if skipDir(rel) {
				return filepath.SkipDir
			}
			_ = fsw.Add(p)
			return nil
		})
	}
	addTree(wd)

	sem := &semWatchState{}
	fire := func() {
		cr, err := runStructuralPass(context.Background(), store, wd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			return
		}
		printStructuralLine(cr)
		maybeAutoSemantic(context.Background(), store, cr, sem)
	}

	stop := make(chan struct{})
	dirty := make(chan struct{}, 1)
	signalDirty := func() {
		select {
		case dirty <- struct{}{}:
		default: // a pass is already pending — coalesce
		}
	}

	// fsnotify reader: filter to source changes, signal "dirty" (coalesced).
	go func() {
		for {
			select {
			case <-stop:
				return
			case ev, ok := <-fsw.Events:
				if !ok {
					return
				}
				rel := relOf(ev.Name)
				if rel == "" || strings.HasPrefix(rel, ".kai/") || strings.HasPrefix(rel, ".git/") {
					continue
				}
				// Ignore pure CHMOD/metadata events. A read (e.g. each test pass
				// stat-ing a source file) emits CHMOD on macOS, which would feed
				// back into the watcher and re-trigger forever. Only real content
				// changes count.
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				if matcher != nil && matcher.MatchSemantic(rel, false) {
					continue
				}
				if ev.Op&fsnotify.Create != 0 {
					if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
						addTree(ev.Name)
						continue
					}
				}
				if dirio.DetectLang(ev.Name) == "" {
					continue
				}
				signalDirty()
			case <-fsw.Errors:
				// non-fatal
			}
		}
	}()

	// Serial worker: debounce a burst, then run exactly ONE pass at a time.
	// Edits during a (long) semantic run coalesce into a single follow-up pass
	// — never concurrent runs (which would fan out redundant LLM verifies).
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-dirty:
				t := time.NewTimer(watchDebounce)
			settle:
				for {
					select {
					case <-stop:
						t.Stop()
						return
					case <-dirty:
						t.Reset(watchDebounce)
					case <-t.C:
						break settle
					}
				}
				fire()
			}
		}
	}()

	// Continuous graph↔git catch-up: watches .git HEAD/refs (plus a slow
	// poll backstop) and converges the graph as commits land, so queries
	// stop paying the inline catch-up cost while the daemon runs.
	go watchGitState(stop, wd, os.Stdout)

	fmt.Println("kai watch · deterministic daemon live (Ctrl-C to stop)")
	fire() // initial pass so status is populated immediately

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	close(stop)
	fmt.Println("\nwatch stopped")
	return nil
}

func printStructuralLine(cr contract.CheckResult) {
	switch {
	case cr.TestsPass == nil:
		fmt.Println("  structural: inconclusive (no test convention detected)")
	case *cr.TestsPass:
		fmt.Println("  structural: ✓ typecheck + tests pass")
	default:
		fmt.Println("  structural: ✗ broken")
		for _, f := range cr.Failures {
			fmt.Printf("    %s\n", firstLine(f))
		}
	}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// maybeAutoSemantic applies the Phase-3 heuristic after a deterministic pass:
// run the expensive graph-grounded semantic verify only when it's worth it, and
// always report WHY it did or didn't, so the daemon is never silently passive.
func maybeAutoSemantic(ctx context.Context, store *contract.Store, cr contract.CheckResult, st *semWatchState) {
	// Gate 1 — green only. No point semantically verifying code that won't
	// build or fails tests. Report the skip so the user knows why.
	if cr.TestsPass == nil {
		return // inconclusive (no test convention) — stay quiet
	}
	if !*cr.TestsPass {
		fmt.Println("  semantic: skipped — structural broken, fix that first")
		return
	}

	kit := kitBinary()
	if kit == "" {
		return // no graph-grounded verifier available
	}

	// Gate 2 — material change since the last semantic run. Deterministic, no
	// LLM: count changed diff lines not present at the previous verify.
	cur := changedLines(workingTreeDiff(maxSemanticDiffBytes))
	newCount := 0
	for _, l := range cur {
		if !st.seen[l] {
			newCount++
		}
	}
	if newCount < semMinNewChangedLines {
		fmt.Println("  semantic: skipped — no material change since last verify")
		return
	}

	// Gate 3 — rate limit, but never throttle the first run of the session.
	if !st.last.IsZero() && time.Since(st.last) < semThrottle {
		fmt.Printf("  semantic: throttled — next eligible in %s\n", (semThrottle - time.Since(st.last)).Round(time.Second))
		return
	}

	cs, err := store.List()
	if err != nil {
		return
	}
	var inflight []*contract.Contract
	for _, c := range cs {
		if !c.Closed {
			inflight = append(inflight, c)
		}
	}
	if len(inflight) == 0 {
		return
	}

	if st.last.IsZero() {
		fmt.Println("  semantic: running (first verify this session)…")
	} else {
		fmt.Println("  semantic: running…")
	}
	for _, c := range inflight {
		if err := semanticVerify(ctx, kit, store, c, cr); err != nil {
			fmt.Fprintf(os.Stderr, "  semantic: %v\n", err)
		}
	}

	// Snapshot what we just verified, for the next material-change check.
	st.seen = make(map[string]bool, len(cur))
	for _, l := range cur {
		st.seen[l] = true
	}
	st.last = time.Now()
}
