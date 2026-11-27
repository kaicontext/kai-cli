package cirun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goldenDir = "../../testdata/ci"

func loadGolden(t *testing.T) []*Workflow {
	t.Helper()
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	var wfs []*Workflow
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		wf, err := ParseWorkflow(e.Name(), data)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		wf.Dir = WorkflowDirs[0]
		wfs = append(wfs, wf)
	}
	for _, wf := range wfs {
		wf.siblings = wfs
	}
	return wfs
}

func TestGoldenFidelity(t *testing.T) {
	wfs := loadGolden(t)
	opts := ClassifyOptions{Images: ImageMap("")}
	cases := []struct {
		target  string
		bucket  Bucket
		want    string // substring of the reason or of some caveat
		partial bool
	}{
		{"run-only.yml:test", BucketRuns, "", false},
		{"actions.yml:composite", BucketRuns, "", false},
		{"actions.yml:node", BucketCaveats, "unpinned actions run on this machine: actions/checkout@v5", false},
		{"actions.yml:docker", BucketCaveats, "actions/cache uses a local cache", false},
		{"matrix.yml:linux-only", BucketRuns, "", false},
		{"matrix.yml:mixed", BucketCaveats, "matrix entries on macos-latest, windows-latest are not run here", true},
		{"matrix.yml:dynamic", BucketCannot, "can't be resolved before the run", false},
		{"needs.yml:package", BucketRuns, "", false},
		{"needs.yml:mac", BucketCannot, "runs-on macos-14", false},
		{"needs.yml:notarize", BucketCannot, "needs mac, which can't run here (runs-on macos-14)", false},
		{"reusable.yml:local", BucketRuns, "", false},
		{"reusable.yml:remote", BucketCaveats, "reusable workflow in another repo", false},
		// A local reusable workflow is checked job by job.
		{"reusable.yml:local-mac", BucketCannot, "whose job mac can't run here (runs-on macos-14)", false},
		{"nested.yml:local-nested", BucketCannot, "whose job local-mac can't run here", false},
		{"reusable.yml:local-missing", BucketCaveats, "which Kai couldn't read to check", false},
		// exclude: removes the only macOS combination, so nothing is partial.
		{"matrix.yml:excluded", BucketRuns, "", false},
		{"matrix.yml:excluded-partly", BucketCaveats, "matrix entries on macos-latest are not run here", true},
		{"limits.yml:services", BucketCaveats, "services: run as local containers", false},
		{"limits.yml:mac", BucketCannot, "runs-on macos-latest", false},
		{"limits.yml:windows", BucketCannot, "runs-on windows-latest", false},
		{"limits.yml:self-hosted", BucketCannot, "not a GitHub-hosted Ubuntu runner", false},
		{"limits.yml:oidc", BucketCannot, "id-token: write", false},
		{"limits.yml:deploy", BucketCannot, `environment "production"`, false},
		{"limits.yml:secret", BucketCaveats, "secrets.NPM_TOKEN, secrets.REGISTRY_TOKEN are empty here", false},
	}
	for _, c := range cases {
		j, err := FindJob(wfs, c.target)
		if err != nil {
			t.Fatal(err)
		}
		f := Resolve(j, opts)
		if f.Bucket != c.bucket || f.Partial != c.partial {
			t.Errorf("%s: bucket %s partial %v, want %s partial %v (%+v)", c.target, f.Bucket, f.Partial, c.bucket, c.partial, f)
		}
		if c.want != "" && !strings.Contains(f.Reason+"\n"+strings.Join(f.Caveats, "\n"), c.want) {
			t.Errorf("%s: want %q in %+v", c.target, c.want, f)
		}
	}

	// A secret with a local value is not a caveat.
	j, _ := FindJob(wfs, "limits.yml:secret")
	f := Classify(j, ClassifyOptions{Images: ImageMap(""), HaveSecrets: map[string]bool{"NPM_TOKEN": true, "REGISTRY_TOKEN": true}})
	if f.Bucket != BucketRuns {
		t.Errorf("secrets supplied: %+v", f)
	}
}

