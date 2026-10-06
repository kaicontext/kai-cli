package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rcLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "theorem t%06d : (%d : Nat) + 0 = %d := by simp -- padding padding padding\n", i, i, i)
	}
	return b.String()
}

func TestRCBudgetPatch(t *testing.T) {
	small := "@@ -0,0 +1,2 @@\n+a\n+b"
	if got := rcBudgetPatch(small, rcPatchTotalBudget); got != small {
		t.Errorf("a small patch changed: %q", got)
	}
	big := "@@ -0,0 +1,2000 @@\n+" + strings.ReplaceAll(strings.TrimRight(rcLines(2000), "\n"), "\n", "\n+")
	got := rcBudgetPatch(big, rcPatchTotalBudget)
	if len(got) > rcPatchFileBudget || !strings.HasSuffix(got, "\n"+rcPatchCutNote) {
		t.Errorf("cut patch is %d bytes / missing the note", len(got))
	}
	if !strings.HasPrefix(big, strings.TrimSuffix(got, rcPatchCutNote)) {
		t.Error("the kept part is not a prefix of the patch")
	}
	if got := rcBudgetPatch(big, 10); got != rcPatchOmittedNote {
		t.Errorf("no room left: got %q", got)
	}
}

// FalkorDB#3172 in miniature: a commit adding many large files gets a bundle
// whose patches stay within the total budget, every file still listed with its
// counts.
func TestRCCommitDiffStatHoldsPatchesToBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, dir)
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("P%02d.lean", i)), []byte(rcLines(700)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCommitAll(t, dir, "proofs")
	t.Chdir(dir)
	if out, err := exec.Command("git", "rev-parse", "HEAD~1").Output(); err != nil || len(out) == 0 {
		t.Fatalf("rev-parse: %v", err)
	}

	added, _, files := rcCommitDiffStat("HEAD~1", "HEAD")
	if len(files) != 40 || added != 40*700 {
		t.Fatalf("files=%d added=%d", len(files), added)
	}
	total, cut, omitted := 0, 0, 0
	for _, f := range files {
		total += len(f.Patch)
		if f.Added != 700 {
			t.Errorf("%s: added %d", f.Path, f.Added)
		}
		switch {
		case f.Patch == rcPatchOmittedNote:
			omitted++
		case strings.HasSuffix(f.Patch, rcPatchCutNote):
			cut++
		}
	}
	if total > rcPatchTotalBudget+len(files)*len(rcPatchOmittedNote) {
		t.Errorf("patches total %d bytes, past the budget", total)
	}
	if cut == 0 || omitted == 0 {
		t.Errorf("cut=%d omitted=%d: expected both on a change this size", cut, omitted)
	}
	if !strings.HasPrefix(files[0].Patch, "@@ -0,0 +1,700 @@\n+theorem t000000") {
		t.Errorf("first file's patch lost its head: %.60q", files[0].Patch)
	}
}

// A small change is untouched: every patch whole.
func TestRCCommitDiffStatSmallChangeWhole(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, dir, "f")
	t.Chdir(dir)
	_, _, files := rcCommitDiffStat("HEAD~1", "HEAD")
	if len(files) != 1 || !strings.Contains(files[0].Patch, "+func F() {}") || strings.Contains(files[0].Patch, "Kai:") {
		t.Fatalf("files=%+v", files)
	}
}
