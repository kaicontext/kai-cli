package main

import (
	"fmt"
	"os"
	"strings"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// A base the server can fetch.
//
// `kai ship --server` names the commit the session was spawned from and
// the server fetches it from GitHub to build the branch on. That commit
// is the CHECKOUT's HEAD at spawn time — and a checkout's HEAD is often
// a commit GitHub has never seen: the person committed locally and did
// not push, or a PR of theirs was squash-merged so the original commit
// exists only on their machine. The server then fails with "not our
// ref", and until now the message told the person to re-ship from a
// fresh session. That is the harness handing its own problem to the
// user.
//
// shipBaseFor resolves it here: if the spawn base is on the remote, it
// is the base. If not, the base becomes the nearest commit that IS —
// the merge-base of the spawn base with the remote's default branch —
// and the delta is re-derived against it, so the PR still carries only
// the agent's change. The local commits that were under the agent's
// feet are not shipped; where the agent's hunks cannot be separated
// from them, the file goes whole and says so.
type serverBase struct {
	SHA      string // what the server fetches
	Spawn    string // the spawn base (checkout HEAD at spawn), for the delta
	Moved    bool   // SHA != Spawn: the spawn base was not on the remote
	Unpushed int    // local commits between the two, when moved
	Reason   string // one line for the person
}

func shipBaseFor(cwd string, e *spawnpkg.Entry) serverBase {
	sha, _ := shipServerBase(cwd)
	b := serverBase{SHA: sha, Spawn: sha}
	if e == nil || e.SourceRepo == "" || sha == "" {
		return b
	}
	repo := e.SourceRepo
	if remoteHas(repo, sha) {
		return b
	}
	// The remote may simply be stale locally; one fetch before deciding.
	_, _ = gitOut(repo, "fetch", "-q", "--no-tags", remoteName(e))
	if remoteHas(repo, sha) {
		return b
	}
	def := remoteDefaultRef(repo, remoteName(e))
	if def == "" {
		b.Reason = fmt.Sprintf("base %.12s is not on the remote and no default branch was found; shipping it as-is", sha)
		return b
	}
	mb, err := gitOut(repo, "merge-base", sha, def)
	if err != nil || mb == "" {
		b.Reason = fmt.Sprintf("base %.12s is not on the remote and shares no history with %s; shipping it as-is", sha, def)
		return b
	}
	n := 0
	if out, err := gitOut(repo, "rev-list", "--count", mb+".."+sha); err == nil {
		fmt.Sscanf(out, "%d", &n)
	}
	b.SHA, b.Moved, b.Unpushed = mb, true, n
	b.Reason = fmt.Sprintf("base %.12s is not on the remote (%d unpushed local commit(s)); shipping against %s (%.12s) instead",
		sha, n, def, mb)
	return b
}

// remoteHas reports whether any remote-tracking ref contains sha.
func remoteHas(repo, sha string) bool {
	out, err := gitOut(repo, "branch", "-r", "--contains", sha)
	return err == nil && strings.TrimSpace(out) != ""
}

func remoteName(e *spawnpkg.Entry) string {
	if e != nil && e.RemoteName != "" {
		return e.RemoteName
	}
	return "origin"
}

// remoteDefaultRef is <remote>/HEAD's target when git knows it, else
// the first of main/master that exists on the remote.
func remoteDefaultRef(repo, remote string) string {
	if out, err := gitOut(repo, "symbolic-ref", "-q", "refs/remotes/"+remote+"/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, "refs/remotes/")
	}
	for _, name := range []string{"main", "master"} {
		if _, err := gitOut(repo, "rev-parse", "-q", "--verify", remote+"/"+name+"^{commit}"); err == nil {
			return remote + "/" + name
		}
	}
	return ""
}

// shipBaseUnfetchable recognizes the server's refusal for a base it
// could not fetch. The proactive check above should prevent it; this
// is the belt for the cases it cannot see (a remote the checkout does
// not track, a force-push under the person's feet).
func shipBaseUnfetchable(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "could not be fetched") || strings.Contains(m, "not our ref")
}

func noteShipBase(b serverBase) {
	if b.Reason != "" {
		fmt.Fprintf(os.Stderr, "note: %s\n", b.Reason)
	}
}