func TestFindJob(t *testing.T) {
	wfs := loadGolden(t)
	if j, err := FindJob(wfs, "needs:lint"); err != nil || j.Ref() != "needs.yml:lint" {
		t.Errorf("needs:lint → %v, %v", j, err)
	}
	if _, err := FindJob(wfs, "test"); err == nil || !strings.Contains(err.Error(), "more than one workflow") {
		t.Errorf("ambiguous bare job: %v", err)
	}
	if _, err := FindJob(wfs, "nope"); err == nil || !strings.Contains(err.Error(), "run-only.yml:test") {
		t.Errorf("unknown job should list jobs: %v", err)
	}
}

func TestTriggered(t *testing.T) {
	wfs := loadGolden(t)
	wf := func(file string) *Workflow {
		for _, w := range wfs {
			if w.File == file {
				return w
			}
		}
		t.Fatalf("no %s", file)
		return nil
	}
	cases := []struct {
		file string
		ev   EventContext
		ok   bool
		why  string
	}{
		{"run-only.yml", EventContext{Event: "push", Branch: "feat/x"}, true, ""},
		{"actions.yml", EventContext{Event: "push", Branch: "feat/x"}, false, "push to feat/x doesn't match branches [main]"},
		{"actions.yml", EventContext{Event: "push", Branch: "main"}, true, ""},
		{"actions.yml", EventContext{Event: "pull_request", Branch: "feat/x", BaseBranch: "main"}, true, ""},
		{"actions.yml", EventContext{Event: "pull_request", Branch: "feat/x", BaseBranch: "release"}, false, "pull request into release"},
		{"reusable.yml", EventContext{Event: "push", Branch: "main"}, false, "tags only"},
		{"limits.yml", EventContext{Event: "push", Branch: "main"}, false, "not triggered by push"},
		{"needs.yml", EventContext{Event: "push", Branch: "x", ChangedKnown: true, Changed: []string{"src/a/b.go"}}, true, ""},
		{"needs.yml", EventContext{Event: "push", Branch: "x", ChangedKnown: true, Changed: []string{"src/docs/a.md", "README.md"}}, false, "no changed file matches its paths"},
		// No base to diff against: can't prove it's unaffected, so it runs.
		{"needs.yml", EventContext{Event: "push", Branch: "x"}, true, ""},
		{"limits.yml", EventContext{Event: "pull_request", BaseBranch: "main", ChangedKnown: true, Changed: []string{"docs/a.txt", "pkg/README.md"}}, false, "paths-ignore"},
		{"limits.yml", EventContext{Event: "pull_request", BaseBranch: "main", ChangedKnown: true, Changed: []string{"docs/a.txt", "main.go"}}, true, ""},
	}
	for i, c := range cases {
		ok, why := Triggered(wf(c.file), c.ev)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("case %d %s: got %v %q, want %v %q", i, c.file, ok, why, c.ok, c.why)
		}
	}
}

func TestMatchPatterns(t *testing.T) {
	cases := []struct {
		patterns []string
		name     string
		want     bool
	}{
		{[]string{"*.js"}, "a.js", true},
		{[]string{"*.js"}, "src/a.js", false},
		{[]string{"**.js"}, "src/a.js", true},
		{[]string{"docs/**"}, "docs/a/b.md", true},
		{[]string{"**/docs/**"}, "docs/a.md", true},
		{[]string{"**/docs/**"}, "x/docs/a.md", true},
		{[]string{"releases/**", "!releases/**-alpha"}, "releases/v1-alpha", false},
		{[]string{"releases/**", "!releases/**-alpha"}, "releases/v1", true},
		{[]string{"feature/*"}, "feature/a/b", false},
		{[]string{"v[12].[0-9]+.[0-9]+"}, "v1.10.3", true},
		{[]string{"main"}, "maintenance", false},
	}
	for _, c := range cases {
		if got := matchPatterns(c.patterns, c.name); got != c.want {
			t.Errorf("%v vs %q: got %v", c.patterns, c.name, got)
		}
	}
}

// actLines builds `act --json` output for tests.
type actLines []string

func (a *actLines) add(fields map[string]any) {
	if _, ok := fields["level"]; !ok {
		fields["level"] = "info"
	}
	b, _ := json.Marshal(fields)
	*a = append(*a, string(b)+"\n")
}

