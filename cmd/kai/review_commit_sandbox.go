package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/tools"
)

// Optional, explicitly provisioned runtime. Never substitute host execution
// when Docker or the image is unavailable. CI operators preload a trusted,
// digest-pinned image with /bin/sh; snippets receive no mounts or host env.
type rcShellSandbox struct{ image string }

func rcConfiguredSandbox() *rcShellSandbox {
	image := strings.TrimSpace(os.Getenv("KAI_REVIEW_SANDBOX_IMAGE"))
	if image == "" {
		return nil
	}
	return &rcShellSandbox{image: image}
}

// Two modes. "script" is a free-form exploration: it runs and its output is
// shown, but it carries no assertions and therefore cannot back a runtime
// verdict. "construct" is the fidelity mode: the model supplies the CODE that
// builds the command, and the harness feeds the generated string verbatim into
// /bin/sh, records the generated command, exit status, stdout, stderr and the
// observed working directory, and evaluates the model's explicit assertions.
// What this enforces is narrow: the model cannot alter the string between its
// own construction and execution. The construction code is still
// model-authored, and nothing here verifies it matches the source under
// review.
func rcShellToolInfo() tools.ToolInfo {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	assertion := map[string]any{"type": "object", "properties": map[string]any{
		"kind":  map[string]any{"type": "string", "enum": []string{"exit", "stdout_contains", "stdout_not_contains", "pwd", "pwd_not"}},
		"value": str("expected value: an exit code, a stdout substring, or an absolute directory"),
	}, "required": []string{"kind", "value"}}
	return tools.ToolInfo{
		Name: "review_shell",
		Description: "Run a synthetic reproduction in a fresh, restricted container (no network, repository, credentials or host mounts; /tmp writable and disposable; five-second deadline; POSIX /bin/sh, not an interactive PTY or Windows shell). " +
			"TWO MODES. (1) Exploration: give \"script\" — it runs and you see the output, but it has no assertions and CANNOT back a supported/refuted verdict on a runtime claim. " +
			"(2) Fidelity — REQUIRED for a runtime verdict: give \"construct\" (Node code that prints, to stdout, the exact command string the code under review would build — reproduce the code's own construction, e.g. process.stdout.write('cd ' + JSON.stringify(p) + ' && pwd')), optional \"setup\" (POSIX sh run first, e.g. mkdir -p a literal directory containing $ or backticks), and \"assertions\" (explicit expected results). The harness executes the GENERATED string verbatim in sh and reports the generated command, exit code, stdout, stderr, the observed working directory, and PASS/FAIL per assertion. Do not retype or escape the command yourself. A verdict may cite an experiment only if ALL its assertions passed; a failed assertion means your expectation was wrong and the experiment supports no verdict.",
		Parameters: map[string]any{
			"script":     str("Exploration only: self-contained POSIX shell script (max 8192 bytes). Not usable as evidence for a runtime verdict."),
			"setup":      str("Fidelity mode: POSIX sh run before construction, e.g. mkdir -p '/tmp/test$dir' (max 4096 bytes)."),
			"construct":  str("Fidelity mode: Node code that prints the exact command string to stdout, built the same way the code under review builds it (max 4096 bytes)."),
			"assertions": map[string]any{"type": "array", "items": assertion, "description": "Fidelity mode: explicit expected results (max 8). kinds: exit (expected exit code of the generated command), stdout_contains, stdout_not_contains, pwd (observed working directory after the command equals this), pwd_not."},
		},
	}
}

// rcAssertion is one explicit expectation the model declares before the run.
type rcAssertion struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// rcAssertionResult is the harness's evaluation of one assertion: the
// expectation, and what was actually observed. A failed assertion is an
// observation in its own right — "expected X, observed Y" — not an error.
type rcAssertionResult struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Passed   bool   `json:"passed"`
	Observed string `json:"observed"`
}

