package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/drift"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
)

// catchUpResult summarizes an incremental catch-up run.
type catchUpResult struct {
	Processed int
	Remaining int
	// BudgetHit means the run stopped on the time budget with commits left;
	// the graph is consistent at the last completed checkpoint.
	BudgetHit bool
}

// catchUpDrift processes the graph's unprocessed commits oldest-first,
// checkpointing after each one: semantic snapshot built from the git ref
// (never the working tree), git.<sha> ref, graph_refs pin, manifest
// retirement. A commit is started only while inside budget (<= 0 =
// unbounded); one commit may run past the budget rather than abandon a
// checkpoint mid-write. Interruption at any point — budget, ctrl-C, crash —
// leaves the graph at the last completed commit: the pin is written only
// after that commit's snapshot lands, and snapshots are content-addressed,
// so a torn run re-does at most one commit's work on resume.
//
// For a diverged graph this processes the behind leg (git's current line);
// advancing the branch pin onto it supersedes the graph's old line, which
// is correct — git's history is the truth catch-up converges to.
func catchUpDrift(db *graph.DB, budget time.Duration, progress func(done, total int, sha string)) (catchUpResult, error) {
	var res catchUpResult
	rep := computeDriftReport()
	if rep == nil || rep.GraphState == "" {
		return res, nil // no git, or nothing pinned to advance from
	}
	if rep.Relationship != drift.RelBehind && rep.Relationship != drift.RelDiverged {
		return res, nil
	}
	mb, err := gitCmdOutput("merge-base", rep.GraphState, rep.GitHead)
	if err != nil || mb == "" {
		return res, nil
	}
	out, err := gitCmdOutput("rev-list", "--reverse", mb+".."+rep.GitHead)
	if err != nil || out == "" {
		return res, nil
	}
	shas := strings.Split(out, "\n")
	res.Remaining = len(shas)

	start := time.Now()
	branch := currentGitBranchRef()
	refMgr := ref.NewRefManager(db)
	autoRefMgr := ref.NewAutoRefManager(db)

	for i, sha := range shas {
		sha = strings.TrimSpace(sha)
		if sha == "" {
			continue
		}
		if budget > 0 && time.Since(start) > budget {
			res.BudgetHit = true
			break
		}

		snapID, err := createSnapshotFromGitRef(db, ".", sha)
		if err != nil {
			return res, fmt.Errorf("catch-up at %s: %w", shortPrefix(sha), err)
		}
		meta := map[string]string{"source": "catch_up", "git_commit": sha}
		_ = refMgr.SetWithMeta("git."+shortPrefix(sha), snapID, ref.KindSnapshot, "", meta)
		_ = autoRefMgr.OnSnapshotCreatedWithMeta(snapID, meta)

		// The checkpoint: graph_refs advances to this commit. The branch pin
		// moves per commit (newest processed commit on this line), which is
		// what makes an interrupted run resume where it stopped.
		if err := drift.Pin(kaiDir, branch, sha, time.Now()); err != nil {
			return res, fmt.Errorf("pinning %s: %w", shortPrefix(sha), err)
		}
		res.Processed++
		res.Remaining = len(shas) - res.Processed
		syncDriftManifest()

		if progress != nil {
			progress(i+1, len(shas), sha)
		}
	}
	return res, nil
}

// runImportSince is `kai import --since <sha>`: treat <sha> as the
// last-processed commit and import everything after it up to HEAD, one
// checkpointed commit at a time. Resumable: if a previous run already
// advanced the pin past <sha>, it resumes from the pin instead of
// re-importing.
func runImportSince(db *graph.DB, since string, w io.Writer) error {
	sinceSHA, err := gitCmdOutput("rev-parse", "--verify", "--quiet", since+"^{commit}")
	if err != nil || sinceSHA == "" {
		return fmt.Errorf("--since %q does not name a commit", since)
	}

	// Assert the starting state, without moving an existing pin backward:
	// a pin that already descends from <sha> means earlier work is done.
	branch := currentGitBranchRef()
	refs, err := drift.LoadRefs(kaiDir)
	if err != nil {
		return err
	}
	existing := ""
	if branch != "" {
		existing = refs.Refs[branch]
	}
	alreadyPast := existing != "" &&
		gitIsAncestor(sinceSHA, existing) && existing != sinceSHA
	if !alreadyPast {
		if err := drift.Pin(kaiDir, branch, sinceSHA, time.Now()); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "  Resuming from %s (already imported past --since)\n", shortPrefix(existing))
	}

	res, err := catchUpDrift(db, 0, func(done, total int, sha string) {
		fmt.Fprintf(w, "\r\033[K  [%d/%d] %s", done, total, shortPrefix(sha))
	})
	if res.Processed > 0 {
		fmt.Fprintf(w, "\r\033[K")
	}
	if err != nil {
		return fmt.Errorf("%w\n  %d commits imported; re-run to resume from the last checkpoint", err, res.Processed)
	}
	if res.Processed == 0 {
		fmt.Fprintln(w, "  Nothing to import — graph is current.")
		return nil
	}
	fmt.Fprintf(w, "  ✓ Imported %s; graph is current with HEAD\n", countNoun(res.Processed, "commit"))
	return nil
}

func gitIsAncestor(ancestor, descendant string) bool {
	_, err := gitCmdOutput("merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}
