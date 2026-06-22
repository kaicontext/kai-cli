package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// build_check.go: the most basic semantic-correctness check possible.
// Run the relevant ecosystem's compile/type-check command against the
// integrated state before the safety gate classifies. If the worker
// introduced a compile error (typo, renamed identifier, hallucinated
// method name), the check fails and the run gets marked Failed with
// the compiler output as the reason.
//
// Why this exists: round-17 dogfood (2026-05-13). Worker emitted
// `r.renderPlanBanner()` — a method that doesn't exist (real name is
// `renderPlanMenu`). Defensive chain caught nothing: edit was made
// (zero-edits guard skipped), plan-coverage was at threshold (silent),
// blast=4 so safety gate held but with no correctness reason. User
// saw "HELD blast 4" and approved a non-compiling snapshot.
//
// Ecosystem coverage: Go (go build), TypeScript (tsc --noEmit), Rust
// (cargo check). Each ecosystem is detected by the presence of its
// manifest anywhere under repoDir. Multiple ecosystems can run if the
// repo is polyglot (e.g. Go backend + TS frontend in one repo); the
// run is marked Failed if ANY check fails. Goal is "the compiler/
// type-checker accepts this code," not "the full build pipeline
// succeeds" — bundling, linking, etc. are intentionally out of scope.

const buildCheckTimeout = 90 * time.Second

// buildCheckResult summarizes one or more ecosystem checks. Err is
// non-nil when at least one check failed (compile error or timeout);
// the Output captures the combined diagnostic from the failing
// check(s). Ran=false means no ecosystems were detected (or the env
// override disabled checking).
type buildCheckResult struct {
	Ran       bool          // false when skipped (no recognized manifest / env disabled)
	Err       error         // non-nil on any check failure or timeout
	Output    string        // truncated stderr/stdout when Err != nil
	Ecosystem string        // which ecosystem produced the failure ("go", "ts", "rust")
	Took      time.Duration // wall time for the failing check

	// Failures is the set of failing units parsed from Output — Go
	// package import paths where parseable, else a single
	// "ecosystem:<name>" sentinel. Empty when Err == nil. This is the
	// currency of the baseline delta: the gate blocks only on units
	// present after the change that were NOT present in the pre-run
	// baseline (see newFailures), so a tree the user handed us already
	// broken doesn't get blamed on the agent.
	Failures map[string]bool
}

// Reason returns a one-line gate reason suitable for verdict.Reasons.
// Truncates the build output to keep snapshot payloads small.
func (r buildCheckResult) Reason() string {
	if r.Err == nil {
		return ""
	}
	out := strings.TrimSpace(r.Output)
	const cap = 1800
	if len(out) > cap {
		out = out[:cap] + "\n…(truncated)"
	}
	return fmt.Sprintf("build check failed (%s, %s): %s\n%s",
		r.Ecosystem, r.Took.Round(time.Millisecond), r.Err, out)
}

// ecosystemCheck pairs an ecosystem label with its check command. The
// detect function returns the directory the check should run from
// (the manifest's parent), or "" if the ecosystem isn't present.
type ecosystemCheck struct {
	name    string
	detect  func(repoDir string) string
	command []string // argv; ["go", "build", "./..."] etc.
}

