package orchestrator

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kaicontext/kai-engine/planner"
)

// plan_coverage.go: a defensive layer that catches "worker stopped after
// step 1" failures. The planner's agent prompt names specific symbols and
// files that the work is supposed to touch; if the worker's diff doesn't
// intersect with those signals, the gate flips to Review with an explicit
// reason so the user sees the mismatch instead of approving a no-op.
//
// Background: round-12 dogfood (May 2026) — planner produced a 5-step
// plan referencing planDetailsExpanded, View(), dispatchPlanChoice, the
// `?` handler, and the esc reset. Worker emitted a single struct-field
// edit (4 lines), self-reported done, and the gate held it for review
// only because blast > AutoThreshold=0. Without plan-coverage, the only
// signal to the user was "blast 4" which read as "small, probably fine"
// — they approved a no-op. This check would have surfaced the gap.

var (
	// Backticked spans in the prompt are explicit "this is a named code
	// thing" markers. We only consider identifiers inside backticks
	// because that gives us strong signal with low false-positive rate
	// (the planner uses backticks specifically for code references).
	reBacktickSpan = regexp.MustCompile("`([^`\n]{1,200})`")
	// Mixed-case or snake_case identifier of length >= 5. Requires either
	// an internal capital after a lowercase, or an underscore, so we
	// exclude plain English words ("details", "prompt") that show up in
	// prose without being code symbols.
	reCodeIdent = regexp.MustCompile(`[A-Za-z][a-z0-9]+(?:[A-Z][A-Za-z0-9]+|_[A-Za-z0-9_]+)+`)
	// Source file paths, dotted-extension form. Limited to extensions
	// kai's worker mode commonly edits.
	reCodeFile = regexp.MustCompile(`[\w][\w./-]*\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|rb|c|h|cpp|hpp)\b`)
)

// coverageResult summarizes the plan-vs-diff intersection. UnderCovered
// is the decision the caller acts on; Reason produces the gate-reason
// string that gets persisted on the snapshot payload.
type coverageResult struct {
	TotalSignals   int
	Matched        int
	MissingSymbols []string
	MissingFiles   []string
}

// UnderCovered returns true when the worker's diff failed to intersect
// with enough of the planner's named signals to be plausibly complete.
// The thresholds are intentionally conservative: at least 3 signals must
// be present (below that, the planner gave us too little to act on), and
// at least half must be missing (a small miss is fine — a clear majority
// miss is the failure mode this layer exists to catch).
func (c coverageResult) UnderCovered() bool {
	if c.TotalSignals < 3 {
		return false
	}
	return c.Matched*2 < c.TotalSignals
}

// Reason renders the gate-reason line. Capped at 6 missing items so the
// snapshot payload and `kai gate list` output don't bloat on a prompt
// that named dozens of symbols.
func (c coverageResult) Reason() string {
	missing := append([]string{}, c.MissingFiles...)
	missing = append(missing, c.MissingSymbols...)
	const cap = 6
	if len(missing) > cap {
		missing = append(missing[:cap], "…")
	}
	return fmt.Sprintf(
		"plan-coverage: diff matched %d/%d planner-named signals; missing: %s",
		c.Matched, c.TotalSignals, strings.Join(missing, ", "),
	)
}

// extractPlanSignals returns the concrete code identifiers and file
// paths the planner named in the worker's prompt. Exposed so the
// zero-edits guard (which has no diff to read) can decide whether the
// worker bailed on a substantive task or had nothing meaningful to do.
func extractPlanSignals(prompt string) (symbols, files []string) {
	return extractSymbols(prompt), extractFiles(prompt)
}

