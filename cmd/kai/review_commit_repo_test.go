package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Reviews on kai-desktop#288 and kai-desktop#300 (2026-09-08) stated they were
// reading "the kai-engine repo" and "the kai-server working tree". Nothing in
// the run could have told them otherwise: the CI job clones into `mktemp -d`,
// so the workspace is a random path, and the prompt named the repository
// nowhere — while rcReviewSystem requires one in the output. These assert the
// answer is now supplied rather than guessed.
func TestRepoIdentityPrefersTheWorkflowsOwnAnswer(t *testing.T) {
	// GITHUB_REPOSITORY_FULLNAME wins, because the CI workflow prefers it for
	// exactly the case where the kai org name and the GitHub org name differ.
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "kaicontext/kai-desktop")
	t.Setenv("GITHUB_REPOSITORY", "kai/kai-desktop")
	if got := rcRepoIdentity(t.TempDir()); got != "kaicontext/kai-desktop" {
		t.Errorf("rcRepoIdentity = %q, want the GitHub-side slug", got)
	}
}

func TestRepoIdentityFallsBackToGithubRepository(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
	t.Setenv("GITHUB_REPOSITORY", "kaicontext/kai-cli")
	if got := rcRepoIdentity(t.TempDir()); got != "kaicontext/kai-cli" {
		t.Errorf("rcRepoIdentity = %q, want kaicontext/kai-cli", got)
	}
}

// The local path: a human running `kai review-commit` has no GitHub
// environment at all, and the checkout's own remote is the answer.
func TestRepoIdentityReadsTheOriginRemote(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "git@github.com:kaicontext/kai-desktop.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable here: %v (%s)", args, err, out)
		}
	}
	if got := rcRepoIdentity(dir); got != "kaicontext/kai-desktop" {
		t.Errorf("rcRepoIdentity = %q, want the slug from the origin remote", got)
	}
}

// Nothing resolves: say nothing. An unnamed boundary is recoverable, a
// confidently wrong one is not — inventing a name here would rebuild the
// defect this closes.
func TestRepoIdentityStaysSilentWhenItCannotTell(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	if got := rcRepoIdentity(t.TempDir()); got != "" {
		t.Errorf("rcRepoIdentity = %q in a non-repo with no environment, want empty", got)
	}
	if got := rcRepoHeader(""); got != "" {
		t.Errorf("rcRepoHeader(\"\") = %q, want empty", got)
	}
}

// The header has to name the repo where the system prompt asks for it: the
// boundary sentence. Naming it once at the top and not in the instruction the
// model is following is how it got ignored before.
func TestRepoHeaderNamesTheBoundary(t *testing.T) {
	got := rcRepoHeader("kaicontext/kai-desktop")
	for _, want := range []string{
		"REPOSITORY: kaicontext/kai-desktop",
		`within kaicontext/kai-desktop, the only caller is X`,
		"Do not name a different repository",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Error("header must end with a blank line so it does not run into AUTHOR CONTEXT")
	}
}
