package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitHistoryRewritten drives the F-16 rewrite detection against a REAL git
// repo, reproducing the exact scenario from the defect: a linear history, then a
// teammate's rebase (reset --soft + recommit) that produces divergent SHAs.
// This is the signal that decides whether bridge import links old->new with a
// SUPERSEDES edge, so it must call a fast-forward a fast-forward and a rebase a
// rebase.
func TestGitHistoryRewritten(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(content, msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", msg)
		return git("rev-parse", "HEAD")
	}

	git("init", "-q")
	git("config", "user.email", "t@t.co")
	git("config", "user.name", "t")

	c1 := commit("v1", "c1")
	c2 := commit("v2", "c2")
	c3 := commit("v3", "c3")

	// Linear history: an earlier commit is an ancestor of a later one — a normal
	// fast-forward advance of git.HEAD, NOT a rewrite.
	if gitHistoryRewritten(dir, c1, c3) {
		t.Error("c1 -> c3 (linear advance) wrongly flagged as a rewrite")
	}
	if gitHistoryRewritten(dir, c2, c3) {
		t.Error("c2 -> c3 (fast-forward) wrongly flagged as a rewrite")
	}

	// Degenerate inputs never count as a rewrite.
	if gitHistoryRewritten(dir, c3, c3) {
		t.Error("identical sha flagged as a rewrite")
	}
	if gitHistoryRewritten(dir, "", c3) {
		t.Error("empty prev sha flagged as a rewrite")
	}

	// The F-16 scenario: teammate rebases shared history — reset --soft to c1 and
	// recommit different content, producing a divergent SHA. git.HEAD was at c3;
	// c3 is NOT an ancestor of the new tip, so this MUST read as a rewrite.
	git("reset", "--soft", c1)
	c2p := commit("v22", "c2-prime")
	if !gitHistoryRewritten(dir, c3, c2p) {
		t.Error("rebase (c3 not an ancestor of c2-prime) was NOT detected as a rewrite")
	}

	// Continuing on the rewritten line is a normal advance again.
	c3p := commit("v33", "c3-prime")
	if gitHistoryRewritten(dir, c2p, c3p) {
		t.Error("c2-prime -> c3-prime (advance on the new line) wrongly flagged as a rewrite")
	}

	// A previous SHA that no longer resolves (e.g. gc'd away after the rewrite)
	// means the old history is gone — also a rewrite.
	if !gitHistoryRewritten(dir, "0000000000000000000000000000000000000000", c3p) {
		t.Error("an unresolvable prev sha should be treated as a rewrite")
	}
}
