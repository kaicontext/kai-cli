package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Nothing the pull request changes differs since the previous review: the
// focus is the files earlier points name, or empty (not nil: not a full
// review) when there are none.
func TestReReviewFocus_NothingChanged(t *testing.T) {
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	got := rcReReviewFocus(&rcPrior{Head: "h1"}, rescopePR, sinceFrom(map[string][]string{"h1": {"vendor/x.go"}}))
	if got == nil || len(got) != 0 {
		t.Fatalf("no points: focus = %#v, want empty and non-nil", got)
	}
	prior := &rcPrior{Head: "h2", Points: []rcPriorPoint{
		{Kind: "defect", Ref: "p1", Text: "d.go:12 — the guard is missing"},
		{Kind: "defect", Ref: "p2", Text: "b.go:3 — off by one", Since: "h1"},
	}}
	// h1..HEAD changed a.go, but that was reviewed at h2 already.
	got = rcReReviewFocus(prior, rescopePR, sinceFrom(map[string][]string{"h2": nil, "h1": {"a.go"}}))
	if want := []string{"b.go", "d.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("points: focus = %v, want %v", got, want)
	}
	if !rcReReviewUnchanged(prior, rescopePR, sinceFrom(map[string][]string{"h2": nil})) {
		t.Fatal("unchanged not detected")
	}
	if rcReReviewUnchanged(prior, rescopePR, sinceFrom(map[string][]string{"h2": {"c.go"}})) {
		t.Fatal("a changed file read as unchanged")
	}
	if rcReReviewUnchanged(prior, rescopePR, sinceFrom(map[string][]string{})) {
		t.Fatal("a missing commit read as unchanged")
	}
	t.Setenv("KAI_REVIEW_RESCOPE", "off")
	if rcReReviewUnchanged(prior, rescopePR, sinceFrom(map[string][]string{"h2": nil})) {
		t.Fatal("unchanged with rescoping off")
	}
}

func TestReCheckTurnsAndBlock(t *testing.T) {
	if got := rcReCheckTurns(0); got != rcReCheckBaseTurns {
		t.Errorf("no point files: %d", got)
	}
	if got := rcReCheckTurns(2); got != rcReCheckBaseTurns+2*rcReviewTurnsPerFile {
		t.Errorf("two point files: %d", got)
	}
	if got := rcReCheckTurns(50); got != rcReviewBaseTurns {
		t.Errorf("never above a full review's base: %d", got)
	}
	b := rcReCheckBlock(&rcPrior{Head: "0123456789abcdef"}, []string{"d.go"}, 11)
	for _, want := range []string{"NOTHING CHANGED", rcShort("0123456789abcdef"), "11 turns", "(d.go)", "Do not review the change again"} {
		if !strings.Contains(b, want) {
			t.Errorf("block missing %q:\n%s", want, b)
		}
	}
}

func TestPatchByFileIgnoresPosition(t *testing.T) {
	a := rcPatchByFile("diff --git a/x.go b/x.go\nindex 111..222 100644\n--- a/x.go\n+++ b/x.go\n@@ -10 +10 @@ func F() {\n-\treturn 1\n+\treturn 2\n")
	b := rcPatchByFile("diff --git a/x.go b/x.go\nindex 333..444 100644\n--- a/x.go\n+++ b/x.go\n@@ -40 +40 @@ func F() {\n-\treturn 1\n+\treturn 2\n")
	if a["x.go"] == "" || a["x.go"] != b["x.go"] {
		t.Fatalf("moved change compares unequal: %q vs %q", a["x.go"], b["x.go"])
	}
	c := rcPatchByFile("diff --git a/x.go b/x.go\nnew file mode 100644\n--- /dev/null\n+++ b/x.go\n@@ -0,0 +1 @@\n+\treturn 2\n")
	if c["x.go"] == a["x.go"] {
		t.Fatal("a new file compares equal to an edit")
	}
}

