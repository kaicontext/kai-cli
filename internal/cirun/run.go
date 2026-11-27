package cirun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options configures one run.
type Options struct {
	Dir     string   // any directory inside the repo
	Targets []string // "workflow.yml:job" or "job"
	All     bool     // every job the event triggers
	Event   string   // "push" (default) or "pull_request"
	Tree    string   // "working" (default) or "head"
	Base    string   // base ref override
	Image   string   // "medium" (default), "full", or an image reference
	Arch    string   // container architecture for act, e.g. "linux/amd64"
	Matrix  []string // "key=value" pairs that narrow a matrix job

	Strict       bool
	DockerSocket bool // mount the host's Docker socket into job containers
	Verbose      bool // show step output live
	ActPath      string
	KaiHome      string        // default ~/.kai
	Timeout      time.Duration // per act invocation; default 60m
	Progress     io.Writer     // live human output; nil for none

	// Seams for tests.
	resolveAct    func(ctx context.Context, actPath, kaiHome string, notify func(string)) (*ActTool, error)
	detectRuntime func() (*ContainerRuntime, error)
	execAct       func(ctx context.Context, inv actInvocation, onLine func(string)) (int, error)
}

// ErrNoSelection is returned when neither a target nor --all was given.
// Picking jobs from `kai ci plan` is the next phase.
var ErrNoSelection = errors.New("name a job or pass --all")

type actInvocation struct {
	bin  string
	args []string
	dir  string
	env  []string
}

type pick struct {
	job *Job
	row *JobResult
	fid Fidelity
	run bool
}

