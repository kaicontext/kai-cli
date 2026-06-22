package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kai/internal/agent"
	"kai/internal/agent/message"
)

// testConvention captures everything the test-pass agent needs about
// a project's test layout: where test files live (file-name pattern)
// and how to run them. Detected once per agent run from the workspace
// root; passed to the test agent so it doesn't have to rediscover.
//
// Empty Convention.Run means "no test convention detected" — the test
// pass should be skipped entirely.
type testConvention struct {
	// Lang is a short label for the prompt: "go", "javascript",
	// "python", "rust". Drives nothing structural; just helps the
	// agent's mental model so it picks the right idioms.
	Lang string
	// FilePattern describes how test files are named (e.g.
	// "*_test.go", "*.test.ts", "test_*.py"). Used by the prompt
	// to anchor the agent's output.
	FilePattern string
	// Run is the shell command that executes the test suite from
	// the convention's Dir (workspace root by default). Empty when
	// no convention was detected.
	Run string
	// Dir is the path relative to the workspace root where the
	// manifest was discovered. Empty when the manifest is at the
	// workspace root. Non-empty for monorepos / nested modules
	// (e.g. "kai-cli" when go.mod lives at kai-cli/go.mod). The
	// orchestrator joins this with the workspace before exec.
	Dir string
}

// detectTestConvention inspects the workspace root for the strongest
// signal of which test framework is in use. Order matters — we
// prefer language-specific manifests (go.mod, Cargo.toml) over
// Makefile targets because the manifests give us a known-good run
// command without parsing.
//
// Returns an empty Convention with ok=false when nothing matches,
// which is the signal to skip the test pass.
func detectTestConvention(workspace string) (testConvention, bool) {
	// locate walks the workspace for a manifest file and returns the
	// directory containing it, relative to workspace. Empty string
	// when not found. Root-level manifests return "" (Dir=""), which
	// preserves the prior root-only behavior for repos without
	// nested modules.
	locate := func(name string) (relDir string, ok bool) {
		absDir := findManifest(name)(workspace)
		if absDir == "" {
			return "", false
		}
		rel, err := filepath.Rel(workspace, absDir)
		if err != nil || rel == "." {
			return "", true
		}
		return rel, true
	}

	if dir, ok := locate("go.mod"); ok {
		return testConvention{Lang: "go", FilePattern: "*_test.go", Run: "go test ./...", Dir: dir}, true
	}
	if dir, ok := locate("Cargo.toml"); ok {
		return testConvention{Lang: "rust", FilePattern: "tests/*.rs and inline #[test] blocks", Run: "cargo test", Dir: dir}, true
	}
	for _, name := range []string{"pyproject.toml", "setup.py", "pytest.ini", "tox.ini"} {
		if dir, ok := locate(name); ok {
			return testConvention{Lang: "python", FilePattern: "test_*.py and tests/", Run: "pytest", Dir: dir}, true
		}
	}
	if dir, ok := locate("package.json"); ok {
		// JS/TS: pull the run command from package.json scripts if
		// "test" exists, falling back to a generic guess. We don't
		// over-detect (jest vs vitest vs mocha) — the package.json
		// "test" script is the canonical entry point regardless.
		run := scriptFromPackageJSON(filepath.Join(workspace, dir, "package.json"))
		if run == "" {
			// No "test" script — leave detection negative so the
			// pass skips rather than running a guess.
			return testConvention{}, false
		}
		return testConvention{Lang: "javascript", FilePattern: "*.test.ts / *.spec.ts / __tests__/", Run: run, Dir: dir}, true
	}
	if dir, ok := locate("Makefile"); ok {
		if makefileHasTarget(filepath.Join(workspace, dir, "Makefile"), "test") {
			return testConvention{Lang: "make", FilePattern: "(see Makefile)", Run: "make test", Dir: dir}, true
		}
	}
	return testConvention{}, false
}

