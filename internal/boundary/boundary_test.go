package boundary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBoundaryClean is the regression anchor: the live public tree + go.mod
// must pass the guard. If this fails on main, the guard found a real boundary
// violation (or needs fixing) — read the message, it names what tripped.
func TestBoundaryClean(t *testing.T) {
	root := repoRoot(t)
	findings, err := Check(root)
	if err != nil {
		t.Fatalf("Check(%s): %v", root, err)
	}
	if len(findings) > 0 {
		t.Fatalf("clean tree expected, guard reported %d finding(s):\n%s", len(findings), Report(findings))
	}
}

// --- go.mod: replace detection -------------------------------------------

func TestCheckGoMod_ReplaceEngineFails(t *testing.T) {
	gomod := baseGoMod + "\nreplace github.com/kaicontext/kai-engine => ../kai-engine\n"
	assertKind(t, CheckGoMod(gomod), "replace")
	assertMessageContains(t, CheckGoMod(gomod), "../kai-engine")
}

func TestCheckGoMod_ReplaceCoreFails(t *testing.T) {
	gomod := baseGoMod + "\nreplace github.com/kaicontext/kai-core => ../kai-core\n"
	assertKind(t, CheckGoMod(gomod), "replace")
	assertMessageContains(t, CheckGoMod(gomod), "../kai-core")
}

func TestCheckGoMod_ReplaceBlockFormFails(t *testing.T) {
	gomod := baseGoMod + "\nreplace (\n\tgithub.com/kaicontext/kai-engine => ../kai-engine\n)\n"
	assertKind(t, CheckGoMod(gomod), "replace")
}

// A versionless replace ("could capture them" — applies to all versions).
func TestCheckGoMod_VersionlessReplaceFails(t *testing.T) {
	gomod := baseGoMod + "\nreplace github.com/kaicontext/kai-engine v0.1.0 => ../kai-engine\n"
	assertKind(t, CheckGoMod(gomod), "replace")
}

// An unrelated replace of a non-kaicontext module must NOT trip the guard.
func TestCheckGoMod_UnrelatedReplaceIgnored(t *testing.T) {
	gomod := baseGoMod + "\nreplace example.com/foo => ./vendored/foo\n"
	if f := CheckGoMod(gomod); len(f) != 0 {
		t.Fatalf("unrelated replace should be ignored, got: %s", Report(f))
	}
}

// A module path mentioned only inside a comment must not be read as a directive.
func TestCheckGoMod_CommentedReplaceIgnored(t *testing.T) {
	gomod := baseGoMod + "\n// replace github.com/kaicontext/kai-engine => ../kai-engine (local dev only)\n"
	if f := CheckGoMod(gomod); len(f) != 0 {
		t.Fatalf("commented replace should be ignored, got: %s", Report(f))
	}
}

// --- go.mod: missing-require detection ------------------------------------

func TestCheckGoMod_MissingEngineRequireFails(t *testing.T) {
	gomod := strings.ReplaceAll(baseGoMod, "\t"+moduleEngine+" v0.1.0\n", "")
	assertKind(t, CheckGoMod(gomod), "missing-require")
	assertMessageContains(t, CheckGoMod(gomod), moduleEngine)
}

func TestCheckGoMod_MissingCoreRequireFails(t *testing.T) {
	gomod := strings.ReplaceAll(baseGoMod, "\t"+moduleCore+" v0.1.0\n", "")
	assertKind(t, CheckGoMod(gomod), "missing-require")
}

// A module present ONLY via a replace line is not "required".
func TestCheckGoMod_RequireViaReplaceOnlyFails(t *testing.T) {
	gomod := strings.ReplaceAll(baseGoMod, "\t"+moduleEngine+" v0.1.0\n", "")
	gomod += "\nreplace " + moduleEngine + " => ../kai-engine\n"
	kinds := kindsOf(CheckGoMod(gomod))
	if !kinds["missing-require"] || !kinds["replace"] {
		t.Fatalf("expected both missing-require and replace, got %v", kinds)
	}
}

func TestCheckGoMod_CleanPasses(t *testing.T) {
	if f := CheckGoMod(baseGoMod); len(f) != 0 {
		t.Fatalf("clean go.mod should pass, got: %s", Report(f))
	}
}

// A look-alike module path must not satisfy the require check.
func TestCheckGoMod_LookAlikeModuleDoesNotSatisfyRequire(t *testing.T) {
	gomod := strings.ReplaceAll(baseGoMod, moduleEngine+" v0.1.0", moduleEngine+"-extra v0.1.0")
	assertKind(t, CheckGoMod(gomod), "missing-require")
}

// --- tree: engine-source detection ----------------------------------------

func TestCheckTree_EngineSourceDirFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("kai-engine", "agent", "loop.go"), "package agent\n\nfunc Loop() {}\n")
	findings := mustCheckTree(t, dir)
	assertKind(t, findings, "engine-source")
	assertMessageContains(t, findings, "kai-engine")
}