var defaultEcosystems = []ecosystemCheck{
	{
		// 'go build ./...' alone misses two classes of bugs the
		// 2026-05-28 dogfood pinned:
		//   1. Undeclared method/field references in TEST files.
		//      `go build` skips *_test.go, so an executor that wrote
		//      `db.AddNode(...)` (no such method) or `e.ID` (no such
		//      field) compiles cleanly and the verify pass green-lights
		//      it. The bug surfaces only when someone later runs the
		//      tests.
		//   2. Suspicious constructs `go vet` catches that the
		//      compiler accepts (Printf format mismatches, unreachable
		//      code, lock copies, etc.). Cheap to add and routinely
		//      catches real bugs.
		// Both fixes here. `go vet ./...` runs the vet analyzers on
		// production AND test code. `go test ./... -run none -count=1
		// -timeout 30s` compiles every test package without executing
		// a single test (Go's idiom for type-check-only), surfacing
		// undeclared symbols in *_test.go files. Whole sequence stays
		// well under the 90s buildCheckTimeout for any reasonable repo.
		name:    "go",
		detect:  findManifest("go.mod"),
		command: []string{"sh", "-c", "go build ./... && go vet ./... && go test ./... -run none -count=1"},
	},
	{
		// Detection requires BOTH a tsconfig.json AND a locally-installed
		// typescript package. Without the second condition, `npx
		// --no-install tsc` finds a tsconfig in a sibling subtree (e.g.
		// kai-desktop/frontend) but no installed typescript and exits
		// with "This is not the tsc command you are looking for", which
		// the run-once loop reads as a compile failure and rejects the
		// integrate. The 2026-05-13 dogfood pinned this on a pure-Go
		// change that never touched a single .ts file.
		//
		// `tsc --noEmit` honors the project's tsconfig include/exclude,
		// so whether *.test.ts files are checked depends on the
		// project's config. Most projects DO include them (default
		// include is "**/*"). For projects with restrictive includes
		// that intentionally exclude tests, we accept the same gap as
		// the user's own `npm run typecheck`.
		name:    "ts",
		detect:  detectTSEcosystem,
		command: []string{"npx", "--no-install", "tsc", "--noEmit"},
	},
	{
		// `cargo check` (no --tests) skips test code, mirroring Go's
		// pre-fix problem. `cargo check --tests --quiet` type-checks
		// the crate AND every #[test] / #[cfg(test)] mod, surfacing
		// undeclared-symbol bugs in tests at verify time. Adds a few
		// seconds to the check for crates with substantial test code;
		// still well under buildCheckTimeout.
		name:    "rust",
		detect:  findManifest("Cargo.toml"),
		command: []string{"cargo", "check", "--tests", "--quiet"},
	},
	{
		// Python: prefer mypy (most projects already have a mypy.ini /
		// pyproject.toml [tool.mypy] section). Falls back to pyright via
		// npx if mypy isn't installed. Both type-check test files by
		// default. Skipped entirely if neither tool is reachable —
		// Python without static type checking is the common case and
		// not something we can synthesize one for.
		//
		// detectPythonEcosystem checks BOTH a pyproject.toml/setup.py
		// AND a usable type checker before claiming the ecosystem.
		// Without the tool check, every Python repo would fail verify
		// on machines without mypy installed.
		name:    "python",
		detect:  detectPythonEcosystem,
		command: []string{"sh", "-c", "command -v mypy >/dev/null && mypy --no-error-summary . || command -v pyright >/dev/null && pyright || exit 0"},
	},
	{
		// JS-without-TypeScript: detected by package.json WITHOUT a
		// tsconfig.json (the ts entry above handles the TS case). The
		// check runs `node --check` against every .js / .mjs / .cjs file
		// under src/ (parses without executing — surfaces syntax errors
		// in production AND test files since *.test.js is the same JS
		// dialect). Doesn't type-check (JS has no static types), but
		// catches the equivalent symbol-mismatch class via 'undefined
		// reference at parse time' for top-level declarations.
		//
		// Pure-JS verify is genuinely limited compared to typed
		// ecosystems; node --check is the strongest deterministic
		// check we can run without project-specific lint config.
		name:    "js",
		detect:  detectJSEcosystem,
		command: []string{"sh", "-c", `find src -type f \( -name "*.js" -o -name "*.mjs" -o -name "*.cjs" \) -print0 2>/dev/null | xargs -0 -n 50 node --check`},
	},
}

