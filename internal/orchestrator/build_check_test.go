package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBuildCheck_NoManifestSkipsCleanly(t *testing.T) {
	dir := t.TempDir()
	// Empty dir, no go.mod / tsconfig / Cargo.toml.
	res := runBuildCheck(context.Background(), dir)
	if res.Ran {
		t.Errorf("empty repo should skip (Ran=false), got Ran=true")
	}
	if res.Err != nil {
		t.Errorf("empty repo skip should not produce error, got: %v", res.Err)
	}
}

func TestRunBuildCheck_EnvOverrideDisables(t *testing.T) {
	dir := t.TempDir()
	// Put a go.mod in place so the check WOULD run if not overridden.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAI_SKIP_BUILD_CHECK", "1")
	res := runBuildCheck(context.Background(), dir)
	if res.Ran {
		t.Errorf("env override should skip the check, got Ran=true")
	}
}

func TestRunBuildCheck_GoCompileError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testmod\ngo 1.22\n")
	// Reference an undefined identifier — same shape as round-17's
	// `r.renderPlanBanner` hallucination.
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func main() {
	undefinedHallucinatedSymbol()
}
`)
	res := runBuildCheck(context.Background(), dir)
	if !res.Ran {
		t.Fatal("expected check to run against a go.mod project")
	}
	if res.Err == nil {
		t.Fatal("expected build to fail on undefined symbol, got nil error")
	}
	if res.Ecosystem != "go" {
		t.Errorf("expected go ecosystem, got %q", res.Ecosystem)
	}
	reason := res.Reason()
	if !strings.Contains(reason, "build check failed") {
		t.Errorf("reason missing 'build check failed': %q", reason)
	}
	if !strings.Contains(reason, "undefinedHallucinatedSymbol") {
		t.Errorf("reason should name the offending symbol: %q", reason)
	}
}

func TestRunBuildCheck_GoSuccess(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testmod\ngo 1.22\n")
	writeFile(t, filepath.Join(dir, "main.go"), `package main

func main() {}
`)
	res := runBuildCheck(context.Background(), dir)
	if !res.Ran {
		t.Fatal("expected check to run against a go.mod project")
	}
	if res.Err != nil {
		t.Errorf("clean compile should pass, got err: %v\noutput: %s", res.Err, res.Output)
	}
}

func TestFindManifest_PrunesIrrelevantDirs(t *testing.T) {
	dir := t.TempDir()
	// Place go.mod under a nested .git dir — finder should NOT find
	// it (so a repo's checked-in submodule with its own go.mod doesn't
	// get picked up).
	mkdirAll(t, filepath.Join(dir, ".git"))
	writeFile(t, filepath.Join(dir, ".git", "go.mod"), "module ignored\n")
	if got := findManifest("go.mod")(dir); got != "" {
		t.Errorf("finder should prune .git, got %q", got)
	}

	// Now place go.mod in a regular subdir — should find it.
	mkdirAll(t, filepath.Join(dir, "kai-cli"))
	writeFile(t, filepath.Join(dir, "kai-cli", "go.mod"), "module ok\n")
	got := findManifest("go.mod")(dir)
	if got == "" || filepath.Base(got) != "kai-cli" {
		t.Errorf("finder should return kai-cli dir, got %q", got)
	}
}

func TestIsExecNotFound(t *testing.T) {
	if !isExecNotFound(execNotFoundErr("executable file not found")) {
		t.Error("should classify executable-not-found")
	}
	if !isExecNotFound(execNotFoundErr("no such file or directory")) {
		t.Error("should classify no-such-file")
	}
	if isExecNotFound(execNotFoundErr("exit status 1")) {
		t.Error("should NOT classify regular exit code as not-found")
	}
}

// Tiny stub error to drive isExecNotFound.
type execNotFoundErr string

func (e execNotFoundErr) Error() string { return string(e) }

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}