// Outcome values. The distinction is the point: a NOT-RUN experiment produced
// no observation and is not evidence of anything; a COMPLETED experiment is an
// observation regardless of whether its assertions passed. Both are recorded.
const (
	rcOutcomeCompleted = "completed" // it ran; observations and assertion results are valid
	rcOutcomeNotRun    = "not_run"   // it could not run; Error says why; no observation exists
)

// rcExperimentRecord is the COMPLETE record of one experiment attempt: what
// was asked, whether it ran, what was generated, what came out, and what each
// assertion showed. Every attempt is recorded — including ones that could not
// run, and ones whose assertions failed. It is kept whole in the challenge
// result and the bundle; only the console summary is bounded.
type rcExperimentRecord struct {
	Outcome          string              `json:"outcome"` // completed | not_run
	Error            string              `json:"error,omitempty"`
	Mode             string              `json:"mode,omitempty"` // script | construct
	Script           string              `json:"script,omitempty"`
	Setup            string              `json:"setup,omitempty"`
	Construct        string              `json:"construct,omitempty"`
	GeneratedCommand string              `json:"generatedCommand,omitempty"`
	ExitCode         int                 `json:"exitCode"`
	ObservedPWD      string              `json:"observedPwd,omitempty"`
	Stdout           string              `json:"stdout"`
	Stderr           string              `json:"stderr"`
	Assertions       []rcAssertionResult `json:"assertions,omitempty"`
	HasAssertions    bool                `json:"hasAssertions"`
	AllPassed        bool                `json:"allPassed"`
}

// completed reports whether the experiment actually ran and its observations
// are valid — irrespective of assertion results.
func (r *rcExperimentRecord) completed() bool { return r != nil && r.Outcome == rcOutcomeCompleted }

// qualifies is the CURRENT gate rule for citing an experiment in support of a
// runtime verdict: it ran, declared at least one assertion, and every assertion
// passed. This rule is known to be too strong — a completed experiment whose
// assertion failed can itself demonstrate an alleged behavior — and its
// revision is paused pending review of the evidence contract. The record
// preserves the observation either way.
func (r *rcExperimentRecord) qualifies() bool {
	return r.completed() && r.HasAssertions && r.AllPassed
}

// disqualifyReason says, precisely, why a cited experiment does not meet the
// current rule — naming the failed expectation and its observed value.
func (r *rcExperimentRecord) disqualifyReason() string {
	if r == nil {
		return "no experiment"
	}
	if !r.completed() {
		return "the cited experiment did not run: " + r.Error
	}
	if !r.HasAssertions {
		return "the cited experiment declared no assertions (an unasserted printout cannot establish behavior)"
	}
	for _, a := range r.Assertions {
		if !a.Passed {
			return fmt.Sprintf("the cited experiment's assertion failed: %s %q, observed %q", a.Kind, a.Value, a.Observed)
		}
	}
	return "the cited experiment did not qualify"
}

// render is the SOURCE text the model sees and cites. It is the full record
// in a fixed layout, so a cited line range points at real observed output.
func (r *rcExperimentRecord) render(image string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Runtime: %s /bin/sh (non-interactive, isolated)\nOutcome: %s\n", image, r.Outcome)
	if !r.completed() {
		fmt.Fprintf(&b, "The experiment could not run: %s\nNo observation was produced; this is not evidence of any behavior.\n", r.Error)
		return b.String()
	}
	fmt.Fprintf(&b, "Mode: %s\n", r.Mode)
	if r.Mode == "script" {
		fmt.Fprintf(&b, "Script:\n%s\n", r.Script)
	} else {
		if r.Setup != "" {
			fmt.Fprintf(&b, "Setup:\n%s\n", r.Setup)
		}
		fmt.Fprintf(&b, "Construct (Node, prints the command):\n%s\nGenerated command (executed verbatim by sh):\n%s\n", r.Construct, r.GeneratedCommand)
	}
	fmt.Fprintf(&b, "Exit code: %d\n", r.ExitCode)
	if r.Mode == "construct" {
		fmt.Fprintf(&b, "Observed working directory after the command: %s\n", r.ObservedPWD)
	}
	fmt.Fprintf(&b, "stdout:\n%s\nstderr:\n%s\n", r.Stdout, r.Stderr)
	if r.HasAssertions {
		b.WriteString("Assertions (each is an observation — expected vs observed):\n")
		for _, a := range r.Assertions {
			mark := "PASS"
			if !a.Passed {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "  %s %s %q (observed %q)\n", mark, a.Kind, a.Value, a.Observed)
		}
		fmt.Fprintf(&b, "All assertions passed: %v\n", r.AllPassed)
	} else {
		b.WriteString("Assertions: none declared — this experiment cannot back a runtime verdict\n")
	}
	return b.String()
}