// Run picks jobs, checks what can run here, runs the rest through act, and
// returns the report. The error is for failures to run at all; job failures
// are in the report.
func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Event == "" {
		o.Event = "push"
	}
	if o.Tree == "" {
		o.Tree = "working"
	}
	if o.Tree != "working" && o.Tree != "head" {
		return nil, fmt.Errorf("--tree %s: use working or head", o.Tree)
	}
	if o.Event != "push" && o.Event != "pull_request" {
		return nil, fmt.Errorf("--event %s: use push or pull_request", o.Event)
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Minute
	}
	if o.KaiHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		o.KaiHome = filepath.Join(home, ".kai")
	}
	if o.resolveAct == nil {
		o.resolveAct = ResolveAct
	}
	if o.detectRuntime == nil {
		o.detectRuntime = DetectRuntime
	}
	if o.execAct == nil {
		o.execAct = execAct
	}
	progress := func(format string, a ...any) {
		if o.Progress != nil {
			fmt.Fprintf(o.Progress, format+"\n", a...)
		}
	}
	matrix, err := matrixArgs(o.Matrix)
	if err != nil {
		return nil, err
	}

	gi, err := ReadGit(ctx, o.Dir, o.Base)
	if err != nil {
		return nil, err
	}
	runDir := gi.Root
	if o.Tree == "head" {
		dir, cleanup, err := headCheckout(ctx, gi)
		if err != nil {
			return nil, fmt.Errorf("checking out HEAD: %w", err)
		}
		defer cleanup()
		runDir = dir
	}
	wfs, err := LoadWorkflows(runDir)
	if err != nil {
		return nil, err
	}

	images := ImageMap(o.Image)
	rep := &Report{
		SHA: gi.HeadSHA, Branch: gi.Branch, Tree: o.Tree, Event: o.Event,
		Dirty: o.Tree == "working" && gi.Dirty, Base: gi.BaseRef, Strict: o.Strict,
	}
	copts := ClassifyOptions{Images: images}

	// Pick.
	var picks []*pick
	byRef := map[string]*pick{}
	add := func(j *Job, why string) *pick {
		if p := byRef[j.Ref()]; p != nil {
			return p
		}
		p := &pick{job: j, row: &JobResult{ID: j.Ref(), Workflow: j.Workflow.File, Job: j.ID, Why: why}}
		byRef[j.Ref()] = p
		picks = append(picks, p)
		rep.Jobs = append(rep.Jobs, p.row)
		return p
	}
	switch {
	case len(o.Targets) > 0:
		for _, t := range o.Targets {
			j, err := FindJob(wfs, t)
			if err != nil {
				return nil, err
			}
			add(j, "asked for").run = true
		}
	case o.All:
		if len(wfs) == 0 {
			return nil, fmt.Errorf("no workflows found under %s", strings.Join(WorkflowDirs, " or "))
		}
		changed, known := ChangedFiles(ctx, gi, o.Tree == "working")
		ev := EventContext{Event: o.Event, Branch: gi.Branch, BaseBranch: gi.DefaultBranch, Changed: changed, ChangedKnown: known}
		for _, wf := range wfs {
			ok, why := Triggered(wf, ev)
			for _, j := range wf.Jobs {
				p := add(j, "triggered by "+o.Event)
				if !ok {
					p.row.Status, p.row.Reason, p.row.Why = StatusSkipped, why, ""
					continue
				}
				p.run = true
			}
		}
	default:
		return nil, ErrNoSelection
	}

	// Fidelity, before anything runs.
	for _, p := range append([]*pick(nil), picks...) {
		if !p.run {
			continue
		}
		p.fid = Resolve(p.job, copts)
		p.row.Caveats, p.row.Partial = p.fid.Caveats, p.fid.Partial
		if p.fid.Bucket == BucketCannot {
			p.run = false
			p.row.Status, p.row.Reason = StatusNotRun, p.fid.Reason
			continue
		}
		// act runs what the job needs; give those jobs a row too.
		for _, dep := range NeedsClosure(p.job) {
			if byRef[dep.Ref()] == nil {
				dp := add(dep, "needed by "+p.job.Ref())
				dp.run = true
				dp.fid = Classify(dep, copts)
				dp.row.Caveats, dp.row.Partial = dp.fid.Caveats, dp.fid.Partial
			}
		}
	}
	// A job that is needed by another picked job runs inside that job's act
	// invocation; only the rest get an invocation of their own.
	needed := map[string]bool{}
	for _, p := range picks {
		if p.run {
			for _, dep := range NeedsClosure(p.job) {
				needed[dep.Ref()] = true
			}
		}
	}
	var roots []*pick
	for _, p := range picks {
		if p.run && !needed[p.job.Ref()] {
			roots = append(roots, p)
		}
	}
	if len(roots) == 0 {
		rep.ExitCode = rep.exitCode()
		return rep, nil
	}
	// Say what won't run before the run starts, not only in the summary.
	for _, p := range picks {
		switch {
		case p.row.Status == StatusNotRun:
			progress("not run  %s: %s", p.row.ID, p.row.Reason)
		case p.row.Status == StatusSkipped:
			progress("skipped  %s: %s", p.row.ID, p.row.Reason)
		}
	}

	// Runner.
	rt, err := o.detectRuntime()
	if err != nil {
		return nil, err
	}
	act, err := o.resolveAct(ctx, o.ActPath, o.KaiHome, func(s string) { progress("%s", s) })
	if err != nil {
		return nil, err
	}
	if act.Warning != "" {
		progress("warning: %s", act.Warning)
	}
	rep.ActVersion, rep.Runtime, rep.Images = act.Version, rt.Name, images
	progress("Images: %s", imageSummary(images))

	scratch, err := os.MkdirTemp("", "kai-ci-run-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	payload, err := BuildEvent(o.Event, gi)
	if err != nil {
		return nil, err
	}
	eventFile := filepath.Join(scratch, "event.json")
	// Stands in for the .secrets, .env, .vars and .input files act would
	// otherwise read from the repo and hand to every action in the job.
	emptyFile := filepath.Join(scratch, "empty")
	if err := os.WriteFile(eventFile, payload, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		return nil, err
	}
	env := os.Environ()
	if rt.Host != "" {
		env = append(env, "DOCKER_HOST="+rt.Host)
	}

	for _, root := range roots {
		args := actArgs(o, root.job.ID, root.job.Workflow.Path, runDir, eventFile, emptyFile, gi.DefaultBranch, images, matrix)
		progress("\n▸ %s", root.row.ID)

		parser := newStreamParser()
		if o.Progress != nil {
			parser.onStep = func(jobID, stage, step, result string) {
				mark := map[string]string{"success": "✓", "failure": "✗", "skipped": "-"}[result]
				if mark == "" || (stage != "Main" && result != "failure") {
					return
				}
				progress("  %s %s: %s", mark, jobID, step)
			}
			if o.Verbose {
				parser.onRaw = func(jobID, line string) { progress("    %s | %s", jobID, line) }
			}
		}
		runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
		start := time.Now()
		code, execErr := o.execAct(runCtx, actInvocation{bin: act.Path, args: args, dir: runDir, env: env}, parser.feed)
		timedOut := runCtx.Err() == context.DeadlineExceeded
		cancel()
		wall := time.Since(start)

		// Jobs that reported a result first, so that a job with none can be
		// explained by a failed job it needs.
		jobs := append([]*Job{root.job}, NeedsClosure(root.job)...)
		var silent []*Job
		for _, j := range jobs {
			row := byRef[j.Ref()].row
			if row.Status == StatusFailed {
				continue // already failed under another root
			}
			js := parser.jobs[j.ID]
			row.FailedStep, row.LogExcerpt, row.Reason = "", "", ""
			if js != nil && !js.first.IsZero() {
				row.DurationMS = js.last.Sub(js.first).Milliseconds()
			} else if j == root.job {
				row.DurationMS = wall.Milliseconds()
			}
			switch {
			case js != nil && js.result == "failure":
				row.Status, row.FailedStep = StatusFailed, js.failedStep
				row.LogExcerpt = strings.Join(js.failedLog, "\n")
			case js != nil && js.result == "success":
				row.Status = StatusPassed
			default:
				silent = append(silent, j)
			}
		}
		for _, j := range silent {
			row, js := byRef[j.Ref()].row, parser.jobs[j.ID]
			if dep := failedDep(j, byRef); dep != "" {
				row.Status, row.Reason = StatusNotRun, fmt.Sprintf("needs %s, which failed", dep)
				continue
			}
			if code == 0 && execErr == nil {
				row.Status, row.Reason = StatusSkipped, "act did not run it; its if: condition is false for this event"
				continue
			}
			row.Status = StatusError
			switch {
			case timedOut:
				row.Reason = fmt.Sprintf("timed out after %s", o.Timeout)
			case execErr != nil:
				row.Reason = execErr.Error()
			default:
				row.Reason = fmt.Sprintf("act exited %d before the job had a result", code)
			}
			tail := parser.other
			if js != nil && len(js.steps) > 0 {
				tail = append(append([]string(nil), js.steps[len(js.steps)-1].log...), tail...)
			}
			row.LogExcerpt = strings.Join(tail, "\n")
		}
		if ctx.Err() != nil {
			break
		}
	}
	rep.ExitCode = rep.exitCode()
	return rep, ctx.Err()
}

