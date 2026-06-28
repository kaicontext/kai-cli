package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"kai/internal/agent"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/kaipath"
	"kai/internal/snapshot"
)

// build_fix.go: recovery layer for build-check failures.
//
// The orchestrator's per-agent integrate path runs `go build ./...` (or
// the ecosystem equivalent) after the worker's edits are captured into
// the main repo. Before this file existed, a build failure terminated
// the integrate with IntegrateErr set — leaving the user's working
// tree containing the worker's broken edits, no way to recover except
// manual file restoration. That's the "the system caught it but you're
// still broken" experience that erodes trust in the loop.
//
// This file provides two things:
//
//   1. restoreWorkingTreeToSnapshot — the safety net. When all recovery
//      attempts exhaust, restore mainRepo to its pre-worker state by
//      checking out the previous snap.latest. The user's tree always
//      compiles after a kai run — either with new edits that built
//      clean, or with their pre-run tree intact.
//
//   2. truncateBuildOutputForSurface — render the first N lines of a
//      failing build's stderr/stdout inline so the user sees the actual
//      compiler error in the TUI without having to chase the agent.log
//      file path. Full output remains available in the log; the
//      surface message gives the gist.
//
// A future revision will add buildFixLoop() — an auto-fix agent that
// reads the build error and patches the working tree, retrying the
// build between rounds. The hook point for that is exactly where
// integrateOneAgent currently calls restoreWorkingTreeToSnapshot: try
// the fix first, fall back to restore only if the fix exhausts.

// surfaceOutputMaxLines caps the number of stderr/stdout lines we
// embed into the gate's user-facing failure reason. The full output
// stays in the planner-debug.log; the surface line is for "what did
// the compiler say?" at a glance. 50 is enough for one typical Go
// compile error stanza with its file:line and context lines, while
// staying small enough that the planner-debug log entry doesn't
// blow past the truncate cap in DebugLog.Routing/Tool.
const surfaceOutputMaxLines = 50

// truncateBuildOutputForSurface returns the first N lines of out,
// joined by newline, with a trailing "(M more lines in <log>)" hint
// when the output was truncated. Used to compose the inline error
// message the user sees in the TUI without forcing them to open the
// agent log file. Empty input returns empty string.
func truncateBuildOutputForSurface(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) <= surfaceOutputMaxLines {
		return strings.Join(lines, "\n")
	}
	head := lines[:surfaceOutputMaxLines]
	more := len(lines) - surfaceOutputMaxLines
	return strings.Join(head, "\n") + fmt.Sprintf("\n…(%d more lines suppressed; full output in agent.log)", more)
}

// restoreWorkingTreeToSnapshot rolls back targetDir to the state of
// the named snapshot. Used after a terminal failure (build-fix loop
// exhausted, agent error, etc.) to ensure the user's working tree
// never carries broken edits from a kai run that didn't succeed.
//
// Returns an error if the snapshot is missing or checkout fails;
// callers should log this but not treat it as fatal — even a partial
// restore is better than leaving a broken tree, and surfacing the
// restore failure inline tells the user to manually inspect.
func restoreWorkingTreeToSnapshot(db *graph.DB, snapID []byte, targetDir string) error {
	if db == nil {
		return fmt.Errorf("restoreWorkingTreeToSnapshot: db is nil")
	}
	if len(snapID) == 0 {
		return fmt.Errorf("restoreWorkingTreeToSnapshot: snapID is empty")
	}
	if targetDir == "" {
		return fmt.Errorf("restoreWorkingTreeToSnapshot: targetDir is empty")
	}
	creator := snapshot.NewCreator(db, nil)
	// clean=true: also delete files NOT in the snapshot, so any files
	// the worker created from scratch are removed. Otherwise a failed
	// run that added new files (e.g. kai_web_search.go in a workspace
	// without it pre-run) leaves orphans on disk.
	if _, err := creator.Checkout(snapID, targetDir, true); err != nil {
		return fmt.Errorf("checkout failed: %w", err)
	}
	return nil
}

