package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/drift"
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