func TestSplitPRChanges(t *testing.T) {
	oldP := map[string]string{"a.go": "-x\n+y\n", "b.go": "-p\n+q\n", "gone.go": "+z\n"}
	newP := map[string]string{"a.go": "-x\n+y\n", "b.go": "-p\n+r\n", "new.go": "+n\n"}
	// a.go and base.go differ commit to commit; only the base moved them.
	own, baseOnly := rcSplitPRChanges([]string{"a.go", "b.go", "base.go", "new.go", "gone.go"}, oldP, newP)
	if want := []string{"b.go", "gone.go", "new.go"}; !reflect.DeepEqual(own, want) {
		t.Errorf("own = %v, want %v", own, want)
	}
	if want := []string{"a.go"}; !reflect.DeepEqual(baseOnly, want) {
		t.Errorf("baseOnly = %v, want %v", baseOnly, want)
	}
}

// End to end on a real repository: main changes a file the pull request also
// changes, then the author merges main in (and, separately, rebases onto it).
// Commit to commit, that file differs; counting the pull request's own
// changes, only the file the author really edited does.
func TestPRChangedSince_MergeAndRebase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lines := func(n int, tag string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(tag)
			b.WriteString("\n")
		}
		return b.String()
	}
	run("init", "-q", "-b", "main")
	write("shared.go", "top\n"+lines(20, "mid")+"bottom\n")
	write("own.go", "one\n")
	run("add", "-A")
	run("commit", "-qm", "base")

	run("checkout", "-q", "-b", "pr")
	write("shared.go", "top PR\n"+lines(20, "mid")+"bottom\n") // the PR edits the top
	write("own.go", "one\n")
	run("commit", "-qam", "pr 1")
	reviewed := run("rev-parse", "HEAD")

	run("checkout", "-q", "main")
	write("shared.go", "top\n"+lines(20, "mid")+"bottom MAIN\n") // main edits the bottom
	write("other.go", "main only\n")
	run("add", "-A")
	run("commit", "-qm", "main moves")

	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"

	// The author merges main in and edits own.go.
	run("checkout", "-q", "pr")
	run("merge", "-q", "--no-edit", "main")
	write("own.go", "one, fixed\n")
	run("commit", "-qam", "fix")
	raw, _ := rcGitChangedSince(reviewed)
	if !reflect.DeepEqual(raw, []string{"other.go", "own.go", "shared.go"}) {
		t.Fatalf("commit to commit = %v", raw)
	}
	own, ok := rcGitPRChangedSince(reviewed)
	if !ok || !reflect.DeepEqual(own, []string{"own.go"}) {
		t.Fatalf("merge: own = %v (ok %v), want [own.go]", own, ok)
	}
	if b := rcBaseOnlySince(reviewed); !reflect.DeepEqual(b, []string{"shared.go"}) {
		t.Fatalf("merge: base only = %v, want [shared.go]", b)
	}

	// Rebased instead, with no edit of its own: nothing the PR changes differs.
	run("checkout", "-q", "-b", "pr2", reviewed)
	run("rebase", "-q", "main")
	own, ok = rcGitPRChangedSince(reviewed)
	if !ok || len(own) != 0 {
		t.Fatalf("rebase: own = %v (ok %v), want none", own, ok)
	}
	prior := &rcPrior{Head: reviewed}
	if !rcReReviewUnchanged(prior, []string{"shared.go", "own.go"}, rcGitPRChangedSince) {
		t.Fatal("rebase without edits not read as unchanged")
	}
}

// Nothing changed and the earlier points name every file of a small pull
// request: still a re-check, not a full review.
func TestReReviewFocus_UnchangedSmallPR(t *testing.T) {
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	prior := &rcPrior{Head: "h1", Points: []rcPriorPoint{{Kind: "defect", Ref: "p", Text: "a.go:3 — x"}}}
	got := rcReReviewFocus(prior, []string{"a.go"}, sinceFrom(map[string][]string{"h1": nil}))
	if !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("focus = %v, want [a.go]", got)
	}
}

