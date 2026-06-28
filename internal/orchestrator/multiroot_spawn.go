package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/ignore"
	"kai/internal/projects"
)

// Multi-root spawn materialization.
//
// Background: in a multi-root workspace (kai.projects.yaml lists more
// than one project), the user often invokes `kai code` from inside
// one sub-project but describes work that touches sibling projects.
// E.g. running `kai code` inside ~/projects/kai/kai-server and asking
// the agent to fix a bug in kai-cli. The planner correctly generates
// task paths like "Kai/kai-cli/internal/tui/views/planner_dispatch.go"
// (using the project NAME as a prefix, per the planner's prompt
// convention).
//
// The single-root spawnFor materializes only the user's primary
// project into the spawn dir, so the agent's file tools can find
// "kai-cli/..." (the project root) but not "Kai/kai-cli/..."
// (sibling-relative). The agent grinds through kai_tree / kai_files
// calls trying to find the prefixed path, fails, gives up empty.
//
// spawnForMulti is the fix. It walks every project in the supplied
// projects.Set, materializes each via `kai checkout @snap:last`
// into a name-stamped subdir of the spawn root, and writes a
// kai.projects.yaml in the spawn root so the agent's tool layer
// re-discovers the same layout. The returned projects.Set has each
// project's Path rewritten to point inside the spawn dir (so the
// file tools' ByName lookup resolves to the spawn copy, not the
// original source repo).
//
// Write-back is now multi-root too (Phase B, 2026-05-29). Edits in
// <spawn>/<sibling>/... DO propagate back: integrateOneAgent absorbs
// every touched project into its own real dir, captures a snapshot in
// each project's graph, classifies + gates per project, rolls back
// snap.latest per project (all-or-nothing) on a non-Auto verdict, and
// build-checks + build-fixes each touched project against its own
// pre-absorb baseline. The historical "Phase A: spawn only, absorb-back
// single-root" caveat that used to live here was stale and has been
// removed. The remaining primary-only step is plan-coverage (computed
// once against the aggregate change set).