func (a *actLines) job(id string, ok bool, failStep string) {
	a.add(map[string]any{"jobID": id, "msg": "🚀  Start image", "time": "2026-11-27T10:00:00Z"})
	a.add(map[string]any{"jobID": id, "stage": "Main", "step": "Checkout", "stepResult": "success", "msg": "  ✅  Success - Main Checkout"})
	if failStep != "" {
		a.add(map[string]any{"jobID": id, "stage": "Main", "step": failStep, "raw_output": true, "msg": "--- FAIL: TestThing (0.00s)\n"})
		a.add(map[string]any{"jobID": id, "stage": "Main", "step": failStep, "raw_output": true, "msg": "FAIL\n"})
		a.add(map[string]any{"jobID": id, "stage": "Main", "step": failStep, "level": "error", "msg": "path not located inside a git repository"})
		a.add(map[string]any{"jobID": id, "stage": "Main", "step": failStep, "level": "error", "msg": "exitcode '1': failure"})
		a.add(map[string]any{"jobID": id, "stage": "Main", "step": failStep, "stepResult": "failure", "level": "error", "msg": "  ❌  Failure - Main " + failStep})
	}
	result := "success"
	if !ok {
		result = "failure"
	}
	a.add(map[string]any{"jobID": id, "jobResult": result, "msg": "🏁  Job", "time": "2026-11-27T10:02:14Z"})
}

func TestStreamParser(t *testing.T) {
	var lines actLines
	lines.job("test", false, "Run tests")
	lines.job("lint", true, "")
	// One matrix entry failing fails the job, whatever order they report in.
	lines.add(map[string]any{"jobID": "matrix", "jobResult": "failure"})
	lines.add(map[string]any{"jobID": "matrix", "jobResult": "success"})
	lines.add(map[string]any{"jobID": "setup", "stepResult": "failure", "msg": "  ❌  Failure - Set up job"})
	lines.add(map[string]any{"jobID": "script", "stage": "Main", "step": "echo one\nexit 3\n", "stepResult": "failure"})
	p := newStreamParser()
	p.feed("Error: Cannot connect to the Docker daemon\n")
	for _, l := range lines {
		p.feed(l)
	}
	if js := p.jobs["test"]; js.result != "failure" || js.failedStep != "Run tests" || strings.Join(js.failedLog, "|") != "--- FAIL: TestThing (0.00s)|FAIL|exitcode '1': failure" {
		t.Errorf("test: %+v", js)
	}
	if js := p.jobs["test"]; js.last.Sub(js.first).Seconds() != 134 {
		t.Errorf("duration: %v", js.last.Sub(js.first))
	}
	if p.jobs["lint"].result != "success" || p.jobs["matrix"].result != "failure" {
		t.Errorf("lint %q matrix %q", p.jobs["lint"].result, p.jobs["matrix"].result)
	}
	if p.jobs["setup"].failedStep != "Set up job" {
		t.Errorf("setup: %+v", p.jobs["setup"])
	}
	if p.jobs["script"].failedStep != "echo one …" {
		t.Errorf("unnamed multi-line step: %q", p.jobs["script"].failedStep)
	}
	if len(p.other) != 1 || !strings.Contains(p.other[0], "Docker daemon") {
		t.Errorf("other: %v", p.other)
	}
}

// testRepo makes a git repo on branch feat/x holding the golden workflows,
// with main as the base.
func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	wfDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(goldenDir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wfDir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-q", "-m", "workflows")
	run("checkout", "-q", "-b", "feat/x")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")
	return dir
}

type fakeAct struct {
	calls []actInvocation
	// respond returns the lines and exit code for an invocation of job id.
	respond func(jobID string) (actLines, int)
}