// detectPythonEcosystem returns the project root only when both a
// Python manifest exists AND mypy or pyright is reachable. Returning
// empty when no checker is installed avoids the "verify fails because
// the operator's machine doesn't have mypy" trap — same shape as
// detectTSEcosystem's typescript install check.
func detectPythonEcosystem(repoDir string) string {
	dir := findManifest("pyproject.toml")(repoDir)
	if dir == "" {
		dir = findManifest("setup.py")(repoDir)
	}
	if dir == "" {
		return ""
	}
	if _, err := exec.LookPath("mypy"); err == nil {
		return dir
	}
	if _, err := exec.LookPath("pyright"); err == nil {
		return dir
	}
	// Manifest present but no checker installed — skip rather than fail.
	return ""
}

// detectJSEcosystem returns the package.json directory only when no
// tsconfig.json is present at or above it (otherwise the TS entry
// handles type-checking and this entry would double-check the same
// code with weaker semantics). Returns empty if node isn't on PATH
// (the check command needs it).
func detectJSEcosystem(repoDir string) string {
	pkgDir := findManifest("package.json")(repoDir)
	if pkgDir == "" {
		return ""
	}
	// If tsconfig is anywhere at or above pkgDir up to repoDir, the
	// TS entry handles it. Don't double-check.
	for dir := pkgDir; ; {
		if _, err := os.Stat(filepath.Join(dir, "tsconfig.json")); err == nil {
			return ""
		}
		if dir == repoDir {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if _, err := exec.LookPath("node"); err != nil {
		return ""
	}
	return pkgDir
}

// detectTSEcosystem returns the tsconfig.json directory only when
// typescript is actually installed (node_modules/typescript exists
// at the manifest dir or any ancestor up to repoDir). Without the
// install-check the bare manifest find triggers tsc invocations that
// fail with "This is not the tsc command you are looking for" — npx
// is unable to locate tsc in the workspace package set, but exits
// non-zero, which the gate reads as a compile failure.
func detectTSEcosystem(repoDir string) string {
	manifestDir := findManifest("tsconfig.json")(repoDir)
	if manifestDir == "" {
		return ""
	}
	// Walk up from the manifest dir looking for a typescript install,
	// stopping at repoDir. Return the MANIFEST dir (not the install
	// dir) when found — that's where tsc must run from to pick up the
	// project's tsconfig. The walk handles the common monorepo layout
	// where apps/web/tsconfig.json shares root node_modules/typescript.
	for dir := manifestDir; ; {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "typescript")); err == nil {
			return manifestDir
		}
		if dir == repoDir {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// runBuildCheck dispatches to each detected ecosystem's check command.
// Returns Ran=false when KAI_SKIP_BUILD_CHECK is set, or when no
// recognized manifest is found anywhere under repoDir. Returns Err set
// to the FIRST failure encountered — short-circuits on failure since
// the caller treats any failure as terminal for the run.
func runBuildCheck(ctx context.Context, repoDir string) buildCheckResult {
	if os.Getenv("KAI_SKIP_BUILD_CHECK") != "" {
		return buildCheckResult{Ran: false}
	}
	var anyDetected bool
	for _, eco := range defaultEcosystems {
		dir := eco.detect(repoDir)
		if dir == "" {
			continue
		}
		anyDetected = true
		res := runOneEcosystem(ctx, eco, dir)
		if res.Err != nil {
			return res
		}
	}
	return buildCheckResult{Ran: anyDetected}
}

func runOneEcosystem(ctx context.Context, eco ecosystemCheck, dir string) buildCheckResult {
	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, buildCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(bctx, eco.command[0], eco.command[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	took := time.Since(start)

	if bctx.Err() == context.DeadlineExceeded {
		return buildCheckResult{
			Ran:       true,
			Err:       fmt.Errorf("timed out after %s", buildCheckTimeout),
			Output:    string(out),
			Ecosystem: eco.name,
			Took:      took,
			Failures:  failingUnits(eco.name, string(out)),
		}
	}
	if err != nil {
		// Distinguish "tool not installed" from "tool ran and reported
		// errors." If the tool wasn't found, treat the check as skipped
		// for this ecosystem rather than failing the run — the user may
		// not have tsc/cargo locally and we don't want to block their
		// Go-edit workflow on a missing TS toolchain.
		if isExecNotFound(err) || isNPXToolMissing(string(out)) {
			return buildCheckResult{Ran: false, Ecosystem: eco.name, Took: took}
		}
		return buildCheckResult{
			Ran:       true,
			Err:       err,
			Output:    string(out),
			Ecosystem: eco.name,
			Took:      took,
			Failures:  failingUnits(eco.name, string(out)),
		}
	}
	return buildCheckResult{Ran: true, Err: nil, Ecosystem: eco.name, Took: took}
}

// failingUnits decomposes a failing check's output into a set of
// comparable identifiers so two runs can be diffed. For Go the unit is
// the package import path (the `# import/path` headers go build/vet/
// test emit, plus `FAIL\timport/path` lines). For ecosystems we can't
// cheaply decompose (ts/rust/python/js), it returns a single
// "ecosystem:<name>" sentinel — coarser, but still enough for the
// delta to answer "was this ecosystem already broken before?".
func failingUnits(eco, output string) map[string]bool {
	if eco == "go" {
		if pkgs := goFailingPackages(output); len(pkgs) > 0 {
			return pkgs
		}
	}
	return map[string]bool{"ecosystem:" + eco: true}
}

// goFailingPackages pulls Go package import paths out of build/vet/test
// output. Two line shapes carry them:
//   - "# import/path"                  (compiler/vet error header)
//   - "# import/path [import/path.test]" (test-binary build header)
//   - "FAIL\timport/path [build failed]" (test runner failure line)
func goFailingPackages(output string) map[string]bool {
	pkgs := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "# "):
			rest := strings.TrimSpace(line[2:])
			// Drop a trailing " [import/path.test]" qualifier.
			if i := strings.IndexByte(rest, ' '); i >= 0 {
				rest = rest[:i]
			}
			if rest != "" {
				pkgs[rest] = true
			}
		case strings.HasPrefix(line, "FAIL\t"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				pkgs[fields[1]] = true
			}
		}
	}
	return pkgs
}

// newFailures returns the failing units present in `after` that were
// NOT failing in `baseline` — the breakage attributable to the change.
// Empty when `after` succeeded, or when every failure it has was
// already present before the run (pre-existing breakage the user
// handed us, which the gate must not blame on the agent).
func newFailures(baseline, after buildCheckResult) map[string]bool {
	if after.Err == nil {
		return nil
	}
	out := map[string]bool{}
	for u := range after.Failures {
		if !baseline.Failures[u] {
			out[u] = true
		}
	}
	return out
}

// findManifest returns a detector that walks repoDir for a file with
// the given name and returns the directory containing the first hit.
// Prunes obviously irrelevant subdirs (.git, node_modules, vendor,
// target, .kai) so polyglot repos with deep frontend/ subtrees don't
// take seconds to detect.
func findManifest(name string) func(string) string {
	return func(repoDir string) string {
		if repoDir == "" {
			return ""
		}
		var hit string
		_ = filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dn := d.Name()
				if dn == ".git" || dn == "node_modules" || dn == "vendor" ||
					dn == "target" || dn == ".kai" || dn == "dist" || dn == "build" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == name {
				hit = filepath.Dir(path)
				return filepath.SkipAll
			}
			return nil
		})
		return hit
	}
}

// isExecNotFound recognizes the "tool not installed" exec error. Used
// so a missing tsc/cargo doesn't fail a Go-only workflow on a machine
// without the optional toolchain installed.
func isExecNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "no such file or directory")
}

// isNPXToolMissing recognizes npx's diagnostic when it found the binary
// it was asked for but the tool isn't actually a working install (the
// classic "found a .bin/tsc shim but no typescript package" case). npx
// exits non-zero so callers see an exec error, but the diagnostic is
// in the captured output — pattern-match it here so a missing TS
// install in a sibling subtree doesn't fail an unrelated Go run.
func isNPXToolMissing(output string) bool {
	return strings.Contains(output, "This is not the tsc command you are looking for") ||
		strings.Contains(output, "npm ERR! could not determine executable to run") ||
		strings.Contains(output, "tsc: not found")
}
