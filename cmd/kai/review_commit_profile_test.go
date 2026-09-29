package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/provider"
)

// useProfile makes p the active profile for one test.
func useProfile(t *testing.T, stages map[string]rcStageSetting) {
	t.Helper()
	rcActiveProfile = &rcReviewProfile{Source: "test", Stages: stages}
	t.Cleanup(func() { rcActiveProfile = nil })
}

// The stage-model environment the review job sets, cleared so each test
// states what it depends on.
func clearReviewEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"KAI_FAST_MODEL", "KAI_SWEEP_MODEL", "KAI_CHALLENGE_MODEL", "KAI_REVIEW_REASONING_EFFORT", "KAI_REVIEW_PROFILE", "GITHUB_REPOSITORY_FULLNAME"} {
		t.Setenv(k, "")
	}
}

func TestParseProfileReadsEveryStage(t *testing.T) {
	p, err := rcParseProfile([]byte(`{
		"run_tag": "run9-glm52-low",
		"stages": {
			"quick_draft": {"model": "anthropic/claude-haiku-4-5", "effort": "low"},
			"quick_factcheck": {"model": "z-ai/glm-5.2", "effort": "LOW"},
			"main": {"model": "z-ai/glm-5.3", "effort": "low"},
			"sweep": {"effort": "off"}
		}
	}`), "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[rcStageQuickFactcheck]; got.Model != "z-ai/glm-5.2" || got.Effort != "low" {
		t.Errorf("quick_factcheck = %+v", got)
	}
	if got := p.Stages[rcStageSweep]; got.Model != "" || got.Effort != rcEffortOff {
		t.Errorf("sweep = %+v", got)
	}
	if _, ok := p.Stages[rcStageIntent]; ok {
		t.Error("intent was not in the file but is in the profile")
	}
}