func TestCheckTree_CoreSourceDirFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("internal", "kai-core", "cas.go"), "package cas\n")
	findings := mustCheckTree(t, dir)
	assertKind(t, findings, "engine-source")
	assertMessageContains(t, findings, "kai-core")
}

// --- tree: sentinel detection ---------------------------------------------

func TestCheckTree_PromptSentinelFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("api", "agentprompt", "prompt.go"),
		"package agentprompt\n\nconst p = `EDIT BUDGET: You have ~10 read-tool calls before you should edit`\n")
	findings := mustCheckTree(t, dir)
	assertKind(t, findings, "sentinel")
}

func TestCheckTree_GateSentinelFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("internal", "drive", "drive.go"),
		"package drive\n\nfunc RunGate(workDir string, t tasksmd.Task, timeout time.Duration) GateResult { return GateResult{} }\n")
	findings := mustCheckTree(t, dir)
	assertKind(t, findings, "sentinel")
}

// --- tree: wrapper false-positive guard -----------------------------------

// A legitimate api/ wrapper that merely IMPORTS the engine must not trip the
// source check: imports != source. This is the exact shape of the real
// api/agentprompt/agentprompt.go re-export wrapper.
func TestCheckTree_EngineImportWrapperNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("api", "agentprompt", "agentprompt.go"),
		"package agentprompt\n\nimport engine \"github.com/kaicontext/kai-engine/agentprompt\"\n\ntype Context = engine.Context\n")
	writeFile(t, dir, filepath.Join("api", "workspace", "workspace.go"),
		"package workspace\n\nimport _ \"github.com/kaicontext/kai-core/cas\"\n")
	if f := mustCheckTree(t, dir); len(f) != 0 {
		t.Fatalf("engine-import wrapper must not be flagged as source, got: %s", Report(f))
	}
}

// The guard's own denylist files hold the sentinel literals and must be
// exempt from the scan — otherwise the guard self-trips.
func TestCheckTree_DenylistFilesExempt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join(guardDirRel, "boundary.go"),
		"package boundary\n\nconst x = `EDIT BUDGET: You have ~10 read-tool calls`\n")
	if f := mustCheckTree(t, dir); len(f) != 0 {
		t.Fatalf("denylist file must be exempt, got: %s", Report(f))
	}
}

// ...but the exemption is only the two denylist files: engine content hidden
// in any OTHER file inside the guard's package is still caught (no blind spot).
func TestCheckTree_NonDenylistFileInGuardDirFlagged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join(guardDirRel, "sneaky.go"),
		"package boundary\n\nconst p = `EDIT BUDGET: You have ~10 read-tool calls before you edit`\n")
	assertKind(t, mustCheckTree(t, dir), "sentinel")
}

// A whole engine module vendored under an unrelated directory name (so the
// kai-core/kai-engine directory check misses it) is caught by its go.mod.
func TestCheckTree_NestedEngineModuleFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, filepath.Join("internal", "engine", "go.mod"),
		"module github.com/kaicontext/kai-engine\n\ngo 1.25.0\n")
	findings := mustCheckTree(t, dir)
	assertKind(t, findings, "engine-source")
	assertMessageContains(t, findings, "github.com/kaicontext/kai-engine")
}

// --- Report actionability -------------------------------------------------

func TestReport_IsActionable(t *testing.T) {
	out := Report([]Finding{{Kind: "replace", Message: "replace directive at go.mod line 42: replace ... => ../kai-engine"}})
	for _, want := range []string{"MODULE BOUNDARY CHECK FAILED", "go.mod line 42", "import it, don't copy it here", "CONTRIBUTING.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("Report missing %q in:\n%s", want, out)
		}
	}
}

// --- helpers --------------------------------------------------------------

// baseGoMod is a minimal valid-shaped go.mod that satisfies the guard: both
// engine modules required, no replace.
const baseGoMod = `module kai

go 1.25.0

require (
	github.com/kaicontext/kai-core v0.1.0
	github.com/spf13/cobra v1.10.1
)

require (
	github.com/kaicontext/kai-engine v0.1.0
	golang.org/x/sys v0.46.0 // indirect
)
`

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			first := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
			if first == "module kai" {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (go.mod with `module kai`)")
		}
		dir = parent
	}
}

func mustCheckTree(t *testing.T, dir string) []Finding {
	t.Helper()
	f, err := CheckTree(dir)
	if err != nil {
		t.Fatalf("CheckTree(%s): %v", dir, err)
	}
	return f
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func kindsOf(findings []Finding) map[string]bool {
	m := map[string]bool{}
	for _, f := range findings {
		m[f.Kind] = true
	}
	return m
}

func assertKind(t *testing.T, findings []Finding, kind string) {
	t.Helper()
	if !kindsOf(findings)[kind] {
		t.Fatalf("expected a %q finding, got: %s", kind, Report(findings))
	}
}

func assertMessageContains(t *testing.T, findings []Finding, sub string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Message, sub) {
			return
		}
	}
	t.Fatalf("expected a finding mentioning %q, got: %s", sub, Report(findings))
}
