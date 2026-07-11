package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/drift"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/spf13/cobra"
	"kai/internal/config"
)

// computeDriftReport resolves the graph↔git relationship for the current
// repo. Best-effort: returns nil outside a git repository (or when HEAD is
// unborn) so callers can skip the section entirely. Deliberately does no
// semantic work — `kai status` must stay on the rev-list-only fast path.
func computeDriftReport() *drift.Report {
	rep, err := drift.Compute("", kaiDir)
	if err != nil {
		return nil
	}
	return rep
}

// renderDriftStatus prints the graph drift section of `kai status`.
// One line for the relationship, plus a hint line when action is useful.
func renderDriftStatus(w io.Writer, rep *drift.Report) {
	if rep == nil {
		return
	}
	for _, line := range driftStatusLines(rep) {
		fmt.Fprintln(w, line)
	}
}

func driftStatusLines(rep *drift.Report) []string {
	head := shortPrefix(rep.GitHead)
	graph := shortPrefix(rep.GraphState)
	switch rep.Relationship {
	case drift.RelSynced:
		return []string{fmt.Sprintf("Graph:      in sync with git HEAD (%s)", head)}
	case drift.RelBehind:
		line := fmt.Sprintf("Graph:      %s behind git HEAD (graph %s · git %s",
			countNoun(rep.Behind, "commit"), graph, head)
		if rep.OldestUnprocessedUnix > 0 {
			line += fmt.Sprintf(" · oldest unprocessed %s", ageString(rep.OldestUnprocessedUnix))
		}
		line += ")"
		return []string{line, "            Run 'kai capture' (or 'kai import' for many commits) to catch up."}
	case drift.RelAhead:
		return []string{fmt.Sprintf("Graph:      %s ahead of git HEAD — checked out an older commit (graph %s · git %s)",
			countNoun(rep.Ahead, "commit"), graph, head)}
	case drift.RelDiverged:
		return []string{
			fmt.Sprintf("Graph:      diverged from git — %s unprocessed, %s only in graph (graph %s · git %s)",
				countNoun(rep.Behind, "commit"), countNoun(rep.Ahead, "commit"), graph, head),
			"            Run 'kai capture' to process the current line.",
		}
	case drift.RelOrphaned:
		lines := []string{"Graph:      pinned state shares no history with git HEAD (history rewritten?)"}
		if rep.GraphState != "" {
			lines[0] = fmt.Sprintf("Graph:      pinned %s shares no history with git HEAD %s (history rewritten?)", graph, head)
		}
		return append(lines, "            Run 'kai shadow drift' to reconcile.")
	case drift.RelUnpinned:
		return []string{fmt.Sprintf("Graph:      not yet pinned to a git commit (git %s) — next 'kai capture' on a commit will pin it", head)}
	}
	return nil
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ageString renders a unix-seconds timestamp as a coarse relative age.
// Unlike relTime it extends to days — drift ages span vacations.
func ageString(unixSec int64) string {
	d := time.Since(time.Unix(unixSec, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// currentGitBranchRef returns the full symbolic ref HEAD points at
// (refs/heads/main), or "" when detached / not a git repo.
func currentGitBranchRef() string {
	out, err := gitCmdOutput("symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// pinGraphRef records sha as processed in the graph_refs record, attributing
// it to the current branch only when the branch tip actually is that sha
// (a replayed range pins intermediate commits without moving the ref pin).
// Best-effort: pinning failures never fail the calling command.
func pinGraphRef(sha string) {
	if sha == "" {
		return
	}
	branch := currentGitBranchRef()
	if branch != "" {
		if tip, err := gitCmdOutput("rev-parse", "--verify", "--quiet", branch); err != nil || tip != sha {
			branch = ""
		}
	}
	if err := drift.Pin(kaiDir, branch, sha, time.Now()); err != nil {
		debugf("drift pin %s: %v", shortPrefix(sha), err)
	}
}

func gitCmdOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// --- per-query staleness (spec step 3) ---

// exitStaleSuspect is the strict-mode exit code for a stale-suspect answer,
// following the CI tripwire convention (75 = machine-visible "recheck").
const exitStaleSuspect = 75

var (
	queryStrictFlag bool
	queryQuietFlag  bool
)

// registerStalenessFlags attaches the shared staleness flags to a query
// command's flag set.
func registerStalenessFlags(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.Flags().BoolVar(&queryStrictFlag, "strict", false,
			"Exit 75 when the answer is stale-suspect (graph drift intersects this query)")
		c.Flags().BoolVarP(&queryQuietFlag, "quiet", "q", false,
			"Suppress the staleness annotation line")
	}
}

// queryStalenessGate classifies this query's answer against graph drift and
// refuses (per staleness.refuse_after_intersecting, or an orphaned graph)
// BEFORE the answer is printed. paths is the neighborhood: every
// repo-relative file that appears in the answer, plus the query target.
// Returns a nil block when staleness is unmeasurable (no git, unpinned).
//
// When the graph is behind and staleness.inline_budget_ms > 0, drift is
// caught up inline first — checkpointed, oldest-first, bounded by the time
// budget — so small drift vanishes instead of being annotated. Whatever
// remains after the budget is classified honestly. Diverged graphs are
// never caught up inline: a history rewrite deserves an explicit action.
func queryStalenessGate(db *graph.DB, paths []string) (*drift.Staleness, error) {
	rep := computeDriftReport()
	if rep == nil {
		return nil, nil
	}
	cfg, _ := config.Load(kaiDir)
	if db != nil && rep.Relationship == drift.RelBehind && cfg.Staleness.InlineBudgetMS > 0 {
		budget := time.Duration(cfg.Staleness.InlineBudgetMS) * time.Millisecond
		if res, err := catchUpDrift(db, budget, nil); err != nil {
			debugf("inline catch-up: %v", err)
		} else if res.Processed > 0 {
			rep = computeDriftReport()
		}
	}
	man, err := drift.SyncManifest("", kaiDir, rep)
	if err != nil {
		man = nil // classification degrades to relationship-only, still honest
	}
	st := drift.ClassifyStaleness(rep, man, drift.NeighborhoodFromFiles(paths), cfg.Staleness.RefuseAfterIntersecting)
	if st != nil && st.Class == drift.StaleRefused {
		return st, fmt.Errorf("staleness: refused — %s", st.Reason)
	}
	return st, nil
}

// annotateStaleness prints the one-line staleness annotation (stderr, so
// stdout stays parseable) after the answer, and applies strict-mode exit
// semantics for stale-suspect. Call as the last statement before return.
func annotateStaleness(st *drift.Staleness) {
	if st == nil || st.Class == drift.StaleFresh {
		return
	}
	if !queryQuietFlag {
		fmt.Fprintln(os.Stderr, stalenessLine(st))
	}
	if st.Class == drift.StaleSuspect {
		strict := queryStrictFlag
		if !strict {
			cfg, _ := config.Load(kaiDir)
			strict = cfg.Staleness.Strict
		}
		if strict {
			os.Exit(exitStaleSuspect)
		}
	}
}

func stalenessLine(st *drift.Staleness) string {
	switch st.Class {
	case drift.StaleValid:
		return fmt.Sprintf("staleness: stale-valid — %s, none intersect this query",
			countNoun(st.Drift, "unprocessed commit"))
	case drift.StaleSuspect:
		shas := make([]string, 0, len(st.Intersecting))
		for _, h := range st.Intersecting {
			shas = append(shas, shortPrefix(h.SHA))
		}
		verb := "intersect"
		if len(st.Intersecting) == 1 {
			verb = "intersects"
		}
		return fmt.Sprintf("staleness: stale-suspect — %s %s this query (%s); run 'kai capture' to catch up",
			countNoun(len(st.Intersecting), "unprocessed commit"), verb, strings.Join(shas, ", "))
	default:
		return fmt.Sprintf("staleness: %s", st.Class)
	}
}

// checkDriftHealth is the `kai doctor` drift section: the graph↔git
// relationship, graph_refs readability, and manifest consistency with the
// current state. With --fix, a stale or corrupt manifest is rebuilt (it is
// pure cache); graph_refs is never auto-modified — pins are assertions,
// and doctor only reports on them.
func checkDriftHealth(w io.Writer, ok, warn, bad string) {
	// graph_refs readability first: a corrupt record makes every other
	// drift signal a lie.
	if _, err := drift.LoadRefs(kaiDir); err != nil {
		fmt.Fprintf(w, "%s graph refs (%s): unreadable: %v\n", bad, drift.RefsFile, err)
		fmt.Fprintf(w, "      Pins can be rebuilt: remove the file, then 'kai import --since <last-good-sha>'.\n")
		return
	}

	rep := computeDriftReport()
	if rep == nil {
		return // unborn HEAD; nothing to check
	}
	switch rep.Relationship {
	case drift.RelSynced:
		fmt.Fprintf(w, "%s graph drift: in sync with git HEAD (%s)\n", ok, shortPrefix(rep.GitHead))
	case drift.RelUnpinned:
		fmt.Fprintf(w, "%s graph drift: not pinned to a git commit yet ('kai capture' or 'kai import' will pin)\n", warn)
	case drift.RelBehind:
		fmt.Fprintf(w, "%s graph drift: %s behind git HEAD ('kai import --since %s' to catch up)\n",
			warn, countNoun(rep.Behind, "commit"), shortPrefix(rep.GraphState))
	case drift.RelAhead:
		fmt.Fprintf(w, "%s graph drift: graph is ahead of git HEAD (older commit checked out) — queries answer from the matching state\n", warn)
	case drift.RelDiverged:
		fmt.Fprintf(w, "%s graph drift: diverged (history rewritten): %s unprocessed, %s superseded ('kai capture' to process the current line)\n",
			warn, countNoun(rep.Behind, "commit"), countNoun(rep.Ahead, "commit"))
	case drift.RelOrphaned:
		fmt.Fprintf(w, "%s graph drift: pinned state shares no history with git HEAD — re-import with 'kai import'\n", bad)
	}

	// Manifest consistency: the manifest is keyed to (graphState, gitHead);
	// a mismatch means it describes a drift range that no longer exists
	// (e.g. a rebase mid-drift with hooks missing). Safe to rebuild — it's
	// derived state.
	man, err := drift.LoadManifest(kaiDir)
	switch {
	case err != nil:
		if doctorFix {
			syncDriftManifest()
			fmt.Fprintf(w, "%s drift manifest: was corrupt, rebuilt\n", ok)
		} else {
			fmt.Fprintf(w, "%s drift manifest: corrupt (%v) — 'kai doctor --fix' rebuilds it\n", bad, err)
		}
	case man.GitHead == "" && man.GraphState == "" && len(man.Commits) == 0:
		// Never written — normal for a repo that hasn't drifted yet.
		if rep.Relationship == drift.RelBehind || rep.Relationship == drift.RelDiverged {
			if doctorFix {
				syncDriftManifest()
				fmt.Fprintf(w, "%s drift manifest: built for the current drift range\n", ok)
			} else {
				fmt.Fprintf(w, "%s drift manifest: not yet computed for the current drift — 'kai doctor --fix' builds it\n", warn)
			}
		} else {
			fmt.Fprintf(w, "%s drift manifest: empty (no drift recorded)\n", ok)
		}
	case man.GitHead != rep.GitHead || man.GraphState != rep.GraphState:
		if doctorFix {
			syncDriftManifest()
			fmt.Fprintf(w, "%s drift manifest: was stale, resynced to current state\n", ok)
		} else {
			fmt.Fprintf(w, "%s drift manifest: stale (describes %s..%s, not the current state) — 'kai doctor --fix' resyncs\n",
				warn, shortPrefix(man.GraphState), shortPrefix(man.GitHead))
		}
	default:
		fmt.Fprintf(w, "%s drift manifest: consistent with graph refs (%s)\n", ok, countNoun(len(man.Commits), "tracked commit"))
	}
}

// syncDriftManifest brings the drift manifest in line with the current
// graph↔git state. Called after pin sites advance graph_refs (catch-up
// shrinks the manifest) and by the detailed drift view. Best-effort.
func syncDriftManifest() {
	rep := computeDriftReport()
	if rep == nil {
		return
	}
	if _, err := drift.SyncManifest("", kaiDir, rep); err != nil {
		debugf("drift manifest sync: %v", err)
	}
}

// runDriftDetail is the default `kai shadow drift` view: the relationship,
// the per-commit drift manifest (files touched, import targets), and the
// estimated catch-up size.
func runDriftDetail(w io.Writer) error {
	rep, err := drift.Compute("", kaiDir)
	if err != nil {
		return fmt.Errorf("resolving git state (drift needs git history): %w", err)
	}
	man, err := drift.SyncManifest("", kaiDir, rep)
	if err != nil {
		return fmt.Errorf("syncing drift manifest: %w", err)
	}

	fmt.Fprintf(w, "Relationship: %s\n", rep.Relationship)
	if rep.GraphState != "" {
		via := rep.GraphRef
		if via == "" {
			via = "?"
		}
		fmt.Fprintf(w, "Graph state:  %s (via %s)\n", shortPrefix(rep.GraphState), via)
	}
	head := shortPrefix(rep.GitHead)
	if rep.GitRef != "" {
		head += " (" + rep.GitRef + ")"
	}
	fmt.Fprintf(w, "Git HEAD:     %s\n", head)

	switch rep.Relationship {
	case drift.RelSynced:
		fmt.Fprintln(w, "\nGraph is in sync with git; nothing to catch up.")
		return nil
	case drift.RelUnpinned:
		fmt.Fprintln(w, "\nNo graph state pinned yet — run 'kai capture' on a commit or 'kai import'.")
		return nil
	case drift.RelOrphaned:
		fmt.Fprintln(w, "\nPinned graph state shares no history with HEAD (history rewrite).")
		fmt.Fprintln(w, "Re-import from the current line with 'kai import', or 'kai capture' to pin HEAD.")
		return nil
	}

	behind, ahead := splitLegs(man)
	if len(behind) > 0 {
		totalFiles := 0
		for _, e := range behind {
			totalFiles += len(e.Changed)
		}
		fmt.Fprintf(w, "\nUnprocessed commits (%d, oldest first; %s to catch up):\n",
			len(behind), countNoun(totalFiles, "file"))
		printManifestEntries(w, behind)
	}
	if len(ahead) > 0 {
		fmt.Fprintf(w, "\nSuperseded commits (%d, only in the graph's old line):\n", len(ahead))
		printManifestEntries(w, ahead)
	}
	fmt.Fprintf(w, "\nManifest: %s — %s\n",
		filepath.Join(kaiDir, drift.ManifestFile), countNoun(len(man.Commits), "commit"))
	fmt.Fprintln(w, "Run 'kai capture' (or 'kai import' for many commits) to catch up.")
	return nil
}

func splitLegs(m *drift.Manifest) (behind, ahead []drift.CommitEntry) {
	for _, e := range m.Commits {
		if e.Leg == "ahead" {
			ahead = append(ahead, e)
		} else {
			behind = append(behind, e)
		}
	}
	return behind, ahead
}

const maxDriftFilesShown = 8

func printManifestEntries(w io.Writer, entries []drift.CommitEntry) {
	for _, e := range entries {
		age := ""
		if e.TimeUnix > 0 {
			age = "  " + ageString(e.TimeUnix)
		}
		note := ""
		if e.Truncated {
			note = "  (too large to analyze — conservatively intersects everything)"
		} else if len(e.ImportTargets) > 0 {
			note = "  [adds imports into " + strings.Join(e.ImportTargets, ", ") + "]"
		}
		fmt.Fprintf(w, "  %s%s  %s%s\n", shortPrefix(e.SHA), age, countNoun(len(e.Changed), "file"), note)
		for i, cf := range e.Changed {
			if i == maxDriftFilesShown {
				fmt.Fprintf(w, "      … and %d more\n", len(e.Changed)-maxDriftFilesShown)
				break
			}
			if cf.OldPath != "" {
				fmt.Fprintf(w, "      %s %s → %s\n", cf.Status, cf.OldPath, cf.Path)
			} else {
				fmt.Fprintf(w, "      %s %s\n", cf.Status, cf.Path)
			}
		}
	}
}