// formatBuildRegressionReason composes the user-facing reason string
// for a terminal build failure the CHANGE introduced. The first line is
// a stable, human lede ("The change broke the build.") that the TUI's
// error classifier matches on to render this as a clear gate block —
// NOT the generic "Something unexpected happened / file a bug" framing.
// It then names the newly-failing packages (the delta vs the pre-run
// baseline), the inline build excerpt, and a rollback note so the user
// knows their tree is safe.
//
// Goes into the held snapshot's gateReasons so the gate review and
// `kai gate diff <id>` surface it consistently.
func formatBuildRegressionReason(bc buildCheckResult, baseline buildCheckResult, rollbackErr error) string {
	var b strings.Builder
	b.WriteString("The change broke the build.")

	if nf := newFailures(baseline, bc); len(nf) > 0 {
		pkgs := make([]string, 0, len(nf))
		for p := range nf {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		b.WriteString("\n\nNewly failing after this change:")
		for _, p := range pkgs {
			b.WriteString("\n  - " + p)
		}
	}

	if excerpt := truncateBuildOutputForSurface(bc.Output); excerpt != "" {
		b.WriteString("\n\n")
		b.WriteString(excerpt)
	}
	if rollbackErr != nil {
		b.WriteString("\n\nWARNING: working tree rollback also failed: ")
		b.WriteString(rollbackErr.Error())
		b.WriteString("\nInspect the working tree manually before running another agent.")
	} else {
		b.WriteString("\n\nWorking tree was restored to its pre-run state; no broken edits remain on disk.")
	}
	return b.String()
}

// runBuildCheckWithContext is a thin wrapper around runBuildCheck that
// also threads ctx — currently identical to runBuildCheck but kept as
// a seam so the future fix-loop can pass a per-attempt timeout.
func runBuildCheckWithContext(ctx context.Context, repoDir string) buildCheckResult {
	return runBuildCheck(ctx, repoDir)
}

// maxBuildFixRounds caps how many times we attempt to auto-fix a
// failing build before giving up and rolling the working tree back.
// Three matches the gate's audit-fix-round cap; the principle is the
// same: a small fixed number of "try again with the error in the
// prompt" attempts catches the easy cases (typos, signature
// mismatches, missing imports) without burning unbounded tokens on
// pathological inputs.
const maxBuildFixRounds = 3

// buildFixPromptTemplate is the focused prompt handed to each
// build-fix attempt. Deliberately narrow: tell the agent what failed,
// hand it the compile error, instruct it to edit the working tree
// minimally. The agent inherits the full tool set so it can view, edit,
// and verify; the prompt's restraint comes from the explicit "make
// the minimum change necessary" framing, not from sandboxing.
const buildFixPromptTemplate = `A build check just failed in the main repository. Your job is to find and fix the cause so the build succeeds.

BUILD FAILURE (round %d/%d):
%s
%s
INSTRUCTIONS:
- Read the files mentioned in the error to understand the actual signatures, types, and conventions in this codebase. Do NOT guess.
- Make the minimum change necessary to fix the build. Do not refactor, reformat, or rename anything beyond what the error requires.
- Keep every existing test. When a test fails to compile or fails an assertion, fix the code under test so the test passes — or, if behavior legitimately changed, update the test body to match — but keep the test function and its assertions in place.
- After editing, optionally run the relevant build command yourself via bash (e.g. "go build ./..." from the right module directory) to verify. The orchestrator will re-check after you exit.
- If the only way you can see to make the build pass is to delete a test, remove its assertions, comment it out, skip it, or replace it with a TODO/stub, then STOP and exit WITHOUT editing. Deleting tests is never a valid fix and will be rejected. Describe the problem briefly and the orchestrator will roll the working tree back to its pre-run state.

Work in the main repo. Exit when the fix is in place.`

// testInventory maps each Go test file (path relative to the repo root)
// to the number of top-level test functions it declares. It is the
// currency of the test-deletion guard: a build-fix round that makes the
// build go green by shrinking this inventory has not fixed anything, it
// has thrown the failing tests overboard.
type testInventory map[string]int

// testFuncRE matches a top-level Go test entry point — Test*, Benchmark*,
// Fuzz*, or Example* declared as a plain func (not a method, which would
// have a receiver between `func` and the name). Anchored to the start of
// a line so commented-out or string-embedded matches are ignored.
var testFuncRE = regexp.MustCompile(`(?m)^func (Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*\s*\(`)

// collectTestInventory walks repoDir and counts test functions in every
// *_test.go file. It is intentionally Go-specific: the dogfood loop and
// the incident that motivated this guard are Go, and a wrong count for
// another ecosystem could falsely block a legitimate fix. .git,
// node_modules, vendor, target and .kai are pruned so the walk stays
// cheap on polyglot repos. A file we can't read is silently skipped —
// an undercount can only make the guard MORE permissive (never a false
// block), which is the safe direction to fail.
func collectTestInventory(repoDir string) testInventory {
	inv := testInventory{}
	if repoDir == "" {
		return inv
	}
	_ = filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "target", ".kai":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(repoDir, path)
		if rerr != nil {
			rel = path
		}
		inv[rel] = len(testFuncRE.FindAll(body, -1))
		return nil
	})
	return inv
}