// summary is the bounded console line set.
func (r *rcExperimentRecord) summary() string {
	if !r.completed() {
		return fmt.Sprintf("outcome=%s error=%q", r.Outcome, rcTruncate(r.Error, 200))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "outcome=%s mode=%s exit=%d", r.Outcome, r.Mode, r.ExitCode)
	if r.Mode == "construct" {
		fmt.Fprintf(&b, " generated=%q pwd=%q", rcTruncate(r.GeneratedCommand, 160), r.ObservedPWD)
	}
	if r.HasAssertions {
		fmt.Fprintf(&b, " assertions_all_passed=%v", r.AllPassed)
		for _, a := range r.Assertions {
			mark := "PASS"
			if !a.Passed {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "\n      %s %s %q (observed %q)", mark, a.Kind, a.Value, rcTruncate(a.Observed, 120))
		}
	} else {
		b.WriteString(" assertions=none")
	}
	return b.String()
}

func rcTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var rcSandboxImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func rcSandboxArgs(image, name string) ([]string, error) {
	if !rcSandboxImagePattern.MatchString(image) {
		return nil, fmt.Errorf("KAI_REVIEW_SANDBOX_IMAGE must name a trusted, preloaded image pinned by sha256 digest")
	}
	return []string{"run", "--rm", "--pull=never", "--name", name,
		"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--user=65534:65534", "--pids-limit=32", "--memory=64m", "--memory-swap=64m", "--cpus=0.5",
		"--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=16m", "--workdir=/tmp",
		"--entrypoint=/bin/sh", "-i", image, "-s"}, nil
}

// Always drain the pipe while retaining a fixed amount, so an experiment cannot
// make the reviewer allocate unbounded output or block Docker on a full pipe.
type rcBoundedOutput struct {
	bytes.Buffer
	truncated bool
}

func (w *rcBoundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := 16*1024 - w.Len()
	if n > left {
		w.truncated = true
		p = p[:left]
	}
	w.Buffer.Write(p)
	return n, nil
}

type rcExperimentParams struct {
	Script     string        `json:"script"`
	Setup      string        `json:"setup"`
	Construct  string        `json:"construct"`
	Assertions []rcAssertion `json:"assertions"`
}

const (
	rcGenBegin   = "__KAI_GEN_BEGIN__"
	rcGenEnd     = "__KAI_GEN_END__"
	rcRunBegin   = "__KAI_RUN_BEGIN__"
	rcExitMarker = "__KAI_EXIT__="
	rcPWDMarker  = "__KAI_PWD__="
)

