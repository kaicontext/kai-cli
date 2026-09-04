package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// The baseline commit must not steal authorship from a user who has a
// git identity configured. The original check asked
// os.Getenv("GIT_AUTHOR_NAME") == "", but an identity normally lives in
// gitconfig, not the environment — so the guard fired for nearly
// everyone, and because GIT_* env vars OUTRANK gitconfig, every
// baseline commit came out as "Kai <kai@local>".
func TestBaselineCommit_KeepsTheUsersIdentity(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.name", "Jacob Schatz")
	gitRun(t, dir, "config", "user.email", "jschatz1@gmail.com")

	if !gitHasIdentity(dir) {
		t.Fatal("a repo with user.email configured must report an identity")
	}

	created, err := ensureGitBaselineCommit(dir)
	if err != nil || !created {
		t.Fatalf("baseline commit not created: created=%v err=%v", created, err)
	}

	if got := gitRun(t, dir, "log", "-1", "--format=%an <%ae>"); got != "Jacob Schatz <jschatz1@gmail.com>" {
		t.Fatalf("the baseline commit took the user's authorship away: %s", got)
	}
}

// With nothing configured, kai supplies its own identity so the commit
// can still be made. `git var GIT_AUTHOR_IDENT` cannot detect this case
// — it answers with an OS-derived guess and exits 0 — which is why the
// probe reads config instead.
func TestBaselineCommit_SuppliesIdentityWhenNoneConfigured(t *testing.T) {
	// Point git's global and system config at /dev/null so the developer's
	// own identity cannot leak in and make this pass by accident. These
	// are inherited by every git subprocess the code under test spawns.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	// And make sure no GIT_* identity is riding in the environment either.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "")
	}

	dir := t.TempDir()
	gitRun(t, dir, "init")

	if gitHasIdentity(dir) {
		t.Fatal("no identity is configured, but the probe claims there is one")
	}

	created, err := ensureGitBaselineCommit(dir)
	if err != nil || !created {
		t.Fatalf("baseline commit not created: created=%v err=%v", created, err)
	}
	if got := gitRun(t, dir, "log", "-1", "--format=%an <%ae>"); got != "Kai <kai@local>" {
		t.Fatalf("kai should have supplied its own identity, got %s", got)
	}
}

// A repo that cannot commit — signing configured with no usable key — must
// not take `kai init` down with it. The baseline commit is an
// optimisation; the error is reported and init continues.
func TestBaselineCommit_ReportsFailureWithoutCreatingACommit(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "config", "commit.gpgsign", "true")
	gitRun(t, dir, "config", "gpg.program", filepath.Join(dir, "definitely-not-a-real-gpg"))

	created, err := ensureGitBaselineCommit(dir)
	if err == nil {
		t.Fatal("a repo that cannot sign should report the commit failure")
	}
	if created {
		t.Fatal("reported creating a commit that does not exist")
	}
	// And HEAD must still be unresolvable — no half-made commit.
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatal("a failed signing run left a commit behind")
	}
}

// Idempotent: a repo that already has a commit is left alone.
func TestBaselineCommit_NoOpWhenACommitExists(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "commit", "--allow-empty", "-m", "first")
	before := gitRun(t, dir, "rev-parse", "HEAD")

	created, err := ensureGitBaselineCommit(dir)
	if err != nil || created {
		t.Fatalf("should have been a no-op: created=%v err=%v", created, err)
	}
	if after := gitRun(t, dir, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved on a repo that already had a commit: %s → %s", before, after)
	}
}