// spawnForMulti is the multi-root counterpart of spawnFor. Called
// when cfg.Projects has >1 entry. Materializes every project into
// <spawnDir>/<project.Name>/ via shell-out to `kai checkout`, then
// returns the spawn dir, workspace name, and a rewritten
// projects.Set whose paths point inside the spawn dir.
//
// Single-project workspaces (len(set.Projects()) <= 1) should NEVER
// reach this function — caller is responsible for the dispatch.
//
// Failure semantics: per-project checkout failures abort the whole
// spawn and return the error. We don't partially materialize — an
// incomplete spawn would surface as a confusing "agent can find
// kai-cli but not kai-server" failure mode that's harder to
// diagnose than a clean error message at startup.
func spawnForMulti(
	ctx context.Context,
	taskName string,
	cfg Config,
	set *projects.Set,
) (spawnDir, wsName string, rewritten *projects.Set, err error) {
	if set == nil || len(set.Projects()) < 2 {
		return "", "", nil, fmt.Errorf("spawnForMulti: requires multi-root set (got %d projects)",
			len(set.Projects()))
	}

	spawnDir = fmt.Sprintf("%s%s-%d", cfg.SpawnPrefix, taskName, time.Now().UnixNano())
	if err := os.MkdirAll(spawnDir, 0o755); err != nil {
		return "", "", nil, fmt.Errorf("creating spawn dir: %w", err)
	}

	// Build the rewritten set as we go. Each project's Path is
	// remapped to its subdir under the spawn root; Name + KaiDir
	// follow along for consistency.
	//
	// Failure semantics are PARTIAL-PROGRESS, not all-or-nothing. Any
	// single project's checkout failure (no snapshots, permission
	// denied, locked DB, transient I/O, bad ref) drops THAT project
	// from the spawn and the loop continues. Reasoning: the user's
	// task usually targets ONE project; failing the whole agent run
	// because a sibling has a stale .kai is exactly the user-hostile
	// pattern we're trying to avoid (observed 2026-05-12 when an
	// empty kai-tui aborted a "rename default model in kai-cli"
	// plan). We only return an error when the user's PRIMARY project
	// failed AND no other projects materialized — at that point we
	// genuinely have nothing to give the agent.
	rewrittenProjects := make([]*projects.Project, 0, len(set.Projects()))
	type skipReason struct {
		name   string
		reason string
	}
	var skipped []skipReason
	primaryName := ""
	if p := set.Primary(); p != nil {
		primaryName = p.Name
	}
	primaryMaterialized := false

	for _, p := range set.Projects() {
		// Skip projects that don't have a checked-out source on
		// disk — they can't contribute a snapshot. Discover()
		// already drops these but defense-in-depth here so a
		// stale yaml entry doesn't fail the whole spawn.
		if _, statErr := os.Stat(p.Path); statErr != nil {
			skipped = append(skipped, skipReason{name: p.Name, reason: "path not on disk"})
			continue
		}

		// Use the on-disk directory basename, NOT p.Name, for the
		// spawn subdir. The 2026-05-12 dogfood pinned why: a project
		// whose yaml says `name: Kai` but lives at `path: .` (which
		// is the on-disk dir `kai/`) used to spawn into `<spawn>/Kai/`.
		// The snapshot then carried `Kai/` paths, and apply-back to
		// the parent workspace CREATED a sibling `Kai/` tree alongside
		// the existing `kai/`, hollowing out the original. Using the
		// resolved absolute path's basename keeps the spawn layout
		// byte-identical to the workspace layout, so snapshot-apply
		// overwrites in place instead of duplicating.
		subdirName := projectDirBasename(p)
		subdir := filepath.Join(spawnDir, subdirName)
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			skipped = append(skipped, skipReason{name: p.Name, reason: "mkdir failed: " + err.Error()})
			continue
		}

		// Materialize the spawn as a direct copy of the project's
		// LIVE working tree — not a `kai checkout` of the latest
		// graph snapshot.
		//
		// The snapshot-checkout approach was the root cause of the
		// 2026-05-15 class of agent failures: when the semantic
		// graph carries File nodes from multiple capture generations
		// (the path-prefix accumulation bug — the same file indexed
		// under "kai-cli/..." and "Kai/kai-cli/..." with divergent
		// digests), `kai checkout` reconstructs a MIXED-ERA tree.
		// Observed concretely: a spawn with a pre-move
		// internal/mcp/server.go (importing kai/internal/synclog)
		// sitting next to a post-move pkg/synclog/ — a combination
		// that never existed as a real commit and does not compile.
		// Every agent run then died at its `go build` VERIFY step
		// through no fault of its own.
		//
		// A direct copy cannot be stale or internally inconsistent:
		// the spawn is byte-for-byte the developer's current files,
		// so VERIFY reflects reality and the absorb step's
		// spawn-vs-main diff is exactly the agent's edits.
		if err := copyProjectTree(p.Path, subdir); err != nil {
			skipped = append(skipped, skipReason{name: p.Name, reason: "copy failed: " + err.Error()})
			_ = os.RemoveAll(subdir)
			continue
		}

		// Capture the rewritten project. Path now points INSIDE the
		// spawn dir; the agent's file tools (which do path lookups
		// via set.ByName + filepath.Join(proj.Path, rest)) will
		// route to the spawn copy.
		rewrittenProjects = append(rewrittenProjects, &projects.Project{
			Path:   subdir,
			Name:   p.Name,
			Pinned: p.Pinned,
			// KaiDir / DB / GateConfig: deliberately NOT inherited.
			// The spawn copy is a fresh working tree; it has no
			// .kai database yet. The agent's read-only kai_*
			// tools route their graph queries via opts.Graph
			// (cfg.MainGraph), which is the SOURCE repo's graph
			// — that's still correct. File ops use Workspace +
			// the rewritten Path, which is correct.
		})
		if p.Name == primaryName {
			primaryMaterialized = true
		}
	}

	// Emit per-project skip notes as warnings. The orchestrator
	// surfaces these to the user so they understand why a sibling
	// project isn't reachable from the agent — silence here would
	// produce the "agent claims X doesn't exist" failure mode that
	// motivated the cross-project visibility work in the first
	// place. Stderr (not stdout) so non-TUI callers can grep cleanly.
	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "spawn (multi-root): skipping %q — %s\n", s.name, s.reason)
	}

	// Hard-fail only when there's genuinely nothing to give the
	// agent. Two paths to that state:
	//   1. Every project failed (the loop populated 0 entries).
	//   2. The user's PRIMARY project failed — even if siblings
	//      succeeded, the agent's task usually centers on the
	//      primary; running with only siblings would produce
	//      misleading results.
	if len(rewrittenProjects) == 0 {
		return "", "", nil, fmt.Errorf("multi-root spawn: every project failed to materialize — see warnings above")
	}
	if primaryName != "" && !primaryMaterialized {
		return "", "", nil, fmt.Errorf("multi-root spawn: primary project %q failed to materialize — see warnings above", primaryName)
	}

	// Write kai.projects.yaml at the spawn root. Mirrors the original
	// yaml so a re-Discover from inside the spawn dir produces the
	// same multi-root view. Paths are name-based (matching the
	// subdir layout), not absolute, so the file stays portable.
	if err := writeSpawnProjectsYAML(spawnDir, rewrittenProjects); err != nil {
		return "", "", nil, fmt.Errorf("writing spawn projects.yaml: %w", err)
	}

	rewritten = projects.New(spawnDir, rewrittenProjects)
	// Workspace name follows the same convention as single-root
	// spawn — "spawn-1" — so the despawn / integrate paths can
	// find it without special-casing the multi-root layout.
	return spawnDir, "spawn-1", rewritten, nil
}

