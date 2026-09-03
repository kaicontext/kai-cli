package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kaicontext/kai-engine/remote"
)

// expectedOldForPush chooses the expected-old value a push presents to
// the server's compare-and-swap for one ref.
//
// Until 2026-09-03 every ref sent the LIVE remote value, read seconds
// earlier — so the server's check could never fail, and the only thing
// between a stale clone and someone else's snapshot was the client-side
// guard, which --force, old binaries and kit (no guard at all) walked
// past. A guarded ref now presents the BASELINE this clone last synced
// (remote/origin/<ref>): when the remote has moved since, the server
// rejects the push whatever the client believes. Unguarded refs (ws.*,
// cs.*, git.*) keep the live value — they are per-session or immutable
// and were never the overwrite path.
//
// Two cases keep the live value even for a guarded ref: the ref is
// absent or already at newTarget (creating it, or a no-op re-push, has
// nothing to protect), and yielded — the guard deliberately let this
// push replace a server-ingest tip after checking git ancestry
// (ingestYieldDecision), so the baseline is not the point.
func expectedOldForPush(name string, live, tracked, newTarget []byte, yielded bool) []byte {
	if !guardedByFastForward(name) || yielded || len(live) == 0 || bytes.Equal(live, newTarget) {
		return live
	}
	return tracked
}

// ingestYieldDecision decides whether a push may replace a snap.latest
// tip written by the server's GitHub ingest (actor "github-ingest").
//
// The old rule yielded unconditionally: the ingest tip is derived and
// rebuildable, and refusing it wedged repos forever (2026-08-24). But an
// unconditional yield is also how a fresh, stale clone replaced a
// GitHub-fresh tip with month-old content (2026-09-03). The tip stands
// for a git commit — the server names it in a git.<short> ref pointing
// at the same snapshot — so the question has a real answer: does this
// clone contain that commit? If HEAD descends from it, the push is at or
// ahead of the tip and may replace it. If not, this clone is behind or
// has diverged, and the push is refused with the commit named.
//
// tipCommits are the short shas of git.* refs pointing at the tip (none
// means the server never recorded which commit the tip came from —
// refuse, since nothing can be checked). isAncestor answers `git
// merge-base --is-ancestor <sha> HEAD`; an error for one sha (unknown
// object) is "not contained", not a pass.
func ingestYieldDecision(tipCommits []string, isAncestor func(sha string) (bool, error)) (yield bool, reason string) {
	if len(tipCommits) == 0 {
		return false, "the remote tip was written by the server's GitHub ingest, but no git.<sha> ref names its commit, so there is nothing to compare your clone against"
	}
	for _, sha := range tipCommits {
		ok, err := isAncestor(sha)
		if err == nil && ok {
			return true, ""
		}
	}
	return false, fmt.Sprintf("the remote tip comes from GitHub commit %s, which your clone does not contain — you are behind it or have diverged", strings.Join(tipCommits, " / "))
}

// shortShasForTarget returns the short shas of the git.<short> refs that
// point at target — the commits the server says a snapshot was taken at.
func shortShasForTarget(refs []*remote.RefEntry, target []byte) []string {
	var out []string
	for _, r := range refs {
		if r == nil || !strings.HasPrefix(r.Name, "git.") || !bytes.Equal(r.Target, target) {
			continue
		}
		if sha := strings.TrimPrefix(r.Name, "git."); sha != "" {
			out = append(out, sha)
		}
	}
	return out
}

// gitIsAncestorOfHead answers `git merge-base --is-ancestor <sha> HEAD`
// in the repository root. Exit 1 (not an ancestor) and 128 (unknown
// object — the commit is not in this clone at all) are both "no".
func gitIsAncestorOfHead(sha string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", sha, "HEAD")
	cmd.Dir = gitRepoRootOrCwd()
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && (ee.ExitCode() == 1 || ee.ExitCode() == 128) {
		return false, nil
	}
	return false, err
}

// rejectedGuardedRefs turns a batch result that refused a guarded ref
// into the push's error text, or "" when every guarded ref landed.
func rejectedGuardedRefs(results []remote.BatchRefResult) string {
	for _, res := range results {
		if res.OK || !guardedByFastForward(res.Name) {
			continue
		}
		if strings.Contains(strings.ToLower(res.Error), "fast-forward") || strings.Contains(strings.ToLower(res.Error), "mismatch") {
			return fmt.Sprintf("push rejected: remote %s has moved since this clone last synced — your push is not a fast-forward.\n"+
				"  Run 'kai pull' to reconcile, then push again. (server: %s)", res.Name, res.Error)
		}
		return fmt.Sprintf("push rejected: %s: %s", res.Name, res.Error)
	}
	return ""
}