// A typo must not fall back to a default silently: the run would be reported
// under a configuration it never ran.
func TestParseProfileRejectsWhatItCannotHonor(t *testing.T) {
	for name, body := range map[string]string{
		"unknown stage":  `{"stages": {"mian": {"model": "z-ai/glm-5.3"}}}`,
		"unknown field":  `{"stages": {"main": {"modle": "z-ai/glm-5.3"}}}`,
		"bad effort":     `{"stages": {"main": {"effort": "extreme"}}}`,
		"not a model id": `{"stages": {"main": {"model": "glm; rm -rf /"}}}`,
		"not json":       `{"stages": `,
	} {
		if _, err := rcParseProfile([]byte(body), "t"); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// An effort that would be dropped is refused on the model each stage resolves
// to, wherever that model came from: the file, main (intent, conclusion), or
// the job's fallback for a stage the file gives only an effort.
func TestProfileEffortOnABareClaudeModelIsRefused(t *testing.T) {
	clearReviewEnv(t)
	for name, tc := range map[string]struct {
		stages map[string]rcStageSetting
		models map[string]string
		ok     bool
	}{
		"named in the file": {
			stages: map[string]rcStageSetting{rcStageQuickDraft: {Model: "claude-haiku-4-5-20251001", Effort: "low"}},
			models: map[string]string{rcStageQuickDraft: "claude-haiku-4-5-20251001"},
		},
		"inherited from main": {
			stages: map[string]rcStageSetting{rcStageMain: {Model: "claude-sonnet-4-6", Effort: "low"}},
			models: map[string]string{rcStageMain: "claude-sonnet-4-6", rcStageIntent: "claude-sonnet-4-6"},
		},
		"the job's fallback model": {
			stages: map[string]rcStageSetting{rcStageSweep: {Effort: "high"}},
			models: map[string]string{rcStageSweep: "claude-sonnet-4-6"},
		},
		"vendor-prefixed carries it": {
			stages: map[string]rcStageSetting{rcStageQuickDraft: {Effort: "low"}},
			models: map[string]string{rcStageQuickDraft: "anthropic/claude-haiku-4-5"},
			ok:     true,
		},
		"effort off": {
			stages: map[string]rcStageSetting{rcStageSweep: {Effort: rcEffortOff}},
			models: map[string]string{rcStageSweep: "claude-sonnet-4-6"},
			ok:     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			useProfile(t, tc.stages)
			if err := rcCheckProfileEfforts(tc.models); (err == nil) != tc.ok {
				t.Errorf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	// Without a profile the job's own effort is not policed here.
	rcActiveProfile = nil
	t.Setenv("KAI_REVIEW_REASONING_EFFORT", "low")
	if err := rcCheckProfileEfforts(map[string]string{rcStageMain: "claude-sonnet-4-6"}); err != nil {
		t.Errorf("no profile: %v", err)
	}
}

// Without a profile every stage resolves exactly as before this change.
func TestNoProfileChangesNothing(t *testing.T) {
	clearReviewEnv(t)
	rcActiveProfile = nil
	t.Setenv("KAI_REVIEW_REASONING_EFFORT", "low")
	t.Setenv("KAI_SWEEP_MODEL", "sweep/model")
	t.Setenv("KAI_CHALLENGE_MODEL", "check/model")
	for stage, want := range map[string]string{
		rcStageQuickDraft: "", rcStageQuickFactcheck: "",
		rcStageIntent: "low", rcStageMain: "low", rcStageSweep: "low", rcStageFactcheck: "low", rcStageConclusion: "low",
	} {
		if got := rcStageEffort(stage); got != want {
			t.Errorf("effort(%s) = %q, want %q", stage, got, want)
		}
	}
	if got := rcSweepModel("z-ai/glm-5.2"); got != "sweep/model" {
		t.Errorf("sweep model = %q", got)
	}
	if got := rcChallengeModel("z-ai/glm-5.2"); got != "check/model" {
		t.Errorf("challenge model = %q", got)
	}
	if got := rcFastChallengeModel("z-ai/glm-5.2"); got != "check/model" {
		t.Errorf("fast challenge model = %q, want the fact-check model it has always shared", got)
	}
	if got := rcStageModel(rcStageIntent, "z-ai/glm-5.2"); got != "z-ai/glm-5.2" {
		t.Errorf("intent model = %q", got)
	}
	if rcDescribeProfile(nil) != "" {
		t.Error("a log line without a profile")
	}
}

// A profile wins over the job's environment, stage by stage, "off" included.
func TestProfileOverridesTheJobEnvironment(t *testing.T) {
	clearReviewEnv(t)
	t.Setenv("KAI_REVIEW_REASONING_EFFORT", "high")
	t.Setenv("KAI_FAST_MODEL", "env/fast")
	t.Setenv("KAI_SWEEP_MODEL", "env/sweep")
	t.Setenv("KAI_CHALLENGE_MODEL", "env/check")
	useProfile(t, map[string]rcStageSetting{
		rcStageQuickDraft:     {Model: "anthropic/claude-haiku-4-5", Effort: "low"},
		rcStageQuickFactcheck: {Effort: "low"},
		rcStageMain:           {Model: "z-ai/glm-5.3", Effort: "low"},
		rcStageSweep:          {Model: "z-ai/glm-5.2", Effort: rcEffortOff},
		rcStageFactcheck:      {Model: "z-ai/glm-5.2"},
	})
	if got := rcFastModel("z-ai/glm-5.2", provider.KindKailab); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("quick draft model = %q", got)
	}
	if got := rcStageModel(rcStageMain, "z-ai/glm-5.2"); got != "z-ai/glm-5.3" {
		t.Errorf("main model = %q", got)
	}
	if got := rcSweepModel("z-ai/glm-5.3"); got != "z-ai/glm-5.2" {
		t.Errorf("sweep model = %q", got)
	}
	if got := rcChallengeModel("z-ai/glm-5.3"); got != "z-ai/glm-5.2" {
		t.Errorf("fact-check model = %q", got)
	}
	// quick_factcheck sets no model, so it follows the fact-check's.
	if got := rcFastChallengeModel("z-ai/glm-5.3"); got != "z-ai/glm-5.2" {
		t.Errorf("quick fact-check model = %q", got)
	}
	// Intent and conclusion set nothing, so they follow main.
	if got := rcStageModel(rcStageConclusion, "z-ai/glm-5.2"); got != "z-ai/glm-5.3" {
		t.Errorf("conclusion model = %q", got)
	}
	for stage, want := range map[string]string{
		rcStageQuickDraft: "low", rcStageQuickFactcheck: "low",
		rcStageIntent: "low", rcStageMain: "low", rcStageConclusion: "low",
		rcStageSweep:     "",     // explicit off beats the job's "high"
		rcStageFactcheck: "high", // no effort in the profile: the job's stays
	} {
		if got := rcStageEffort(stage); got != want {
			t.Errorf("effort(%s) = %q, want %q", stage, got, want)
		}
	}
	line := rcDescribeProfile(map[string]string{rcStageMain: "z-ai/glm-5.3", rcStageSweep: "z-ai/glm-5.2"})
	for _, want := range []string{"review profile test", "main=z-ai/glm-5.3/low", "sweep=z-ai/glm-5.2/off"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q: %s", want, line)
		}
	}
}

// The fact-check call carries its stage's effort: the fast pass's (no
// repository on the context) and the grounded one's separately.
func TestGateEffortFollowsTheProfile(t *testing.T) {
	clearReviewEnv(t)
	useProfile(t, map[string]rcStageSetting{
		rcStageQuickFactcheck: {Effort: "low"},
		rcStageFactcheck:      {Effort: "medium"},
	})
	if got := rcGateEffort(context.Background()); got != "low" {
		t.Errorf("fast gate effort = %q, want low", got)
	}
	if got := rcGateEffort(rcWithRepo(context.Background(), &rcRepo{})); got != "medium" {
		t.Errorf("grounded gate effort = %q, want medium", got)
	}
}

// profileRepo makes a repository whose base branch carries body at the
// profile path (none when body is empty) and whose head does not, the way the
// harness lays out a benchmark repo.
func profileRepo(t *testing.T, body string) string {
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
	git("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "--quiet", "-m", "base")
	git("checkout", "--quiet", "-b", "pr")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "-am", "change")
	git("checkout", "--quiet", "main")
	if body != "" {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rcProfilePath)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rcProfilePath), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", rcProfilePath)
		git("commit", "--quiet", "-m", "profile")
	}
	git("checkout", "--quiet", "pr")
	return dir
}

func inDir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// The file is read from the base branch, and only in the benchmark org.
func TestLoadProfileFromTheBaseBranchOfABenchmarkRepo(t *testing.T) {
	clearReviewEnv(t)
	t.Cleanup(func() { rcActiveProfile = nil })
	inDir(t, profileRepo(t, `{"stages": {"main": {"model": "z-ai/glm-5.3", "effort": "low"}}}`))

	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "kaicontextbench/sentry__sentry__kai-run9__PR1__20260929")
	if err := rcLoadProfileFor("main"); err != nil {
		t.Fatal(err)
	}
	if rcActiveProfile == nil || rcActiveProfile.Stages[rcStageMain].Model != "z-ai/glm-5.3" {
		t.Fatalf("profile = %+v, want main on z-ai/glm-5.3", rcActiveProfile)
	}
	if !strings.Contains(rcActiveProfile.Source, "kaicontextbench/") {
		t.Errorf("source = %q, want the repository named", rcActiveProfile.Source)
	}

	// A customer repository carrying the same file is reviewed as always.
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "acme/api")
	if err := rcLoadProfileFor("main"); err != nil || rcActiveProfile != nil {
		t.Fatalf("outside the benchmark org: profile %+v, err %v; want none", rcActiveProfile, err)
	}
	// So is a local run, which has no GitHub environment.
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "")
	if err := rcLoadProfileFor("main"); err != nil || rcActiveProfile != nil {
		t.Fatalf("local run: profile %+v, err %v; want none", rcActiveProfile, err)
	}
}

