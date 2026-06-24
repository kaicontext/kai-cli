package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGitCloneTargetDir pins the directory resolution to git's own naming rule
// across the URL forms a real `kai clone` actually receives. This matters
// because the F-11 fallback uses this path both to clean up a failed clone and
// to set up Kai afterwards — if it disagrees with the directory git created,
// cleanup misses (leaving a partial dir that breaks the Kai-only retry) or, with
// RemoveAll, targets the wrong path.
func TestGitCloneTargetDir(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"https URL", []string{"https://atlas.kaicontext.com/acet/myrepo"}, "myrepo"},
		{"https URL with .git suffix", []string{"https://github.com/acet/myrepo.git"}, "myrepo"},
		{"scp-like ssh URL", []string{"git@github.com:acet/myrepo.git"}, "myrepo"},
		{"kai shorthand tenant/repo", []string{"acet/myrepo"}, "myrepo"},
		{"explicit dir arg (the repro form)", []string{"acet/myrepo", "/tmp/dest"}, "/tmp/dest"},
		{"explicit dir overrides URL basename", []string{"https://h/o/r.git", "dest"}, "dest"},
		{"host without path segment", []string{"kaicontext.com/acet/myrepo"}, "myrepo"},
		{"no args", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitCloneTargetDir(tc.args); got != tc.want {
				t.Errorf("gitCloneTargetDir(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestPartialCloneCleanup_PreservesUserData is the safety regression for F-11:
// the fallback may remove a directory git left behind, so it must NEVER remove a
// directory the user already had. It models the exact decision runClone makes —
// sample the directory state before the (failed) clone, then decide whether to
// RemoveAll — over a real filesystem.
func TestPartialCloneCleanup_PreservesUserData(t *testing.T) {
	t.Run("pre-existing dir with user data is never deleted", func(t *testing.T) {
		// The user runs `kai clone <repo> existing-dir` where existing-dir
		// already holds their files. git refuses; the fallback must not touch it.
		dir := filepath.Join(t.TempDir(), "existing")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(keep, []byte("user data"), 0644); err != nil {
			t.Fatal(err)
		}

		existedNonEmpty := dirExistsNonEmpty(dir) // sampled before clone
		if !existedNonEmpty {
			t.Fatalf("dirExistsNonEmpty(%q) = false, want true", dir)
		}
		if shouldCleanPartialClone(dir, existedNonEmpty) {
			t.Fatalf("shouldCleanPartialClone returned true for a pre-existing non-empty dir — would delete user data")
		}
		// Model the fix's guarded cleanup and confirm the data survives.
		if shouldCleanPartialClone(dir, existedNonEmpty) {
			_ = os.RemoveAll(dir)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("user file was removed: %v", err)
		}
	})

	t.Run("directory git created (absent before) is cleaned up", func(t *testing.T) {
		// The common F-11 case: target didn't exist; git created then (sometimes)
		// left a partial dir. Cleaning it is safe so the Kai-only retry, which
		// refuses a non-empty target, can proceed.
		base := t.TempDir()
		dir := filepath.Join(base, "fresh")

		existedNonEmpty := dirExistsNonEmpty(dir) // sampled before clone -> false
		if existedNonEmpty {
			t.Fatalf("dirExistsNonEmpty(%q) = true, want false (dir does not exist yet)", dir)
		}
		if !shouldCleanPartialClone(dir, existedNonEmpty) {
			t.Fatalf("shouldCleanPartialClone returned false for a git-created dir — partial clone would block the retry")
		}

		// Simulate git leaving a partial dir behind, then the guarded cleanup.
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if shouldCleanPartialClone(dir, existedNonEmpty) {
			_ = os.RemoveAll(dir)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("partial clone dir still present after cleanup: stat err = %v", err)
		}
	})

	t.Run("empty target name is never cleaned", func(t *testing.T) {
		if shouldCleanPartialClone("", false) {
			t.Errorf("shouldCleanPartialClone(\"\", false) = true, want false")
		}
	})
}
