package main

import "testing"

// TestGuardedByFastForward pins which refs the F-13 guard covers.
//
// The guard stops a contended push clobbering someone else's work, which
// is right for snap.latest — that records what a person made.
//
// It was also applied to cs.latest, and there it was a trap. The server
// force-sets cs.latest on every SSH git-receive-pack, so the tracking
// ref this guard compares against goes stale with no action by the user.
// From then on the guard fires on every push forever, and because the
// pre-push hook runs `kai push` to trigger CI, CI silently stops firing
// on ordinary git pushes. Both escapes it offers are destructive:
// `kai pull --force` overwrites the working tree, `kai push --force`
// overwrites the remote.
func TestGuardedByFastForward(t *testing.T) {
	if !guardedByFastForward("snap.latest") {
		t.Error("snap.latest must stay guarded — it is authored content and clobbering it loses a snapshot")
	}
	if guardedByFastForward("cs.latest") {
		t.Error("cs.latest must NOT be guarded — the server advances it too, so the guard fires forever and kills CI triggering")
	}
	for _, n := range []string{"snap.main", "snap.working", "ws.x.head", "git.abc1234", ""} {
		if guardedByFastForward(n) {
			t.Errorf("%q unexpectedly guarded", n)
		}
	}
}

// TestNeverSyncedIsNotDivergence pins the distinction the F-13 message
// used to blur.
//
// isNonFastForwardPush returns true when the tracking ref is absent —
// not because anything diverged, but because there is nothing to
// compare against. Reporting that as "the remote has advanced since you
// last synced" is false for a clone that has never synced, and it sends
// the reader hunting for a conflict that does not exist. The refusal is
// right; only the explanation was wrong.
func TestNeverSyncedIsNotDivergence(t *testing.T) {
	local, remote := []byte{1}, []byte{2}

	// Never synced: no tracking ref.
	if !isNonFastForwardPush(remote, nil, local) {
		t.Error("a push with no tracking ref should still be refused — there is no basis to judge it")
	}
	// Synced, and the remote moved: a real divergence.
	if !isNonFastForwardPush(remote, []byte{3}, local) {
		t.Error("a remote that moved past our baseline is a real divergence")
	}
	// Synced and unchanged: allowed.
	if isNonFastForwardPush(remote, remote, local) {
		t.Error("a remote matching our baseline is a fast-forward and must be allowed")
	}
}