// projectDirBasename returns the spawn-safe directory name to use
// for a project. Always derived from the resolved on-disk path's
// basename — NEVER from p.Name. p.Name is human-readable and may
// drift from the on-disk casing (e.g. `name: Kai`, `path: .` where
// `.` is `kai/` on disk), which would cause the spawn snapshot to
// carry a different path than the workspace and turn apply-back
// into a duplicate-tree creation rather than an in-place overwrite.
//
// Resolves `.` and relative paths via filepath.Abs so the basename
// is always meaningful. Falls back to sanitizeSubdir(p.Name) only
// when the path is genuinely unresolvable, which shouldn't happen
// in practice (the caller has already stat'd p.Path) but covers
// the test-fixture case where Path is empty.
func projectDirBasename(p *projects.Project) string {
	if p != nil && p.Path != "" {
		abs, err := filepath.Abs(p.Path)
		if err == nil {
			base := filepath.Base(abs)
			if base != "" && base != "." && base != "/" {
				return sanitizeSubdir(base)
			}
		}
	}
	if p != nil {
		return sanitizeSubdir(p.Name)
	}
	return "project"
}

// sanitizeSubdir turns a project name into a safe directory name.
// Most names are already fine (Kai, kai-cli, kai-server-2) but
// names with spaces ("Kai Server") would produce ugly paths in
// shell output and trip up tools that don't quote properly.
// Replaces runs of whitespace and `/` with single underscores.
func sanitizeSubdir(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range name {
		switch {
		case r == ' ' || r == '\t' || r == '/' || r == '\\':
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		default:
			b.WriteRune(r)
			prevUnderscore = false
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if out == "" {
		out = "project"
	}
	return out
}

// writeSpawnProjectsYAML writes a minimal kai.projects.yaml at the
// spawn dir's root. Format matches projects.SaveFile (key order,
// indent) so a re-load via projects.LoadFile produces the same
// in-memory shape. Paths are relative to the spawn dir.
func writeSpawnProjectsYAML(spawnDir string, ps []*projects.Project) error {
	path := filepath.Join(spawnDir, projects.ProjectsFileName)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, "projects:\n"); err != nil {
		return err
	}
	for _, p := range ps {
		rel, err := filepath.Rel(spawnDir, p.Path)
		if err != nil {
			rel = p.Path
		}
		fmt.Fprintf(f, "    - path: %s\n", rel)
		fmt.Fprintf(f, "      name: %s\n", p.Name)
	}
	return nil
}

// isNoSnapshotsCheckoutError matches the kai-checkout error returned
// when a project has never been captured (no snapshots in its
// graph). Retained as a named helper so summarizeCheckoutError can
// give a meaningfully-different message for the empty-project case
// versus other failures — the user-visible reason matters here
// because "no captures yet" is actionable (run kai capture) while
// "permission denied" is a different problem entirely.
func isNoSnapshotsCheckoutError(combined string) bool {
	return strings.Contains(combined, "not found: @snap:last") ||
		strings.Contains(combined, "no snapshots")
}

// summarizeCheckoutError turns the kai-checkout stderr noise into a
// one-line human-readable reason suitable for a skip warning. The
// raw combined output can be hundreds of bytes of Go panic-trace,
// transient retry chatter, or json-encoded resolver state — none of
// it useful in a "skipping kai-tui because..." line.
//
// Classification order: most-specific first, then a truncated raw
// fallback so we never lose information entirely for a failure
// mode we haven't categorized yet.
func summarizeCheckoutError(combined string, raw error) string {
	switch {
	case isNoSnapshotsCheckoutError(combined):
		return "no captures yet (run `kai capture` in that project)"
	case strings.Contains(combined, "permission denied"):
		return "permission denied reading project state"
	case strings.Contains(combined, "database is locked"):
		return "kai database locked — another kai process may be running"
	case strings.Contains(combined, "context canceled"):
		return "canceled"
	}
	// Truncate raw output to a single line of bounded length so a
	// 4KB panic-trace doesn't blow up the warning area.
	first := combined
	if nl := strings.IndexByte(first, '\n'); nl >= 0 {
		first = first[:nl]
	}
	if len(first) > 200 {
		first = first[:197] + "..."
	}
	if first == "" {
		return fmt.Sprintf("checkout failed: %v", raw)
	}
	return first
}

// runCheckout shells out to `kai checkout @snap:last --dir <subdir>`
// from the project's source path. Extracted so the spawn loop can
// retry after a self-heal (`kai capture` on a no-captures project)
// without duplicating the exec.Command boilerplate.
func runCheckout(ctx context.Context, cfg Config, projectPath, subdir string) ([]byte, error) {
	c := exec.CommandContext(ctx, kaiBinary(cfg), "checkout", "@snap:last", "--dir", subdir)
	c.Dir = projectPath
	return c.CombinedOutput()
}

// copyProjectTree recursively copies the LIVE working tree of src
// into dst, applying the same ignore rules `kai capture` uses
// (.kai/, .git/, node_modules/, plus .gitignore / .kaiignore). It is
// the spawn-materialization strategy that replaced `kai checkout
// @snap:last`.
//
// Why a copy and not a snapshot checkout: a checkout reconstructs the
// tree from the semantic graph, which can carry File nodes from
// several capture generations at once (the path-prefix accumulation
// bug). The reconstruction is then a mix of eras that may not
// compile. A direct copy is, by construction, exactly what is on
// disk — so the agent's build/test VERIFY step is meaningful and the
// absorb diff is precisely the agent's edits.
//
// Symlinks are skipped (we cannot safely reproduce them into a fresh
// tree and the agent does not need them). Per-file mode bits are
// preserved via copyFileForAbsorb so executables stay executable.
func copyProjectTree(src, dst string) error {
	matcher, err := ignore.LoadFromDir(src)
	if err != nil || matcher == nil {
		matcher = ignore.NewMatcher(src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if shouldIgnoreObserver(rel+"/") || rel == ".kai" || rel == ".git" || rel == "node_modules" {
				return filepath.SkipDir
			}
			if matcher != nil && matcher.Match(rel, true) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if shouldIgnoreObserver(rel) {
			return nil
		}
		if matcher != nil && matcher.Match(rel, false) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		return copyFileForAbsorb(path, filepath.Join(dst, rel))
	})
}

// runAutoCapture creates an initial snapshot for a project that's
// never been captured. Same shell-out the TUI auto-repair runs for
// the "preflight.no_snapshots" classified error, just inlined here
// so the multi-root spawn can self-heal mid-flight without
// surfacing the failure to the user.
//
// Bounded timeout: 2 minutes. Empty / placeholder projects (the
// kai-tui case that drove this) capture in well under a second;
// real projects with content may take longer but anything past 2
// minutes is almost certainly a misconfiguration the user needs
// to see, not a transient.
//
// Output is discarded — the spawn loop's caller already logs a
// readable summary if the retry-after-capture succeeds.
func runAutoCapture(ctx context.Context, cfg Config, projectPath string) error {
	capCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(capCtx, kaiBinary(cfg), "capture", "-m", "auto-repair: initial snapshot")
	c.Dir = projectPath
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("kai capture: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