// failedDep names a job in j's needs closure that failed, or "".
func failedDep(j *Job, byRef map[string]*pick) string {
	for _, dep := range NeedsClosure(j) {
		if p := byRef[dep.Ref()]; p != nil && p.row.Status == StatusFailed {
			return dep.ID
		}
	}
	return ""
}

// actArgs builds the act command line for one job.
func actArgs(o Options, jobID, wfPath, runDir, eventFile, emptyFile, defaultBranch string, images map[string]string, matrix []string) []string {
	args := []string{
		o.Event,
		"--json",
		"-C", runDir,
		"-W", wfPath,
		"-j", jobID,
		"-e", eventFile,
		"--defaultbranch", defaultBranch,
		// Keep act from reading the repo's own dotfiles into the job.
		"--secret-file", emptyFile,
		"--var-file", emptyFile,
		"--env-file", emptyFile,
		"--input-file", emptyFile,
		"--action-cache-path", filepath.Join(o.KaiHome, "cache", "actions"),
	}
	labels := make([]string, 0, len(images))
	for l := range images {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		args = append(args, "-P", l+"="+images[l])
	}
	if !o.DockerSocket {
		// On GitHub an action runs on a throwaway VM. Here it runs on this
		// machine, and the Docker socket is root on it.
		args = append(args, "--container-daemon-socket", "-")
	}
	if o.Arch != "" {
		args = append(args, "--container-architecture", o.Arch)
	}
	for _, m := range matrix {
		args = append(args, "--matrix", m)
	}
	return args
}

// matrixArgs turns "os=ubuntu-latest,go=1.25" style values into act's
// "key:value" form.
func matrixArgs(in []string) ([]string, error) {
	var out []string
	for _, group := range in {
		for _, kv := range strings.Split(group, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || k == "" || v == "" {
				return nil, fmt.Errorf("--matrix %q: use key=value", kv)
			}
			out = append(out, k+":"+v)
		}
	}
	return out, nil
}

func imageSummary(images map[string]string) string {
	byImage := map[string][]string{}
	for l, img := range images {
		byImage[img] = append(byImage[img], l)
	}
	var parts []string
	for img, labels := range byImage {
		sort.Strings(labels)
		parts = append(parts, strings.Join(labels, ", ")+" → "+img)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// execAct runs act and hands every line it prints, on either stream, to
// onLine. The exit code is act's; err is for failing to run it at all.
func execAct(ctx context.Context, inv actInvocation, onLine func(string)) (int, error) {
	c := exec.CommandContext(ctx, inv.bin, inv.args...)
	c.Dir, c.Env = inv.dir, inv.env
	c.WaitDelay = childWaitDelay
	pr, pw := io.Pipe()
	c.Stdout, c.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReaderSize(pr, 64<<10)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				onLine(line)
			}
			if err != nil {
				return
			}
		}
	}()
	err := c.Run()
	pw.Close()
	<-done
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