// A change the author took back out of the pull request is a change: the
// review is not a re-check, and covers what is left in full.
func TestReReviewFocus_DroppedChange(t *testing.T) {
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	old := rcDroppedSinceFn
	defer func() { rcDroppedSinceFn = old }()
	rcDroppedSinceFn = func(string, []string) []string { return []string{"b.go"} }
	prior := &rcPrior{Head: "h1"}
	since := sinceFrom(map[string][]string{"h1": {"b.go"}})
	if got := rcReReviewFocus(prior, []string{"a.go", "c.go"}, since); got != nil {
		t.Fatalf("focus = %v, want nil (the whole change)", got)
	}
	if rcReReviewUnchanged(prior, []string{"a.go", "c.go"}, since) {
		t.Fatal("a dropped change read as unchanged")
	}
}

// gitRepo is a throwaway repository the test runs in.
func gitRepo(t *testing.T) (run func(...string) string, write func(name, body string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	run = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write = func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	return run, write
}

// Real git, base unmoved: the author reverts one of the pull request's files.
func TestDroppedSince_Revert(t *testing.T) {
	run, write := gitRepo(t)
	write("a.go", "a\n")
	write("b.go", "b\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("a.go", "a2\n")
	write("b.go", "b2\n")
	run("commit", "-qam", "pr")
	reviewed := run("rev-parse", "HEAD")
	write("b.go", "b\n") // taken back out
	run("commit", "-qam", "revert b")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	changed := []string{"a.go"}
	if d := rcDroppedSince(reviewed, changed); !reflect.DeepEqual(d, []string{"b.go"}) {
		t.Fatalf("dropped = %v, want [b.go]", d)
	}
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	if f := rcReReviewFocus(&rcPrior{Head: reviewed}, changed, rcGitPRChangedSince); f != nil {
		t.Fatalf("focus = %v, want nil", f)
	}
}

// Real git, base moved, a non-ASCII file name: git quotes such names unless
// told not to, and a quoted name would never match the pull request's files.
func TestPRChangedSince_NonASCIIName(t *testing.T) {
	run, write := gitRepo(t)
	write("café.go", "x\n")
	write("other.go", "o\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("café.go", "x2\n")
	run("commit", "-qam", "pr")
	reviewed := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	write("other.go", "o2\n")
	run("commit", "-qam", "main moves")
	run("checkout", "-q", "pr")
	run("merge", "-q", "--no-edit", "main")
	write("café.go", "x3\n")
	run("commit", "-qam", "fix")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	if own, ok := rcGitPRChangedSince(reviewed); !ok || !reflect.DeepEqual(own, []string{"café.go"}) {
		t.Fatalf("own = %q (ok %v), want [café.go]", own, ok)
	}
}

// A body line can start like a file header: a removed SQL or Lua comment
// "-- x" is "--- x" in a diff, an added "++ x" is "+++ x". They are part of
// the change, and two patches differing only there differ.
func TestPatchByFile_HeaderLikeBodyLines(t *testing.T) {
	head := "diff --git a/q.sql b/q.sql\nindex 1..2 100644\n--- a/q.sql\n+++ b/q.sql\n"
	a := rcPatchByFile(head + "@@ -3 +2,0 @@\n--- old note\n")
	b := rcPatchByFile(head + "@@ -3 +2,0 @@\n--- other note\n")
	if a["q.sql"] == "" || a["q.sql"] == b["q.sql"] {
		t.Fatalf("header-like body lines dropped: %q vs %q", a["q.sql"], b["q.sql"])
	}
	c := rcPatchByFile(head + "@@ -0,0 +1 @@\n+++ counter\n")
	if !strings.Contains(c["q.sql"], "+++ counter") {
		t.Fatalf("added '++' line dropped: %q", c["q.sql"])
	}
}

// The file's real path comes from the "---"/"+++" headers, not the
// "diff --git" line: a quoted name (a tab in it) has no " b/" there, and a
// path whose directory ends " b" has a misleading one.
func TestPatchByFile_HeaderPath(t *testing.T) {
	q := rcPatchByFile("diff --git \"a/x\\ty.go\" \"b/x\\ty.go\"\nindex 1..2 100644\n--- \"a/x\\ty.go\"\n+++ \"b/x\\ty.go\"\n@@ -1 +1 @@\n-a\n+b\n")
	if _, ok := q["x\ty.go"]; !ok {
		t.Fatalf("quoted path not keyed by its real name: %q", q)
	}
	s := rcPatchByFile("diff --git a/x b/y.go b/x b/y.go\nindex 1..2 100644\n--- a/x b/y.go\t\n+++ b/x b/y.go\t\n@@ -1 +1 @@\n-a\n+b\n")
	if _, ok := s["x b/y.go"]; !ok {
		t.Fatalf("path with a directory ending \" b\" not keyed by its real name: %q", s)
	}
	d := rcPatchByFile("diff --git \"a/x\\ty.go\" \"b/x\\ty.go\"\ndeleted file mode 100644\n--- \"a/x\\ty.go\"\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n")
	if _, ok := d["x\ty.go"]; !ok {
		t.Fatalf("deleted quoted path not keyed by its old name: %q", d)
	}
}

// A mode-only or binary entry has no "---"/"+++" line, so its key comes from
// the "diff --git" line alone: a quoted path must be unquoted and an unquoted
// one split at its midpoint, not by LastIndex(" b/").
func TestDiffGitPath(t *testing.T) {
	cases := []struct{ line, want string }{
		{"diff --git a/run.sh b/run.sh", "run.sh"},
		{"diff --git a/x b/y.go b/x b/y.go", "x b/y.go"},
		{`diff --git "a/x\ty.go" "b/x\ty.go"`, "x\ty.go"},
		{`diff --git "a/dir b/run.sh" "b/dir b/run.sh"`, "dir b/run.sh"},
	}
	for _, c := range cases {
		if got := rcDiffGitPath(c.line); got != c.want {
			t.Errorf("rcDiffGitPath(%q) = %q, want %q", c.line, got, c.want)
		}
	}
	// End to end: a mode-only change on a " b"-directory path keeps its key.
	m := rcPatchByFile("diff --git a/x b/run.sh b/x b/run.sh\nold mode 100644\nnew mode 100755\n")
	if _, ok := m["x b/run.sh"]; !ok {
		t.Fatalf("mode-only entry lost its path: %q", m)
	}
}

// A patch over the limit is given up on at once: git, blocked writing the
// rest, is stopped rather than waited on until the deadline.
func TestGitPatchByFile_OverLimitReturnsAtOnce(t *testing.T) {
	run, write := gitRepo(t)
	write("big.txt", "x\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	write("big.txt", strings.Repeat("a line of the big change\n", 40000)) // ~1 MB, past any pipe buffer
	run("commit", "-qam", "big")
	old := rcPatchLimit
	defer func() { rcPatchLimit = old }()
	rcPatchLimit = 1024
	start := time.Now()
	if _, ok := rcGitPatchByFile("HEAD~1", "HEAD"); ok {
		t.Fatal("an over-limit patch was used")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s: waited on a blocked git", d)
	}
}

// A mode change has no hunk: "old mode"/"new mode" are the whole change, and
// a patch that differs only there differs.
func TestPatchByFile_ModeChange(t *testing.T) {
	plain := rcPatchByFile("diff --git a/run.sh b/run.sh\nindex 1..2 100644\n--- a/run.sh\n+++ b/run.sh\n@@ -1 +1 @@\n-a\n+b\n")
	chmod := rcPatchByFile("diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\nindex 1..2\n--- a/run.sh\n+++ b/run.sh\n@@ -1 +1 @@\n-a\n+b\n")
	if plain["run.sh"] == chmod["run.sh"] {
		t.Fatalf("a mode change compared equal: %q", chmod["run.sh"])
	}
	only := rcPatchByFile("diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n")
	if only["run.sh"] == "" {
		t.Fatal("a mode-only change normalised to nothing")
	}
}

// Real git, base moved: after the review the author only makes a script
// executable. That is the pull request's own change.
func TestPRChangedSince_ModeOnlyChange(t *testing.T) {
	run, write := gitRepo(t)
	write("run.sh", "echo hi\n")
	write("other.go", "o\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("run.sh", "echo hello\n")
	run("commit", "-qam", "pr")
	reviewed := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	write("other.go", "o2\n")
	run("commit", "-qam", "main moves")
	run("checkout", "-q", "pr")
	run("merge", "-q", "--no-edit", "main")
	run("update-index", "--chmod=+x", "run.sh")
	run("commit", "-qm", "make executable")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	if own, ok := rcGitPRChangedSince(reviewed); !ok || !reflect.DeepEqual(own, []string{"run.sh"}) {
		t.Fatalf("own = %v (ok %v), want [run.sh]", own, ok)
	}
}

// The pull request's file list (rcCommitDiffStat, from --numstat) quotes a
// non-ASCII path and names a renamed file by its old path. The re-review
// still finds the edited file, and a rename is not a dropped change.
func TestReReviewFocus_QuotedAndRenamedPaths(t *testing.T) {
	run, write := gitRepo(t)
	write("café.go", "a\n")
	write("old.go", "package o\n\nfunc F() {}\n")
	write("x.go", "x\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("café.go", "a2\n")
	write("x.go", "x2\n")
	run("mv", "old.go", "new.go")
	run("add", "-A")
	run("commit", "-qm", "pr")
	reviewed := run("rev-parse", "HEAD")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	_, _, files := rcCommitDiffStat("main", "HEAD")
	changed := rcPathsOf(files)
	prior := &rcPrior{Head: reviewed}

	// Nothing since: a re-check, not a full review (the rename is not a drop).
	if f := rcReReviewFocus(prior, changed, rcGitPRChangedSince); f == nil || len(f) != 0 {
		t.Fatalf("unchanged: focus = %#v (changed %q), want empty", f, changed)
	}
	if !rcReReviewUnchanged(prior, changed, rcGitPRChangedSince) {
		t.Fatalf("unchanged not detected (changed %q)", changed)
	}

	// The author edits café.go only: it is the focus, in the list's spelling.
	write("café.go", "a3\n")
	run("commit", "-qam", "fix")
	if rcReReviewUnchanged(prior, changed, rcGitPRChangedSince) {
		t.Fatal("an edit to café.go read as unchanged")
	}
	f := rcReReviewFocus(prior, changed, rcGitPRChangedSince)
	if len(f) != 1 || rcGitUnquote(f[0]) != "café.go" {
		t.Fatalf("focus = %q, want café.go", f)
	}
}

// A rename after the previous review shows as two paths (old and new) but is
// one file of the pull request: it neither fills the focus on its own nor
// reads as more files than the pull request has.
func TestReReviewFocus_RenameCountsOnce(t *testing.T) {
	run, write := gitRepo(t)
	body := strings.Repeat("line that stays the same\n", 30) // big enough for git to pair a rename
	for _, f := range []string{"a.go", "b.go"} {
		write(f, f+"\n"+body)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("a.go", "a2\n"+body)
	write("b.go", "b2\n"+body)
	run("commit", "-qam", "pr")
	reviewed := run("rev-parse", "HEAD")
	run("mv", "a.go", "a_renamed.go") // the only edit since the review
	run("commit", "-qm", "rename")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	t.Setenv("KAI_REVIEW_RESCOPE", "")
	_, _, files := rcCommitDiffStat("main", "HEAD")
	changed := rcPathsOf(files)
	prior := &rcPrior{Head: reviewed}
	f := rcReReviewFocus(prior, changed, rcGitPRChangedSince)
	if f == nil {
		t.Fatalf("a rename alone made the focus the whole change (changed %q)", changed)
	}
	if n := rcPRFileCount(prior, changed); len(f) > n {
		t.Fatalf("focus %q is more files than the pull request's %d", f, n)
	}
}