func (f *fakeAct) options(dir string, o Options) Options {
	o.Dir = dir
	o.KaiHome = filepath.Join(dir, ".kaihome")
	o.resolveAct = func(context.Context, string, string, func(string)) (*ActTool, error) {
		return &ActTool{Path: "act", Version: ActVersion, Source: "managed"}, nil
	}
	o.detectRuntime = func() (*ContainerRuntime, error) { return &ContainerRuntime{Name: "Docker"}, nil }
	o.execAct = func(_ context.Context, inv actInvocation, onLine func(string)) (int, error) {
		f.calls = append(f.calls, inv)
		job := ""
		for i, a := range inv.args {
			if a == "-j" {
				job = inv.args[i+1]
			}
		}
		lines, code := f.respond(job)
		for _, l := range lines {
			onLine(l)
		}
		return code, nil
	}
	return o
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func rowOf(t *testing.T, rep *Report, id string) *JobResult {
	t.Helper()
	for _, j := range rep.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("no row %s in %+v", id, rep.Jobs)
	return nil
}

func TestRunPassAndFail(t *testing.T) {
	dir := testRepo(t)
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		return l, 0
	}}
	rep, err := Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}}))
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, rep, "run-only.yml:test"); r.Status != StatusPassed || r.DurationMS != 134000 || rep.ExitCode != 0 {
		t.Errorf("pass: %+v exit %d", r, rep.ExitCode)
	}
	if rep.Tree != "working" || rep.SHA == "" || rep.Branch != "feat/x" || rep.Base != "main" || rep.ActVersion != ActVersion {
		t.Errorf("report header: %+v", rep)
	}
	args := strings.Join(fake.calls[0].args, " ")
	for _, want := range []string{
		"push --json", "-j test", "--container-daemon-socket -",
		"-P ubuntu-latest=catthehacker/ubuntu:act-latest", "-W " + filepath.Join(dir, ".github", "workflows", "run-only.yml"),
	} {
		if !strings.Contains(args, want) {
			t.Errorf("act args lack %q: %s", want, args)
		}
	}
	// The repo's dotfiles must not reach the job: every file flag points at
	// an empty file, and no token or secret is passed.
	for _, flag := range []string{"--secret-file", "--env-file", "--var-file", "--input-file"} {
		if !strings.Contains(args, flag+" ") {
			t.Errorf("act args lack %s: %s", flag, args)
		}
	}
	// The only secret on the command line is an empty GITHUB_TOKEN, which
	// stops act from fetching one with `gh auth token`.
	if strings.Count(args, " -s ") != 1 || !strings.Contains(args, " -s GITHUB_TOKEN= ") {
		t.Errorf("act must get exactly one secret, an empty GITHUB_TOKEN: %s", args)
	}
	// act starts outside the repo, so it reads no .actrc from it.
	if d := fake.calls[0].dir; d == dir || strings.HasPrefix(d, dir+string(filepath.Separator)) {
		t.Errorf("act's working directory %s is inside the repo", d)
	}

	fake = &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, false, "Run tests")
		return l, 1
	}}
	rep, err = Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}, DockerSocket: true}))
	if err != nil {
		t.Fatal(err)
	}
	r := rowOf(t, rep, "run-only.yml:test")
	if r.Status != StatusFailed || r.FailedStep != "Run tests" || !strings.Contains(r.LogExcerpt, "FAIL: TestThing") || rep.ExitCode != 1 {
		t.Errorf("fail: %+v exit %d", r, rep.ExitCode)
	}
	if strings.Contains(strings.Join(fake.calls[0].args, " "), "--container-daemon-socket") {
		t.Errorf("--docker-socket should leave the socket mounted")
	}
	var out bytes.Buffer
	rep.WriteSummary(&out)
	if !strings.Contains(out.String(), `run-only.yml:test  failed   2m14s   step "Run tests"`) {
		t.Errorf("summary:\n%s", out.String())
	}
}

func TestRunNotRunIsNeverPassed(t *testing.T) {
	dir := testRepo(t)
	fake := &fakeAct{respond: func(string) (actLines, int) {
		t.Error("act must not be invoked for a job that can't run here")
		return nil, 0
	}}
	o := fake.options(dir, Options{Targets: []string{"limits.yml:mac", "needs.yml:notarize"}})
	// No runtime and no act are needed to say a job can't run.
	o.detectRuntime = func() (*ContainerRuntime, error) { return nil, ErrNoRuntime }
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, rep, "limits.yml:mac"); r.Status != StatusNotRun || r.Reason != "runs-on macos-latest" {
		t.Errorf("mac: %+v", r)
	}
	if r := rowOf(t, rep, "needs.yml:notarize"); r.Status != StatusNotRun {
		t.Errorf("notarize: %+v", r)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit %d, want 0 without --strict", rep.ExitCode)
	}
	o.Strict = true
	if rep, _ = Run(context.Background(), o); rep.ExitCode != 1 {
		t.Errorf("--strict exit %d, want 1", rep.ExitCode)
	}
}