// checkPlanCoverage compares the planner's authoritative target file
// list (task.Files) against the actual changed paths from the
// orchestrator's absorb step. Both sides use the same multi-root-
// prefixed path convention (e.g. "kai-server/foo.go"), so the
// comparison is a direct set intersection.
//
// Two structural fixes from the legacy implementation:
//
//   1. PATH NORMALIZATION. The old code ran `git diff -C mainRepo`
//      and substring-searched the diff for planner-named paths.
//      In multi-root, the planner's paths were prefixed
//      "kai-server/..." but the diff only ran in primary's repo
//      with paths like "kailab-control/...". 0/N false-positive
//      holds resulted. Using `changed` (also prefixed) eliminates
//      the format mismatch.
//
//   2. EXEMPLAR vs TARGET. The old code regex-extracted every file
//      mention from the prompt prose. The planner often mentions
//      files as exemplars ("matches the pattern in webhooks.go") —
//      those got pulled into the signal list and counted as
//      must-edit targets even when they were just guidance. Now
//      only task.Files counts as a target signal; the planner is
//      explicit about that list, prose is prose.
//
// Symbol coverage was dropped from this check — the legacy regex
// pulled identifiers from anywhere in backticks, with the same
// exemplar-conflation problem as files. A future revision could add
// symbol coverage by walking the diff per project (each project's DB
// has the prevLatest/newLatest snapshots needed to compute it), but
// the file-level check catches the headline failure mode this guard
// exists for ("worker stopped after step 1").
//
// Returns a zero result (no under-coverage) when task.Files is empty
// — the planner explicitly didn't name targets, so there's nothing
// to verify coverage against. The defensive layer below still has
// the agent-error guard and the build check; this is one signal of
// several, never the sole hold-vs-promote decision.
func checkPlanCoverage(task planner.AgentTask, changed []string) coverageResult {
	if len(task.Files) == 0 {
		return coverageResult{}
	}

	// Build a set of changed paths for O(1) lookup. The orchestrator's
	// `changed` slice is already deduped at absorb time, so no
	// secondary dedup needed here.
	changedSet := make(map[string]bool, len(changed))
	for _, c := range changed {
		changedSet[c] = true
	}

	matched := 0
	var missing []string
	for _, f := range task.Files {
		if pathMatchesChanged(f, changed, changedSet) {
			matched++
		} else {
			missing = append(missing, f)
		}
	}
	return coverageResult{
		TotalSignals: len(task.Files),
		Matched:      matched,
		MissingFiles: missing,
	}
}

// pathMatchesChanged reports whether the planner-named file `f`
// matches any path in the changed-paths set. Tries three matchers
// to absorb the few legitimate format variants seen in dogfood:
//
//   1. Exact match. Both sides should use multi-root-prefixed paths
//      in standard cases (planner emits `kai-server/foo.go`, absorb
//      emits the same).
//   2. Project-prefix stripped match. Defensive: if the planner
//      named "foo.go" and the diff has "kai-server/foo.go" (or
//      vice versa), strip the leading "<projectname>/" segment from
//      both and try again.
//   3. Suffix match. If neither side's prefix is well-formed but
//      one path ends with the other, accept it. Catches the rare
//      case where the planner gives a deeper path than the diff.
//
// All three are conservative: false-positive matches in coverage are
// MUCH cheaper than false-negative holds (the current bug). When
// signal is ambiguous, lean toward "matched" so the gate doesn't
// hold legitimate work.
func pathMatchesChanged(f string, changed []string, changedSet map[string]bool) bool {
	if changedSet[f] {
		return true
	}
	fStripped := stripFirstSegment(f)
	if fStripped != f && changedSet[fStripped] {
		return true
	}
	for _, c := range changed {
		cStripped := stripFirstSegment(c)
		if cStripped == fStripped && fStripped != "" {
			return true
		}
		if strings.HasSuffix(c, "/"+f) || strings.HasSuffix(f, "/"+c) {
			return true
		}
	}
	return false
}

// stripFirstSegment returns p with its first "/"-delimited segment
// removed; returns p unchanged if there's no "/".
func stripFirstSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func extractSymbols(prompt string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reBacktickSpan.FindAllStringSubmatch(prompt, -1) {
		for _, id := range reCodeIdent.FindAllString(m[1], -1) {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func extractFiles(prompt string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range reCodeFile.FindAllString(prompt, -1) {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