// scriptFromPackageJSON pulls the "test" entry from package.json's
// scripts block. Returns "" if missing, malformed, or empty. Doesn't
// validate the script — that's the agent's job at run time.
//
// We grep rather than json.Unmarshal because the file may have
// trailing commas / comments (some tooling allows it) and we'd
// rather a permissive scan than a strict reject.
func scriptFromPackageJSON(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(body)
	// Find the "test" entry inside "scripts": { ... }. Permissive
	// regex avoided in favor of a small state walk — keeps output
	// deterministic and easy to debug.
	scriptsIdx := strings.Index(s, `"scripts"`)
	if scriptsIdx < 0 {
		return ""
	}
	rest := s[scriptsIdx:]
	testIdx := strings.Index(rest, `"test"`)
	if testIdx < 0 {
		return ""
	}
	rest = rest[testIdx+len(`"test"`):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = rest[colon+1:]
	q := strings.Index(rest, `"`)
	if q < 0 {
		return ""
	}
	rest = rest[q+1:]
	endQ := strings.Index(rest, `"`)
	if endQ < 0 {
		return ""
	}
	cmd := strings.TrimSpace(rest[:endQ])
	if cmd == "" {
		return ""
	}
	// Most tooling assumes the script runs through the project's
	// package manager. We default to bun → npm → yarn detection
	// based on lockfile presence. If none, fall back to running
	// the command directly (which works when the script is a bare
	// command like "vitest run").
	dir := filepath.Dir(path)
	switch {
	case fileExists(filepath.Join(dir, "bun.lockb")) || fileExists(filepath.Join(dir, "bun.lock")):
		return "bun run test"
	case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm test"
	case fileExists(filepath.Join(dir, "yarn.lock")):
		return "yarn test"
	case fileExists(filepath.Join(dir, "package-lock.json")):
		return "npm test"
	}
	return cmd
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// makefileHasTarget checks whether path contains a target named
// `target:` at the start of a line. Permissive — we don't parse the
// Makefile, just look for the pattern. False negatives possible if
// the user uses unusual prefixes (.PHONY: test alone won't match —
// they need the actual "test:" rule), which is the right bias.
func makefileHasTarget(path, target string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.Split(string(body), "\n")
	prefix := target + ":"
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// shouldRunTests decides whether the test pass should fire after
// verify completes. Skip when:
//
//   - the agent didn't apply edits
//   - all changed files are themselves test files (the agent was
//     editing tests; running a test pass would loop)
//   - all changed files are docs/config-only (.md, .yaml, .toml,
//     .json, .gitignore, etc.) — no logic to test
//   - no test convention detected (nothing to run)
//
// All four checks are AND'd: any one failure skips. The convention
// arg is detected upstream in runOneAgent so we don't pay the disk
// scan twice.
func shouldRunTests(changedPaths []string, conv testConvention) bool {
	if conv.Run == "" {
		return false
	}
	if len(changedPaths) == 0 {
		return false
	}
	allTestFiles := true
	allConfigFiles := true
	for _, p := range changedPaths {
		if !isTestPath(p) {
			allTestFiles = false
		}
		if !isConfigOrDocPath(p) {
			allConfigFiles = false
		}
	}
	if allTestFiles {
		return false
	}
	if allConfigFiles {
		return false
	}
	return true
}

// isTestPath returns true for files that follow common test-file
// naming conventions. Conservative — we'd rather miss a test file
// (and run the test pass when we shouldn't) than treat a real source
// file as a test (and skip when we should run). The cost imbalance
// favors false negatives.
func isTestPath(p string) bool {
	base := filepath.Base(p)
	low := strings.ToLower(base)
	switch {
	case strings.HasSuffix(low, "_test.go"):
		return true
	case strings.HasSuffix(low, ".test.ts"), strings.HasSuffix(low, ".test.tsx"),
		strings.HasSuffix(low, ".test.js"), strings.HasSuffix(low, ".test.jsx"),
		strings.HasSuffix(low, ".test.mjs"):
		return true
	case strings.HasSuffix(low, ".spec.ts"), strings.HasSuffix(low, ".spec.tsx"),
		strings.HasSuffix(low, ".spec.js"), strings.HasSuffix(low, ".spec.jsx"):
		return true
	case strings.HasPrefix(low, "test_") && strings.HasSuffix(low, ".py"):
		return true
	case strings.HasSuffix(low, "_test.py"):
		return true
	}
	// Path-based: anything under __tests__/ or tests/ counts.
	parts := strings.Split(filepath.ToSlash(p), "/")
	for _, part := range parts {
		switch part {
		case "__tests__", "tests", "test", "spec":
			return true
		}
	}
	return false
}

// isConfigOrDocPath returns true for files that don't carry runnable
// logic worth testing. Used to skip the test pass when an agent only
// touched config/doc/data files (a README rewrite, a gitignore tweak,
// a yaml schema update). Conservative — keeps the list short to
// avoid swallowing real source files.
func isConfigOrDocPath(p string) bool {
	low := strings.ToLower(filepath.Base(p))
	switch low {
	case ".gitignore", "license", "license.md", "license.txt",
		"readme.md", "readme", "readme.txt",
		"contributing.md", "code_of_conduct.md", "changelog.md":
		return true
	}
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".md", ".markdown", ".rst", ".txt",
		".yaml", ".yml", ".toml", ".json", ".jsonc",
		".env", ".lock", ".sum":
		return true
	}
	return false
}

// buildTestPrompt composes the system + user instructions for the
// test-pass agent. The prompt is deliberately narrow: do ONE thing
// (cover the changed code with tests), check ONE outcome (do they
// pass), output ONE line per test added.
func buildTestPrompt(changedFiles []string, conv testConvention, originalRequest string) string {
	var b strings.Builder
	b.WriteString("System: You are running a test-coverage pass after another agent applied edits.\n\n")
	b.WriteString("Project test convention:\n")
	fmt.Fprintf(&b, "  Language: %s\n", conv.Lang)
	fmt.Fprintf(&b, "  Test files: %s\n", conv.FilePattern)
	fmt.Fprintf(&b, "  Run command: %s\n", conv.Run)
	if conv.Dir != "" {
		fmt.Fprintf(&b, "  Run from: %s (the manifest lives in a subdirectory; cd there before running build/test commands)\n", conv.Dir)
	}
	b.WriteByte('\n')
	b.WriteString("Files the previous agent changed (only non-test, non-doc files listed):\n")
	for _, f := range changedFiles {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	b.WriteString("\nOriginal user request that triggered this work:\n  ")
	b.WriteString(strings.TrimSpace(originalRequest))
	b.WriteString("\n\nYour job:\n")
	b.WriteString("  1. For each changed function, find existing tests that cover it. Update them to match the new behavior.\n")
	b.WriteString("  2. If a changed function has NO existing test, write a new test in the project's convention. Stay close to the patterns you see in nearby tests — don't introduce a new framework or style.\n")
	b.WriteString("  3. After writing/updating tests, run the test command above and confirm everything passes.\n")
	b.WriteString("  4. Report what you did in the format:\n")
	b.WriteString("       added: N tests across M files\n")
	b.WriteString("       updated: P tests across Q files\n")
	b.WriteString("       passing: X / Y\n")
	b.WriteString("     Then list any failures with one line each: \"<test name>: <one-sentence reason>\".\n\n")
	b.WriteString("Constraints:\n")
	b.WriteString("  - DO NOT modify the production code the previous agent wrote. Tests describe what the code DOES; if a test fails, that's signal for the user, not for you to rewrite the code to make the test pass.\n")
	b.WriteString("  - DO NOT add new dependencies. Use whatever testing library is already in use.\n")
	b.WriteString("  - If you can't tell what to test (the change was a refactor with no observable behavior change), say so and exit. The harness will run the test command itself to confirm the existing suite still passes.\n")
	return b.String()
}

// runTestCommand executes the project's detected test command from
// the workspace root, capturing output (head+tail truncated) and
// pass/fail counts via shallow parsing. Returns the test result the
// orchestrator surfaces to the user — the harness's source of truth,
// independent of whatever the test agent claimed in its response.
type testRunResult struct {
	ExitCode  int
	Output    string // truncated head+tail of stdout/stderr
	Duration  time.Duration
	Errored   bool   // true if the command itself failed to start (not "tests failed")
	StartErr  string // populated when Errored is true
}

func runTestCommand(ctx context.Context, workspace, command string, timeout time.Duration) testRunResult {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "bash", "-c", command)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "CI=1", "NONINTERACTIVE=1")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return testRunResult{
				ExitCode: -1,
				Errored:  true,
				StartErr: err.Error(),
				Duration: elapsed,
			}
		}
	}
	// Reuse the same head+tail trim the bash tool uses so the
	// output is bounded and readable. 80 lines covers headers +
	// failure summaries for typical suites.
	trimmed := headTailLines(string(out), 40, 40)
	return testRunResult{
		ExitCode: exitCode,
		Output:   trimmed,
		Duration: elapsed,
	}
}