// testsRemoved compares a baseline inventory against a later one and
// returns a human-readable list of any regression: test files that
// vanished entirely, or files whose test-function count dropped. Returns
// "" when nothing was removed. Counts that grow or hold steady are fine —
// adding or expanding tests during a build fix is welcome; only deletion
// is the smell. Merging two tests into one is the rare legitimate case
// this flags; the surfaced message lets a human override deliberately.
func testsRemoved(base, cur testInventory) string {
	var msgs []string
	for file, baseCount := range base {
		curCount, ok := cur[file]
		if !ok {
			msgs = append(msgs, fmt.Sprintf("  - %s (entire test file deleted; had %d test(s))", file, baseCount))
			continue
		}
		if curCount < baseCount {
			msgs = append(msgs, fmt.Sprintf("  - %s (%d → %d test(s))", file, baseCount, curCount))
		}
	}
	if len(msgs) == 0 {
		return ""
	}
	sort.Strings(msgs)
	return strings.Join(msgs, "\n")
}

// tryBuildFixLoop runs up to maxBuildFixRounds of "agent reads the
// build error and edits the tree" attempts. Each round:
//
//	1. Constructs a focused agent.Options with the build output in the
//	   prompt.
//	2. Runs the agent against mainRepo. The agent can edit files
//	   freely.
//	3. Re-captures mainRepo to produce a new snapshot.
//	4. Re-runs the build check against the new state.
//	5. If the build is clean AND no tests were removed to get there,
//	   returns success with the now-clean bc.
//	6. Otherwise, loops with the new build output as the next round's
//	   prompt context.
//
// Test-deletion guard: a failing build is trivially "fixed" by deleting
// the offending test, so a clean build alone is not proof of a real fix.
// Before accepting a clean build we compare the test inventory against
// the worker's pre-fix baseline; if any test file or test function
// disappeared, the round is rejected — the deleted tests are restored
// from the pre-fix snapshot and the (still-failing) build is handed to
// the next round with an explicit note that deletion is not allowed.
//
// Returns (success=false, last-seen bc) if all rounds exhaust without
// producing a clean build. The caller is expected to roll the working
// tree back in that case.
//
// Baseline awareness: success is "no NEW failures vs the pre-run
// buildBaseline", not "the whole tree compiles". On a tree that was
// already broken when kai started, the fix loop only has to undo the
// breakage the agent itself introduced — pre-existing failures are not
// its job and would otherwise make every round look like a failure.
func tryBuildFixLoop(ctx context.Context, db *graph.DB, cfg Config, run *AgentRun, mainRepo string, newLatest []byte, buildBaseline buildCheckResult, bc buildCheckResult) (bool, buildCheckResult) {
	if cfg.AgentProvider == nil {
		// Can't run a fix without a provider. Return as-is so the
		// caller falls through to rollback.
		return false, bc
	}

	// Baseline captured from the worker's output (the failing state we
	// were handed) before any fix attempt. A later round that shrinks
	// this inventory removed tests rather than fixing code.
	baselineTests := collectTestInventory(mainRepo)

	// violationNote carries a test-deletion warning from a rejected round
	// into the next round's prompt. Empty on the first round.
	var violationNote string

	for round := 1; round <= maxBuildFixRounds; round++ {
		prompt := fmt.Sprintf(buildFixPromptTemplate, round, maxBuildFixRounds,
			truncateBuildOutputForSurface(bc.Output), violationNote)

		opts := agent.Options{
			Workspace:       mainRepo,
			Prompt:          prompt,
			Model:           cfg.AgentModel,
			MaxTotalTokens:  cfg.MaxAgentTokens,
			Provider:        cfg.AgentProvider,
			ConsultProvider: cfg.AgentProvider,
			ConsultModel:    cfg.ConsultModel,
			Graph:           cfg.MainGraph,
			EnableBash:      cfg.AgentBashEnabled,
			BashAllow:       cfg.AgentBashAllow,
			SessionStore:    cfg.AgentSessionStore,
			TaskName:        fmt.Sprintf("build-fix-round-%d", round),
			Mode:            agent.ModeCoding,
			KaiBinary:       kaiBinary(cfg),
			RunLogDir:       kaipath.Resolve(mainRepo),
			// Tighter caps than a normal agent run: this is a focused
			// remediation, not exploration. Five turns is enough to
			// view two files and apply an edit.
			KeepToolResults: true,
			MaxReadsPerTurn: 5,
		}

		if _, err := agent.Run(ctx, opts); err != nil {
			// Agent failed (provider drop, budget, ctx cancel). Don't
			// retry — exit the loop and let the caller roll back.
			// bc still reflects the last build state.
			return false, bc
		}

		// Re-capture mainRepo so the next build check (and any
		// downstream classify) sees the agent's edits as a real
		// snapshot. Use the same KAI_CAPTURE_SKIP_SUMMARY pattern as
		// the worker's main capture to avoid the heavy tree-sitter
		// summary phase between rounds.
		if err := runInWithEnv(ctx, mainRepo,
			[]string{"KAI_CAPTURE_SKIP_SUMMARY=1"},
			kaiBinary(cfg), "capture", "-m",
			fmt.Sprintf("orchestrator: build-fix round %d", round)); err != nil {
			// Capture itself failed — can't reason about the new
			// state. Bail to rollback rather than guess.
			return false, bc
		}

		bc = runBuildCheckWithContext(ctx, mainRepo)
		if !bc.Ran || len(newFailures(buildBaseline, bc)) == 0 {
			// No NEW failures remain (pre-existing breakage is tolerated)
			// — but a build can go green simply because the failing tests
			// were deleted. Reject the round if the test inventory shrank
			// relative to the worker's baseline.
			removed := testsRemoved(baselineTests, collectTestInventory(mainRepo))
			if removed == "" {
				return true, bc
			}

			// Restore the worker's pre-fix snapshot so the deleted tests
			// come back, then resume from the real (still-failing) build.
			// If the restore can't run, bail with a descriptive bc and let
			// the caller do its own rollback to the pre-run snapshot.
			if rerr := restoreWorkingTreeToSnapshot(db, newLatest, mainRepo); rerr != nil {
				bc.Err = fmt.Errorf("build was made to pass by removing tests, and restoring them failed: %w", rerr)
				bc.Output = "removed tests:\n" + removed
				return false, bc
			}
			if cerr := runInWithEnv(ctx, mainRepo,
				[]string{"KAI_CAPTURE_SKIP_SUMMARY=1"},
				kaiBinary(cfg), "capture", "-m",
				fmt.Sprintf("orchestrator: revert test-removing build-fix round %d", round)); cerr != nil {
				bc.Err = fmt.Errorf("build was made to pass by removing tests; reverting them failed at capture: %w", cerr)
				bc.Output = "removed tests:\n" + removed
				return false, bc
			}
			bc = runBuildCheckWithContext(ctx, mainRepo)
			violationNote = fmt.Sprintf("\nYour previous attempt made the build pass by removing tests:\n%s\nThose tests have been restored. Do NOT delete, skip, or stub tests — fix the code under test instead.\n", removed)
			// Restoring should reinstate the original failing build; if it
			// somehow has no new failures now, accept it (no tests lost).
			if !bc.Ran || len(newFailures(buildBaseline, bc)) == 0 {
				return true, bc
			}
			continue
		}
		// Still failing; next round picks up bc.Output as the new
		// error context.
	}
	// Exhausted. If we got here after rejecting a test-deletion attempt,
	// prepend that context so the surfaced reason explains the loop kept
	// trying to delete tests rather than fix the code.
	if violationNote != "" && bc.Err != nil {
		bc.Output = strings.TrimSpace(violationNote) + "\n\n" + bc.Output
	}
	return false, bc
}
