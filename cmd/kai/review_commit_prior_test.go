package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPriorFromEnv(t *testing.T) {
	t.Setenv("KAI_PRIOR_REVIEW", "")
	if rcPriorFromEnv() != nil {
		t.Error("an unset prior review was read")
	}
	t.Setenv("KAI_PRIOR_REVIEW", "{not json")
	if rcPriorFromEnv() != nil {
		t.Error("an unreadable prior review was read")
	}
	t.Setenv("KAI_PRIOR_REVIEW", `{"head":"19db9fca4cfd","readiness":3,"summary":"One fix.","points":[{"kind":"defect","text":"retry.go:12 — `+"`lastErr`"+` is swallowed"}]}`)
	p := rcPriorFromEnv()
	if p == nil || p.Head != "19db9fca4cfd" || p.Readiness != 3 || len(p.Points) != 1 || p.Points[0].Kind != "defect" {
		t.Fatalf("prior = %+v", p)
	}
}

func TestChangedSinceListsFilesOrSaysUnknown(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n"), 0o644)
	git("add", ".")
	git("commit", "-qm", "one")
	first := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0o644)
	git("commit", "-qam", "two")
	t.Chdir(dir)

	if files, known := rcChangedSince(first); !known || len(files) != 1 || files[0] != "b.go" {
		t.Errorf("changed since the first commit = %v (known %v), want [b.go]", files, known)
	}
	if files, known := rcChangedSince(git("rev-parse", "HEAD")); !known || len(files) != 0 {
		t.Errorf("changed since HEAD = %v (known %v), want nothing", files, known)
	}
	if _, known := rcChangedSince("0123456789abcdef0123456789abcdef01234567"); known {
		t.Error("a commit the clone does not have was treated as known")
	}
}

func TestPriorBlock(t *testing.T) {
	p := &rcPrior{Head: "19db9fca4cfd10a1b24a", Readiness: 3, Summary: "One fix before merge."}
	p.Points = append(p.Points, rcPriorPoint{Kind: "defect", Text: "internal/retry.go:12 — `lastErr` is swallowed"})

	b := rcPriorBlock(p, []string{"internal/retry.go"}, true, nil)
	for _, want := range []string{"YOUR PREVIOUS REVIEW OF THIS PULL REQUEST (of commit 19db9fca4cfd, MERGE_READY 3)", "Summary then: One fix before merge.",
		"- [defect] internal/retry.go:12 — `lastErr` is swallowed", "Files changed since then (1): internal/retry.go"} {
		if !strings.Contains(b, want) {
			t.Errorf("block lacks %q:\n%s", want, b)
		}
	}
	if b := rcPriorBlock(p, nil, true, nil); !strings.Contains(b, "none — the code is the same") {
		t.Errorf("unchanged code is not said:\n%s", b)
	}
	if b := rcPriorBlock(p, nil, false, nil); !strings.Contains(b, "unknown (that commit is not in this clone)") {
		t.Errorf("an unknown history is not said:\n%s", b)
	}
	// Force-push / shallow clone: the prior commit is gone, but the review's
	// own changed files still give the reviewer a focus list.
	if b := rcPriorBlock(p, nil, false, []string{"internal/retry.go", "internal/retry_test.go"}); !strings.Contains(b, "could not diff against that commit") ||
		!strings.Contains(b, "these are the files this review covers (2): internal/retry.go, internal/retry_test.go") {
		t.Errorf("the force-push fallback is not said:\n%s", b)
	}
	if rcPriorBlock(nil, nil, false, nil) != "" {
		t.Error("no prior review rendered a block")
	}
	if !strings.Contains(rcReviewSystem, "YOU HAVE REVIEWED THIS BEFORE") || !strings.Contains(rcReviewSystem, "Keep MERGE_READY where it was unless") {
		t.Error("the reviewer prompt lacks the re-review rule")
	}
}

// What the author answered reaches the reviewer, with the rule that it is
// not raised again and does not hold the score down (kai-server#434).
func TestPriorBlockCarriesTheAnswers(t *testing.T) {
	t.Setenv("KAI_PRIOR_REVIEW", `{"head":"ea7255b8993d","readiness":3,"answered":[{"point":"internal/smoketest/excerpt.go:12 — `+"`Excerpt`"+` still panics on a negative `+"`limit`"+`","reply":"Intentional — every caller passes a constant."}]}`)
	p := rcPriorFromEnv()
	if p == nil || len(p.Answered) != 1 {
		t.Fatalf("prior = %+v", p)
	}
	b := rcPriorBlock(p, nil, true, nil)
	for _, want := range []string{"see ANSWERED POINTS", "- you said: internal/smoketest/excerpt.go:12 — `Excerpt` still panics", "they replied: Intentional — every caller passes a constant."} {
		if !strings.Contains(b, want) {
			t.Errorf("block lacks %q:\n%s", want, b)
		}
	}
	if !strings.Contains(rcReviewSystem, "ANSWERED POINTS:") || !strings.Contains(rcReviewSystem, "An answered point never lowers MERGE_READY") {
		t.Error("the reviewer prompt lacks the answered-points rule")
	}
	p.Answered = nil
	if strings.Contains(rcPriorBlock(p, nil, true, nil), "ANSWERED") {
		t.Error("an answered heading with nothing answered")
	}
}