func TestRunNeedsChain(t *testing.T) {
	dir := testRepo(t)
	// lint passes, test fails, so act never reaches package.
	fake := &fakeAct{respond: func(string) (actLines, int) {
		var l actLines
		l.job("lint", true, "")
		l.job("test", false, "Run echo test")
		return l, 1
	}}
	rep, err := Run(context.Background(), fake.options(dir, Options{Targets: []string{"needs.yml:package", "needs.yml:lint"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("%d act invocations, want 1 (lint runs inside package's)", len(fake.calls))
	}
	if r := rowOf(t, rep, "needs.yml:lint"); r.Status != StatusPassed {
		t.Errorf("lint: %+v", r)
	}
	if r := rowOf(t, rep, "needs.yml:test"); r.Status != StatusFailed || r.Why != "needed by needs.yml:package" {
		t.Errorf("test: %+v", r)
	}
	if r := rowOf(t, rep, "needs.yml:package"); r.Status != StatusNotRun || r.Reason != "needs test, which failed" {
		t.Errorf("package: %+v", r)
	}
	if rep.ExitCode != 1 {
		t.Errorf("exit %d", rep.ExitCode)
	}
}

func TestRunNoResultIsNotAPass(t *testing.T) {
	dir := testRepo(t)
	// act exits 0 having said nothing about the job (its if: was false).
	fake := &fakeAct{respond: func(string) (actLines, int) { return nil, 0 }}
	rep, _ := Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}}))
	if r := rowOf(t, rep, "run-only.yml:test"); r.Status != StatusSkipped {
		t.Errorf("silent exit 0: %+v", r)
	}
	// act dies before the job reports.
	fake = &fakeAct{respond: func(string) (actLines, int) {
		return actLines{"Error: Cannot connect to the Docker daemon\n"}, 1
	}}
	rep, _ = Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}}))
	r := rowOf(t, rep, "run-only.yml:test")
	if r.Status != StatusError || !strings.Contains(r.LogExcerpt, "Docker daemon") || rep.ExitCode != 1 {
		t.Errorf("act error: %+v exit %d", r, rep.ExitCode)
	}
}

