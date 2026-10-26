package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRulesReadsEveryField(t *testing.T) {
	r, err := rcParseRules([]byte(`
rules: [design, Tests, design]
sensitive_paths:
  - "billing/**"
  - /auth/*.go
instructions: |
  Every handler checks org membership before reading a row.
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Packs, ",") != "design,tests" {
		t.Errorf("packs = %v, want design,tests (lowercased, deduplicated)", r.Packs)
	}
	if strings.Join(r.SensitivePaths, ",") != "billing/**,auth/*.go" {
		t.Errorf("sensitive = %v, want the leading slash dropped", r.SensitivePaths)
	}
	if r.Instructions != "Every handler checks org membership before reading a row." {
		t.Errorf("instructions = %q", r.Instructions)
	}
}

func TestParseRulesEmptyFileIsNoRules(t *testing.T) {
	r, err := rcParseRules(nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Instructions != "" || len(r.Packs) != 0 || len(r.SensitivePaths) != 0 {
		t.Errorf("rules = %+v, want empty", r)
	}
}

// A typo must say so: a rule that silently did nothing would leave the
// maintainers believing it applied.
func TestParseRulesRejectsWhatItCannotHonor(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":    "instruction: typo\n",
		"unknown pack":   "rules: [security]\n",
		"bad glob":       "sensitive_paths: [\"billing/[\"]\n",
		"empty glob":     "sensitive_paths: [\"\"]\n",
		"not yaml":       "rules: [design\n",
		"long":           "instructions: " + strings.Repeat("x", rcRulesMaxInstructionBytes+1) + "\n",
		"too many globs": "sensitive_paths: [" + strings.TrimSuffix(strings.Repeat("a/**,", rcRulesMaxSensitivePaths+1), ",") + "]\n",
	} {
		if _, err := rcParseRules([]byte(body), "test"); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
	_, err := rcParseRules([]byte("rules: [security]\n"), "test")
	if err == nil || !strings.Contains(err.Error(), "design") {
		t.Errorf("unknown pack error %v, want the valid packs listed", err)
	}
}

func withRules(t *testing.T, r *rcRepoRules) {
	t.Helper()
	rcActiveRules = r
	t.Cleanup(func() { rcActiveRules = nil })
}

func TestRulesBlock(t *testing.T) {
	withRules(t, nil)
	if got := rcRulesBlock([]string{"a.go"}); got != "" {
		t.Errorf("no rules: block %q, want none", got)
	}

	// Packs alone are recorded, not prompted: nothing would publish them yet.
	withRules(t, &rcRepoRules{Packs: []string{"design"}, SensitivePaths: []string{"billing/**"}})
	if got := rcRulesBlock([]string{"api/a.go"}); got != "" {
		t.Errorf("packs only, no sensitive file touched: block %q, want none", got)
	}

	withRules(t, &rcRepoRules{
		Instructions:   "Money is int64 cents.",
		SensitivePaths: []string{"billing/**", "**/auth.go"},
	})
	got := rcRulesBlock([]string{"billing/charge.go", "web/page.tsx", "svc/x/auth.go"})
	for _, want := range []string{"Money is int64 cents.", "  billing/charge.go\n", "  svc/x/auth.go\n", "cannot edit them"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "web/page.tsx") {
		t.Errorf("block names a file outside the sensitive globs:\n%s", got)
	}
	if strings.Contains(got, "design") {
		t.Errorf("block mentions a rule pack:\n%s", got)
	}

	// Instructions without a sensitive file still go in; the list does not.
	got = rcRulesBlock([]string{"web/page.tsx"})
	if !strings.Contains(got, "Money is int64 cents.") || strings.Contains(got, "SENSITIVE") {
		t.Errorf("block = %q, want instructions and no sensitive list", got)
	}
}

func TestRulesBlockCapsTheSensitiveList(t *testing.T) {
	withRules(t, &rcRepoRules{SensitivePaths: []string{"s/**"}})
	var paths []string
	for i := 0; i < rcRulesMaxSensitiveListed+5; i++ {
		paths = append(paths, filepath.Join("s", strings.Repeat("f", i+1)+".go"))
	}
	got := rcRulesBlock(paths)
	if !strings.Contains(got, "... and 5 more") {
		t.Errorf("block does not cap the list:\n%s", got)
	}
}

// The fact-check gets the instructions for every batch, so an allegation that
// rests on a stated requirement can cite it.
func TestRulesChallengeSources(t *testing.T) {
	withRules(t, &rcRepoRules{SensitivePaths: []string{"a/**"}})
	if got := rcRulesChallengeSources([]string{"a/b.go"}); got != nil {
		t.Errorf("no instructions: sources %v, want none", got)
	}
	withRules(t, &rcRepoRules{Instructions: "Never log tokens."})
	src := rcRulesChallengeSources([]string{"a/b.go", "c.go"})
	if len(src) != 1 || !strings.Contains(src[0].Text, "Never log tokens.") {
		t.Fatalf("sources = %+v", src)
	}
	got := rcBatchSources(nil, src, []string{"c.go:3 — logs the token"})
	if len(got) != 1 {
		t.Errorf("a batch about c.go got %d sources, want the instructions", len(got))
	}
}

// rulesRepo makes a repository whose base branch (main) carries body at the
// rules path, and whose pr branch edits the file to something else — the case
// that matters: a pull request cannot change the rules it is reviewed by.
func rulesRepo(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable here: %v (%s)", args, err, out)
		}
	}
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet", "-b", "main")
	write("a.txt", "a\n")
	git("add", "a.txt")
	if body != "" {
		write(rcRulesPath, body)
		git("add", rcRulesPath)
	}
	git("commit", "--quiet", "-m", "base")
	git("checkout", "--quiet", "-b", "pr")
	write(rcRulesPath, "instructions: anything goes\n")
	git("add", rcRulesPath)
	git("commit", "--quiet", "-m", "loosen the rules")
	return dir
}

func clearRulesEnv(t *testing.T) {
	t.Setenv("KAI_REVIEW_RULES", "")
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
}

func TestLoadRulesFromTheBaseBranch(t *testing.T) {
	clearRulesEnv(t)
	t.Cleanup(func() { rcActiveRules = nil })
	inDir(t, rulesRepo(t, "instructions: Money is int64 cents.\n"))
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "acme/api")

	if warn := rcLoadRulesFor("main"); warn != "" {
		t.Fatal(warn)
	}
	if rcActiveRules == nil || rcActiveRules.Instructions != "Money is int64 cents." {
		t.Fatalf("rules = %+v, want the base branch's, not the pull request's", rcActiveRules)
	}
	if !strings.HasPrefix(rcActiveRules.Source, "acme/api@main:") {
		t.Errorf("source = %q, want the repository and base named", rcActiveRules.Source)
	}
	if line := rcDescribeRules(); !strings.Contains(line, "instructions") {
		t.Errorf("log line = %q", line)
	}

	// No base, no rules: a single-commit review has no base to trust.
	if warn := rcLoadRulesFor(""); warn != "" || rcActiveRules != nil {
		t.Fatalf("no base: rules %+v, warning %q; want none", rcActiveRules, warn)
	}
}

func TestLoadRulesWithoutAFileIsNoRules(t *testing.T) {
	clearRulesEnv(t)
	t.Cleanup(func() { rcActiveRules = nil })
	inDir(t, rulesRepo(t, ""))
	if warn := rcLoadRulesFor("main"); warn != "" || rcActiveRules != nil {
		t.Fatalf("rules %+v, warning %q; want none", rcActiveRules, warn)
	}
}

// A broken file is reported and the review runs without rules — never fails.
func TestLoadRulesWarnsOnABrokenFile(t *testing.T) {
	clearRulesEnv(t)
	t.Cleanup(func() { rcActiveRules = nil })
	inDir(t, rulesRepo(t, "rules: [security]\n"))
	warn := rcLoadRulesFor("main")
	if rcActiveRules != nil {
		t.Fatalf("broken file loaded: %+v", rcActiveRules)
	}
	if !strings.Contains(warn, "security") || !strings.Contains(warn, "without them") {
		t.Errorf("warning = %q, want the bad pack named and the fallback stated", warn)
	}
}

// KAI_REVIEW_RULES points at a local file, for trying rules by hand.
func TestLoadRulesFromALocalFile(t *testing.T) {
	clearRulesEnv(t)
	t.Cleanup(func() { rcActiveRules = nil })
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte("rules: [observability]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAI_REVIEW_RULES", path)
	if warn := rcLoadRulesFor(""); warn != "" {
		t.Fatal(warn)
	}
	if rcActiveRules == nil || len(rcActiveRules.Packs) != 1 {
		t.Fatalf("rules = %+v", rcActiveRules)
	}
	if line := rcDescribeRules(); !strings.Contains(line, "not yet applied") {
		t.Errorf("log line = %q, want packs reported as not yet applied", line)
	}
}

func TestParseRulesDetail(t *testing.T) {
	r, err := rcParseRules([]byte("detail: Short\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Detail != "short" {
		t.Errorf("detail = %q, want short (lowercased)", r.Detail)
	}
	if !strings.Contains(func() string { rcActiveRules = r; defer func() { rcActiveRules = nil }(); return rcDescribeRules() }(), "detail short") {
		t.Error("the review log line does not name the detail level")
	}
	if _, err := rcParseRules([]byte("detail: loud\n"), "test"); err == nil || !strings.Contains(err.Error(), "score, short, full") {
		t.Errorf("err = %v, want an error naming the levels", err)
	}
	r, err = rcParseRules([]byte("instructions: hi\n"), "test")
	if err != nil || r.Detail != "" {
		t.Errorf("no detail line: %+v %v, want detail unset", r, err)
	}
}