// rcConstructDriver is the sh program that realizes fidelity mode inside the
// container. The generated command is written to a file and executed as its
// own line by a child sh, followed by probes that record its exit status and
// the working directory it left behind. Nothing between construction and
// execution touches the string.
func rcConstructDriver(p rcExperimentParams) string {
	var b strings.Builder
	b.WriteString("set +e\n")
	if strings.TrimSpace(p.Setup) != "" {
		b.WriteString(p.Setup)
		b.WriteString("\n__kai_setup_rc=$?\nif [ \"$__kai_setup_rc\" -ne 0 ]; then printf '__KAI_SETUP_FAILED__=%s\\n' \"$__kai_setup_rc\"; exit 96; fi\n")
	}
	b.WriteString("cat > /tmp/__kai_construct.js <<'__KAI_CONSTRUCT_EOF__'\n")
	b.WriteString(p.Construct)
	b.WriteString("\n__KAI_CONSTRUCT_EOF__\n")
	b.WriteString("node /tmp/__kai_construct.js > /tmp/__kai_generated.cmd 2> /tmp/__kai_construct.err || { printf '__KAI_CONSTRUCT_FAILED__\\n'; cat /tmp/__kai_construct.err; exit 97; }\n")
	fmt.Fprintf(&b, "printf '%s\\n'; cat /tmp/__kai_generated.cmd; printf '\\n%s\\n'\n", rcGenBegin, rcGenEnd)
	b.WriteString("{ cat /tmp/__kai_generated.cmd; printf '\\n__KAI_RC__=$?\\nprintf \"" + rcExitMarker + "%%s\\\\n" + rcPWDMarker + "%%s\\\\n\" \"$__KAI_RC__\" \"$PWD\"\\n'; } > /tmp/__kai_run.sh\n")
	fmt.Fprintf(&b, "printf '%s\\n'\n", rcRunBegin)
	b.WriteString("sh /tmp/__kai_run.sh\n")
	return b.String()
}

// runExperiment executes one experiment and returns its record. Every attempt
// yields a record: on a harness-level failure (bad parameters, Docker
// unavailable, timeout, truncated output, a construct that produced no
// command) the record's Outcome is not_run with the reason, AND an error is
// returned so the caller can tell the model no observation exists. A record
// with Outcome completed is a valid observation whether or not its assertions
// passed.
func (s *rcShellSandbox) runExperiment(ctx context.Context, input string) (*rcExperimentRecord, error) {
	rec := &rcExperimentRecord{Outcome: rcOutcomeNotRun}
	notRun := func(format string, args ...any) (*rcExperimentRecord, error) {
		err := fmt.Errorf(format, args...)
		rec.Error = err.Error()
		return rec, err
	}
	var p rcExperimentParams
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		return notRun("experiment parameters are not valid JSON")
	}
	var program string
	switch {
	case strings.TrimSpace(p.Construct) != "":
		rec.Mode, rec.Setup, rec.Construct = "construct", p.Setup, p.Construct
		if len(p.Construct) > 4096 || len(p.Setup) > 4096 || len(p.Assertions) > 8 {
			return notRun("fidelity experiment limits: construct and setup ≤ 4096 bytes, at most 8 assertions")
		}
		for _, a := range p.Assertions {
			switch a.Kind {
			case "exit", "stdout_contains", "stdout_not_contains", "pwd", "pwd_not":
			default:
				return notRun("unknown assertion kind %q", a.Kind)
			}
		}
		program = rcConstructDriver(p)
	case len(p.Script) > 0:
		rec.Mode, rec.Script = "script", p.Script
		if len(p.Script) > 8192 {
			return notRun("experiment needs a script of 1–8192 bytes")
		}
		if len(p.Assertions) > 0 {
			return notRun("assertions require fidelity mode (construct); a free-form script cannot be asserted")
		}
		program = p.Script
	default:
		return notRun("experiment needs either a script or a construct")
	}

	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return notRun("%v", err)
	}
	name := "kai-review-" + hex.EncodeToString(id[:])
	args, err := rcSandboxArgs(s.image, name)
	if err != nil {
		return notRun("%v", err)
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return notRun("Docker is unavailable; host execution is disabled")
	}
	// Killing the Docker client does not necessarily stop its container.
	// Remove by our random name on every exit, including timeout/cancellation.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanup, docker, "rm", "-f", name)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
	}()
	runctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runctx, docker, args...)
	cmd.Stdin = strings.NewReader(program)
	cmd.WaitDelay = time.Second
	var stdout, stderr rcBoundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runctx.Err() != nil {
		return notRun("experiment did not finish: %v", runctx.Err())
	}
	containerExit := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			return notRun("experiment could not execute: %v", err)
		}
		containerExit = exit.ExitCode()
		if containerExit < 0 || containerExit >= 125 {
			return notRun("container/runtime unavailable (exit %d): %s", containerExit, stderr.String())
		}
	}
	if stdout.truncated || stderr.truncated {
		return notRun("experiment exceeded the output limit; incomplete output is not evidence")
	}
	rec.Stderr = stderr.String()
	if rec.Mode == "script" {
		rec.Outcome, rec.ExitCode, rec.Stdout = rcOutcomeCompleted, containerExit, stdout.String()
		return rec, nil
	}
	if containerExit == 96 {
		return notRun("fidelity experiment: setup failed: %s", strings.TrimSpace(stdout.String()+stderr.String()))
	}
	if containerExit == 97 {
		return notRun("fidelity experiment: construct did not produce a command (is node available in the sandbox image?): %s", strings.TrimSpace(stdout.String()))
	}
	if err := rec.parseConstructOutput(stdout.String()); err != nil {
		return notRun("%v", err)
	}
	rec.Outcome = rcOutcomeCompleted
	rec.evaluate(p.Assertions)
	return rec, nil
}