func TestRunAll(t *testing.T) {
	dir := testRepo(t)
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		for _, dep := range map[string][]string{"package": {"lint", "test"}}[job] {
			l.job(dep, true, "")
		}
		return l, 0
	}}
	var progress bytes.Buffer
	rep, err := Run(context.Background(), fake.options(dir, Options{All: true, Progress: &progress}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Status{
		"run-only.yml:test":   StatusPassed,
		"actions.yml:node":    StatusSkipped, // push to feat/x, branches [main]
		"needs.yml:package":   StatusPassed,  // src/a.go matches its paths
		"needs.yml:mac":       StatusNotRun,
		"needs.yml:notarize":  StatusNotRun,
		"reusable.yml:local":  StatusSkipped, // tags only
		"limits.yml:services": StatusSkipped, // pull_request only
		"matrix.yml:mixed":    StatusPassed,
		"matrix.yml:dynamic":  StatusNotRun,
	}
	for id, st := range want {
		if r := rowOf(t, rep, id); r.Status != st {
			t.Errorf("%s: %s (%s), want %s", id, r.Status, r.Reason, st)
		}
	}
	if !rowOf(t, rep, "matrix.yml:mixed").Partial {
		t.Error("mixed matrix should be partial")
	}
	if !strings.Contains(progress.String(), "skipped  actions.yml:node: push to feat/x doesn't match branches [main]") {
		t.Errorf("progress should explain skips:\n%s", progress.String())
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit %d", rep.ExitCode)
	}
	// A partly-run matrix is not full coverage.
	o := fake.options(dir, Options{Targets: []string{"matrix.yml:mixed"}, Strict: true})
	if rep, _ = Run(context.Background(), o); rep.ExitCode != 1 {
		t.Errorf("--strict with a partial matrix: exit %d", rep.ExitCode)
	}
}

func TestRunTreeHead(t *testing.T) {
	dir := testRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var runDir string
	fake := &fakeAct{}
	fake.respond = func(job string) (actLines, int) {
		if len(fake.calls) == 1 { // the --tree head run
			runDir = argAfter(fake.calls[0].args, "-C")
			if _, err := os.Stat(filepath.Join(runDir, "uncommitted.txt")); err == nil {
				t.Error("--tree head ran with uncommitted files present")
			}
			if _, err := os.Stat(filepath.Join(runDir, "src", "a.go")); err != nil {
				t.Errorf("--tree head lacks committed files: %v", err)
			}
			// act needs a real .git directory to read the commit from.
			if fi, err := os.Stat(filepath.Join(runDir, ".git")); err != nil || !fi.IsDir() {
				t.Errorf("--tree head checkout has no .git directory: %v", err)
			}
		}
		var l actLines
		l.job(job, true, "")
		return l, 0
	}
	rep, err := Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}, Tree: "head"}))
	if err != nil {
		t.Fatal(err)
	}
	if runDir == "" || runDir == dir || rep.Tree != "head" || rep.Dirty {
		t.Errorf("runDir %q tree %s dirty %v", runDir, rep.Tree, rep.Dirty)
	}
	if _, err := os.Stat(runDir); err == nil {
		t.Errorf("head checkout %s was not cleaned up", runDir)
	}
	if branch, _ := git(context.Background(), dir, "symbolic-ref", "--short", "HEAD"); branch != "feat/x" {
		t.Errorf("the repo's own checkout moved to %q", branch)
	}

	rep, _ = Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}}))
	if !rep.Dirty {
		t.Error("working tree run should record that it was dirty")
	}
}

func TestRunNeedsSelection(t *testing.T) {
	dir := testRepo(t)
	if _, err := Run(context.Background(), (&fakeAct{}).options(dir, Options{})); err != ErrNoSelection {
		t.Errorf("got %v", err)
	}
}

func TestBuildEvent(t *testing.T) {
	gi := &GitInfo{Branch: "feat/x", HeadSHA: "abc", BaseSHA: "def", DefaultBranch: "main", BaseBranch: "main", Repo: "kaicontext/kai-cli", UserName: "Dev"}
	var push struct {
		Ref, Before, After string
		Repository         struct {
			FullName string `json:"full_name"`
			Owner    struct{ Login string }
		}
	}
	b, err := BuildEvent("push", gi)
	if err != nil || json.Unmarshal(b, &push) != nil {
		t.Fatal(err)
	}
	if push.Ref != "refs/heads/feat/x" || push.Before != "def" || push.After != "abc" || push.Repository.FullName != "kaicontext/kai-cli" || push.Repository.Owner.Login != "kaicontext" {
		t.Errorf("push: %s", b)
	}
	var pr struct {
		Number      int
		PullRequest struct {
			Base, Head struct{ Ref, SHA string }
		} `json:"pull_request"`
	}
	b, _ = BuildEvent("pull_request", gi)
	if json.Unmarshal(b, &pr) != nil || pr.PullRequest.Base.Ref != "main" || pr.PullRequest.Base.SHA != "def" || pr.PullRequest.Head.Ref != "feat/x" || pr.PullRequest.Head.SHA != "abc" {
		t.Errorf("pull_request: %s", b)
	}
	for url, want := range map[string]string{
		"git@github.com:kaicontext/kai-cli.git":     "kaicontext/kai-cli",
		"https://github.com/kaicontext/kai-cli":     "kaicontext/kai-cli",
		"ssh://git@github.com/kaicontext/kai-cli/":  "kaicontext/kai-cli",
		"https://x:tok@github.com/kaicontext/a.git": "kaicontext/a",
	} {
		if got := repoFromURL(url); got != want {
			t.Errorf("repoFromURL(%s) = %s", url, got)
		}
	}
}

