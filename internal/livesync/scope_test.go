package livesync

import "testing"

// A saved replay position belongs to one scope. Sync seqs are global per
// repo, so resuming workspace A's LastSeq in workspace B would skip B's
// earlier events; and a scope must never resume another's.
func TestResumeSeqIsScoped(t *testing.T) {
	dir := t.TempDir()
	a := &Engine{kaiDir: dir, workspace: "feat/x#aaaa"}
	a.saveSyncSeq(696)
	if got := a.resumeSeq(); got != 696 {
		t.Fatalf("same scope resumeSeq = %d, want 696", got)
	}
	b := &Engine{kaiDir: dir, workspace: "feat/x#bbbb"}
	if got := b.resumeSeq(); got != 0 {
		t.Fatalf("other scope resumeSeq = %d, want 0 (replay its own events from the start)", got)
	}
	b.SaveState(nil)
	if got := a.resumeSeq(); got != 0 {
		t.Fatalf("after scope b saved state, scope a resumeSeq = %d, want 0", got)
	}
}

func TestCRDTDirIsScoped(t *testing.T) {
	dir := t.TempDir()
	repo := (&Engine{kaiDir: dir}).crdtDir()
	a := (&Engine{kaiDir: dir, workspace: "feat/x#aaaa"}).crdtDir()
	b := (&Engine{kaiDir: dir, workspace: "feat/x#bbbb"}).crdtDir()
	if a == b || a == repo || b == repo {
		t.Fatalf("CRDT dirs must differ per scope: repo=%s a=%s b=%s", repo, a, b)
	}
}
