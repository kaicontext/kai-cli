package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// kai-server#688. A review pod clones blobless, so the diff fetches file
// contents from the remote. When that fetch failed (an expired clone token),
// rcCommitDiff returned "" and the review ended with "has an empty diff". The
// failure is git's and has to be reported as git's.
func TestRCCommitDiffReportsAFailedBlobFetch(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, src)
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, src, "second")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(src, "config", "uploadpack.allowFilter", "true")
	git(src, "config", "uploadpack.allowAnySHA1InWant", "true")

	clone := filepath.Join(t.TempDir(), "clone")
	git(".", "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+src, clone)
	t.Chdir(clone)

	// The remote stops answering, as GitHub does for an expired token. The
	// credentials in the URL must not reach the error.
	git(clone, "remote", "set-url", "origin", "https://x-access-token:s3cret@127.0.0.1:1/gone.git")
	diff, _, err := rcCommitDiff("HEAD~1", "HEAD", 1<<20)
	if err == nil {
		t.Fatalf("no error for a diff git could not produce; diff = %q", diff)
	}
	if !strings.Contains(err.Error(), rcGitDiffFailed) {
		t.Errorf("error %q does not carry %q, which kai-server reads the job log for", err, rcGitDiffFailed)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the remote's credentials: %q", err)
	}

	// With the remote back, the same call is the diff.
	git(clone, "remote", "set-url", "origin", "file://"+src)
	diff, _, err = rcCommitDiff("HEAD~1", "HEAD", 1<<20)
	if err != nil || !strings.Contains(diff, "+two") {
		t.Fatalf("diff = %q, err = %v", diff, err)
	}
}