func TestDownloadActVerifiesChecksum(t *testing.T) {
	asset, err := actAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil || strings.HasSuffix(asset, ".zip") {
		t.Skip("no tar.gz asset for this platform")
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho act version " + ActVersion + "\n")
	for _, f := range []struct {
		name string
		data []byte
	}{{"LICENSE", []byte("MIT")}, {"act", body}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(f.data)
	}
	tw.Close()
	gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fmt.Sprintf("/v%s/%s", ActVersion, asset) {
			http.NotFound(w, r)
			return
		}
		w.Write(archive.Bytes())
	}))
	defer srv.Close()
	oldURL, oldSum := actBaseURL, actSHA256[asset]
	actBaseURL = srv.URL
	defer func() { actBaseURL, actSHA256[asset] = oldURL, oldSum }()

	// The pinned digest is for the real release, so this archive is refused.
	home := t.TempDir()
	if _, err := ResolveActManaged(context.Background(), home); err == nil || !strings.Contains(err.Error(), "refusing to install") {
		t.Fatalf("mismatched archive: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(ManagedActPath(home))); len(entries) != 0 {
		t.Errorf("a refused download left files behind: %v", entries)
	}

	sum := sha256.Sum256(archive.Bytes())
	actSHA256[asset] = hex.EncodeToString(sum[:])
	tool, err := ResolveActManaged(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Version != ActVersion || tool.Source != "managed" || tool.Warning != "" {
		t.Errorf("tool: %+v", tool)
	}
	if got, _ := os.ReadFile(tool.Path); !bytes.Equal(got, body) {
		t.Errorf("installed binary differs")
	}
}

func TestActEnvKeepsCredentialsOut(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/dev", "GITHUB_TOKEN=ghp_live", "GH_TOKEN=ghp_live2",
		"NPM_TOKEN=npm_live", "AWS_SECRET_ACCESS_KEY=aws", "ANTHROPIC_API_KEY=sk", "LC_ALL=C",
		"DOCKER_HOST=unix:///old.sock", "https_proxy=http://proxy:3128", "XDG_CONFIG_HOME=/home/dev/.config",
	}
	env := actEnv(environ, "/scratch/home", &ContainerRuntime{Host: "unix:///new.sock"})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("%s set twice", k)
		}
		got[k] = v
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "NPM_TOKEN", "AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached act's environment", k)
		}
	}
	want := map[string]string{
		"PATH": "/usr/bin", "LC_ALL": "C", "https_proxy": "http://proxy:3128",
		// No user or project config is readable from here.
		"HOME": "/scratch/home", "XDG_CONFIG_HOME": filepath.Join("/scratch/home", ".config"),
		// Registry logins still are.
		"DOCKER_CONFIG": filepath.Join("/home/dev", ".docker"),
		"DOCKER_HOST":   "unix:///new.sock",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRunPassesNoHostEnvironment(t *testing.T) {
	dir := testRepo(t)
	t.Setenv("GITHUB_TOKEN", "ghp_live")
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		return l, 0
	}}
	if _, err := Run(context.Background(), fake.options(dir, Options{Targets: []string{"run-only.yml:test"}})); err != nil {
		t.Fatal(err)
	}
	for _, kv := range fake.calls[0].env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Errorf("act was started with %s", kv)
		}
		if home, ok := strings.CutPrefix(kv, "HOME="); ok {
			if real, _ := os.UserHomeDir(); home == real {
				t.Errorf("act was started with the real HOME, where ~/.actrc lives")
			}
		}
	}
}

func TestSameNamedWorkflowsInBothDirs(t *testing.T) {
	dir := testRepo(t)
	kailab := filepath.Join(dir, ".kailab", "workflows")
	if err := os.MkdirAll(kailab, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(goldenDir, "run-only.yml"))
	if err := os.WriteFile(filepath.Join(kailab, "run-only.yml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	wfs, err := LoadWorkflows(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FindJob(wfs, "run-only.yml:test"); err == nil ||
		!strings.Contains(err.Error(), "run-only.yml:test, .kailab/workflows/run-only.yml:test") {
		t.Errorf("ambiguity should name two different refs: %v", err)
	}
	j, err := FindJob(wfs, ".kailab/workflows/run-only.yml:test")
	if err != nil || j.Workflow.Dir != ".kailab/workflows" {
		t.Fatalf("full path target: %v, %v", j, err)
	}
	// --all gives each of them its own row and its own run.
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		return l, 0
	}}
	rep, err := Run(context.Background(), fake.options(dir, Options{All: true}))
	if err != nil {
		t.Fatal(err)
	}
	if rowOf(t, rep, "run-only.yml:test").Status != StatusPassed || rowOf(t, rep, ".kailab/workflows/run-only.yml:test").Status != StatusPassed {
		t.Errorf("rows: %+v", rep.Jobs)
	}
}