// headTailLines is a string-based mirror of bash.go's truncateLines.
// Kept local because the bash version operates on []byte and lives
// in a different package; importing it would create a coupling
// between orchestrator and the agent tools that's not worth one
// helper. Same shape: head N + tail N + "--- N lines truncated ---".
func headTailLines(s string, head, tail int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= head+tail {
		return s
	}
	dropped := len(lines) - head - tail
	headLines := lines[:head]
	tailLines := lines[len(lines)-tail:]
	return strings.Join(headLines, "\n") +
		fmt.Sprintf("\n--- %d lines truncated ---\n", dropped) +
		strings.Join(tailLines, "\n")
}

// nonTestSourceChanges filters changedPaths down to the files that
// actually warrant test coverage: not test files, not docs, not
// config. Returns the filtered slice in stable order.
func nonTestSourceChanges(changedPaths []string) []string {
	out := make([]string, 0, len(changedPaths))
	for _, p := range changedPaths {
		if isTestPath(p) || isConfigOrDocPath(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// finalAssistantText pulls the last assistant message's text out of
// a result transcript. Mirrors the helper inline in runVerifyPass —
// kept separate so the test pass can run against its own transcript
// without tangling the verify path.
func finalAssistantText(res *agent.Result) string {
	if res == nil {
		return ""
	}
	for i := len(res.Transcript) - 1; i >= 0; i-- {
		m := res.Transcript[i]
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			if t, ok := p.(message.TextContent); ok {
				return t.Text
			}
		}
	}
	return ""
}

// VerifyResult is the outcome of VerifyWorkspace — a ground-truth
// build+test pass over a workspace. The gate review uses it to refuse
// an "approve" recommendation on code that does not actually run.
type VerifyResult struct {
	Ran     bool   // false when no test convention was detected
	OK      bool   // true when the test command exited 0
	Summary string // one-paragraph result, safe to show a human or an LLM
}

// VerifyWorkspace detects the workspace's test convention and runs it,
// returning a deterministic pass/fail. Unlike the LLM reviewer, this
// cannot be reasoned out of a failure: a non-zero exit, a build error,
// or a timeout (a hang/deadlock) all come back as OK=false. The gate
// review calls this so a held — or just-fixed — change is graded by
// execution, not by the reviewer's prose.
func VerifyWorkspace(ctx context.Context, workspace string, timeout time.Duration) VerifyResult {
	conv, ok := detectTestConvention(workspace)
	if !ok || strings.TrimSpace(conv.Run) == "" {
		return VerifyResult{Ran: false, OK: true,
			Summary: "no test convention detected — verification skipped"}
	}
	dir := workspace
	if conv.Dir != "" {
		dir = filepath.Join(workspace, conv.Dir)
	}
	// The verification gate runs inside a review→fix loop, so it must
	// be fast. For Go, `-short` skips long quick-check/simulation
	// tests while still compiling everything (catches build errors)
	// and running correctness tests (catches panics; a deadlock still
	// trips the timeout). Other languages use the detected command.
	cmd := conv.Run
	if conv.Lang == "go" {
		cmd = "go test -short ./..."
	}
	res := runTestCommand(ctx, dir, cmd, timeout)
	switch {
	case res.Errored:
		return VerifyResult{Ran: true, OK: false,
			Summary: fmt.Sprintf("verify command `%s` failed to start: %s", cmd, res.StartErr)}
	case res.ExitCode == 0:
		return VerifyResult{Ran: true, OK: true,
			Summary: fmt.Sprintf("`%s` passed (%s)", cmd, res.Duration.Round(time.Second))}
	default:
		return VerifyResult{Ran: true, OK: false,
			Summary: fmt.Sprintf("`%s` FAILED — exit %d after %s. A timeout here means the code hangs/deadlocks. Output:\n%s",
				cmd, res.ExitCode, res.Duration.Round(time.Second), res.Output)}
	}
}