func TestLoadProfileWithoutAFileIsNoProfile(t *testing.T) {
	clearReviewEnv(t)
	t.Cleanup(func() { rcActiveProfile = nil })
	inDir(t, profileRepo(t, ""))
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "kaicontextbench/repo")
	if err := rcLoadProfileFor("main"); err != nil || rcActiveProfile != nil {
		t.Fatalf("profile %+v, err %v; want none", rcActiveProfile, err)
	}
}

// A broken file fails the review loudly rather than running on defaults.
func TestLoadProfileRejectsABrokenFile(t *testing.T) {
	clearReviewEnv(t)
	t.Cleanup(func() { rcActiveProfile = nil })
	inDir(t, profileRepo(t, `{"stages": {"main": {"model": "z-ai/glm-5.3", "effort": "extreme"}}}`))
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "kaicontextbench/repo")
	if err := rcLoadProfileFor("main"); err == nil {
		t.Fatal("a broken profile loaded without error")
	}
}

// KAI_REVIEW_PROFILE points at a local file, for trying a profile by hand.
func TestLoadProfileFromALocalFile(t *testing.T) {
	clearReviewEnv(t)
	t.Cleanup(func() { rcActiveProfile = nil })
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"stages": {"sweep": {"effort": "high"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAI_REVIEW_PROFILE", path)
	if err := rcLoadProfileFor(""); err != nil {
		t.Fatal(err)
	}
	if got := rcStageEffort(rcStageSweep); got != "high" {
		t.Errorf("sweep effort = %q, want high", got)
	}
}