func TestInvalidFilterIsAnErrorNotASkip(t *testing.T) {
	dir := testRepo(t)
	bad := "name: Bad\non:\n  push:\n    paths: ['src/[z-a]*']\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "bad.yml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		return l, 0
	}}
	rep, err := Run(context.Background(), fake.options(dir, Options{All: true}))
	if err != nil {
		t.Fatal(err)
	}
	r := rowOf(t, rep, "bad.yml:j")
	if r.Status != StatusError || !strings.Contains(r.Reason, `invalid filter pattern "src/[z-a]*"`) || rep.ExitCode != 1 {
		t.Errorf("bad filter: %+v exit %d", r, rep.ExitCode)
	}
}

func TestChangedFilesUnknownWhenGitFails(t *testing.T) {
	dir := testRepo(t)
	ctx := context.Background()
	gi, err := ReadGit(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if files, ok := ChangedFiles(ctx, gi, true); !ok || len(files) != 1 || files[0] != "src/a.go" {
		t.Fatalf("healthy repo: %v %v", files, ok)
	}
	// A corrupt index makes the working-tree diff fail. The answer has to be
	// "unknown", not the committed files alone.
	if err := os.WriteFile(filepath.Join(dir, ".git", "index"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if files, ok := ChangedFiles(ctx, gi, true); ok {
		t.Errorf("broken index: got %v, ok=true", files)
	}
}

func TestActPathIsMadeAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "act"), []byte("#!/bin/sh\necho act version 0.0.1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	tool, err := ResolveAct(context.Background(), "./act", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(tool.Path) || tool.Source != "flag" || tool.Warning == "" {
		t.Errorf("tool: %+v", tool)
	}
	if _, err := ResolveAct(context.Background(), ".", t.TempDir(), nil); err == nil {
		t.Error("a directory was accepted as --act-path")
	}
}

func TestBaseFlagSetsThePullRequestTarget(t *testing.T) {
	dir := testRepo(t)
	ctx := context.Background()
	if _, err := git(ctx, dir, "branch", "release", "main"); err != nil {
		t.Fatal(err)
	}
	gi, err := ReadGit(ctx, dir, "")
	if err != nil || gi.BaseBranch != "main" || gi.DefaultBranch != "main" {
		t.Fatalf("default: %+v %v", gi, err)
	}
	// A branch name moves the pull request's target.
	if gi, err = ReadGit(ctx, dir, "release"); err != nil || gi.BaseBranch != "release" || gi.BaseRef != "release" {
		t.Fatalf("--base release: %+v %v", gi, err)
	}
	b, _ := BuildEvent("pull_request", gi)
	if !strings.Contains(string(b), `"ref": "release"`) {
		t.Errorf("pull_request payload should target release: %s", b)
	}
	// A commit expression moves only the diff base.
	if gi, err = ReadGit(ctx, dir, "HEAD~1"); err != nil || gi.BaseBranch != "main" || gi.BaseSHA == "" {
		t.Fatalf("--base HEAD~1: %+v %v", gi, err)
	}

	// With --base release, a workflow for pull requests into release runs.
	wf := "name: Rel\non:\n  pull_request:\n    branches: [release]\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "rel.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAct{respond: func(job string) (actLines, int) {
		var l actLines
		l.job(job, true, "")
		return l, 0
	}}
	rep, err := Run(ctx, fake.options(dir, Options{All: true, Event: "pull_request", Base: "release"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, rep, "rel.yml:j"); r.Status != StatusPassed {
		t.Errorf("rel.yml:j with --base release: %+v", r)
	}
	if r := rowOf(t, rep, "actions.yml:node"); r.Status != StatusSkipped {
		t.Errorf("actions.yml targets main, so it should be skipped: %+v", r)
	}
}