// parseConstructOutput splits the driver's stdout into the generated command,
// the program's own output, and the exit/pwd probes.
func (r *rcExperimentRecord) parseConstructOutput(out string) error {
	gb, ge := strings.Index(out, rcGenBegin+"\n"), strings.Index(out, "\n"+rcGenEnd+"\n")
	if gb < 0 || ge < gb {
		return fmt.Errorf("fidelity experiment record incomplete: generated command not captured")
	}
	r.GeneratedCommand = strings.TrimSuffix(out[gb+len(rcGenBegin)+1:ge], "\n")
	rb := strings.Index(out, rcRunBegin+"\n")
	if rb < 0 {
		return fmt.Errorf("fidelity experiment record incomplete: execution not captured")
	}
	run := out[rb+len(rcRunBegin)+1:]
	pi := strings.LastIndex(run, rcPWDMarker)
	ei := strings.LastIndex(run, rcExitMarker)
	if pi < 0 || ei < 0 || ei > pi {
		return fmt.Errorf("fidelity experiment record incomplete: exit status or working directory not captured")
	}
	r.ObservedPWD = strings.TrimRight(run[pi+len(rcPWDMarker):], "\n")
	code, err := strconv.Atoi(strings.TrimSpace(run[ei+len(rcExitMarker) : pi]))
	if err != nil {
		return fmt.Errorf("fidelity experiment record incomplete: exit status unreadable")
	}
	r.ExitCode = code
	r.Stdout = run[:ei]
	return nil
}

// evaluate applies the declared assertions to the recorded outcome. Each
// result records the expectation and the observed value; a failure is an
// observation, not an error.
func (r *rcExperimentRecord) evaluate(assertions []rcAssertion) {
	r.HasAssertions = len(assertions) > 0
	r.AllPassed = r.HasAssertions
	for _, a := range assertions {
		res := rcAssertionResult{Kind: a.Kind, Value: a.Value}
		switch a.Kind {
		case "exit":
			res.Observed = strconv.Itoa(r.ExitCode)
			want, err := strconv.Atoi(strings.TrimSpace(a.Value))
			res.Passed = err == nil && want == r.ExitCode
		case "stdout_contains":
			res.Observed = rcTruncate(r.Stdout, 400)
			res.Passed = strings.Contains(r.Stdout, a.Value)
		case "stdout_not_contains":
			res.Observed = rcTruncate(r.Stdout, 400)
			res.Passed = !strings.Contains(r.Stdout, a.Value)
		case "pwd":
			res.Observed = r.ObservedPWD
			res.Passed = r.ObservedPWD == a.Value
		case "pwd_not":
			res.Observed = r.ObservedPWD
			res.Passed = r.ObservedPWD != a.Value
		}
		if !res.Passed {
			r.AllPassed = false
		}
		r.Assertions = append(r.Assertions, res)
	}
}

// run keeps the original text-returning entry point for callers and tests that
// only need the rendered source of a completed experiment.
func (s *rcShellSandbox) run(ctx context.Context, input string) (string, error) {
	rec, err := s.runExperiment(ctx, input)
	if err != nil {
		return "", err
	}
	return rec.render(s.image), nil
}
