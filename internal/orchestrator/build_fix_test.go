package orchestrator

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTruncateBuildOutputForSurface_Empty(t *testing.T) {
	if got := truncateBuildOutputForSurface(""); got != "" {
		t.Errorf("empty input should produce empty output, got %q", got)
	}
	if got := truncateBuildOutputForSurface("   \n\n  "); got != "" {
		t.Errorf("whitespace-only input should produce empty output, got %q", got)
	}
}

func TestTruncateBuildOutputForSurface_ShortInput(t *testing.T) {
	in := "line1\nline2\nline3"
	got := truncateBuildOutputForSurface(in)
	if got != in {
		t.Errorf("short input should pass through unchanged.\n got: %q\nwant: %q", got, in)
	}
}

func TestTruncateBuildOutputForSurface_LongInput(t *testing.T) {
	// Construct a 75-line input; expect 50 lines + a "(25 more)" hint.
	var lines []string
	for i := 0; i < 75; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	in := strings.Join(lines, "\n")
	got := truncateBuildOutputForSurface(in)
	gotLines := strings.Split(got, "\n")
	// 50 head lines + 1 trailing hint line = 51
	if len(gotLines) != surfaceOutputMaxLines+1 {
		t.Errorf("expected %d lines (head + hint), got %d", surfaceOutputMaxLines+1, len(gotLines))
	}
	if !strings.Contains(gotLines[surfaceOutputMaxLines], "25 more") {
		t.Errorf("trailing hint should mention 25 more lines, got: %q", gotLines[surfaceOutputMaxLines])
	}
	// First 50 must be the actual heads.
	for i := 0; i < surfaceOutputMaxLines; i++ {
		want := "line" + itoa(i)
		if gotLines[i] != want {
			t.Errorf("line %d: got %q, want %q", i, gotLines[i], want)
		}
	}
}

func TestFormatBuildRegressionReason_RollbackSuccess(t *testing.T) {
	bc := buildCheckResult{
		Ran:       true,
		Err:       errString("exit status 2"),
		Output:    "# example/foo\n./foo.go:42:13: undefined: Bar",
		Ecosystem: "go",
		Failures:  map[string]bool{"example/foo": true},
	}
	// Clean baseline → example/foo is newly broken by the change.
	msg := formatBuildRegressionReason(bc, buildCheckResult{}, nil)
	mustContain(t, msg, "The change broke the build")
	mustContain(t, msg, "Newly failing after this change")
	mustContain(t, msg, "example/foo")
	mustContain(t, msg, "undefined: Bar")
	mustContain(t, msg, "Working tree was restored")
	if strings.Contains(msg, "rollback also failed") {
		t.Errorf("rollback success should not mention rollback failure: %q", msg)
	}
}

func TestFormatBuildRegressionReason_RollbackFailed(t *testing.T) {
	bc := buildCheckResult{
		Ran:       true,
		Err:       errString("exit status 2"),
		Output:    "# example/foo\n./foo.go:42:13: undefined: Bar",
		Ecosystem: "go",
		Failures:  map[string]bool{"example/foo": true},
	}
	msg := formatBuildRegressionReason(bc, buildCheckResult{}, errString("disk full"))
	mustContain(t, msg, "WARNING: working tree rollback also failed")
	mustContain(t, msg, "disk full")
	mustContain(t, msg, "Inspect the working tree manually")
}

func TestNewFailures_ExcludesPreexisting(t *testing.T) {
	baseline := buildCheckResult{
		Ran: true, Err: errString("x"), Ecosystem: "go",
		Failures: map[string]bool{"pkg/a": true},
	}
	after := buildCheckResult{
		Ran: true, Err: errString("x"), Ecosystem: "go",
		Failures: map[string]bool{"pkg/a": true, "pkg/b": true},
	}
	nf := newFailures(baseline, after)
	if nf["pkg/a"] {
		t.Errorf("pre-existing pkg/a must not count as new")
	}
	if !nf["pkg/b"] {
		t.Errorf("pkg/b is newly broken and must count")
	}
	if len(nf) != 1 {
		t.Errorf("expected exactly 1 new failure, got %d: %v", len(nf), nf)
	}

	// Same failures before and after → nothing new → empty.
	if got := newFailures(after, after); len(got) != 0 {
		t.Errorf("identical states should yield no new failures, got %v", got)
	}
	// after clean → nil regardless of baseline.
	if got := newFailures(baseline, buildCheckResult{Ran: true}); len(got) != 0 {
		t.Errorf("clean after-state should yield no new failures, got %v", got)
	}
}

func TestGoFailingPackages(t *testing.T) {
	out := `# kai/internal/graph
internal/graph/graph_test.go:361:13: undefined: NewGraph
# kai/internal/agent/tools [kai/internal/agent/tools.test]
internal/agent/tools/bash.go:1085:24: method WriteByte...
FAIL	kai/internal/other [build failed]`
	pkgs := goFailingPackages(out)
	for _, want := range []string{"github.com/kaicontext/kai-engine/graph", "kai/internal/agent/tools", "kai/internal/other"} {
		if !pkgs[want] {
			t.Errorf("expected %q in failing set, got %v", want, pkgs)
		}
	}
	if len(pkgs) != 3 {
		t.Errorf("expected 3 packages, got %d: %v", len(pkgs), pkgs)
	}
}

func TestCollectTestInventory_CountsTestFuncs(t *testing.T) {
	dir := t.TempDir()
	// A test file with three entry points (Test, Benchmark) plus a
	// non-test helper that must NOT be counted.
	writeFile(t, filepath.Join(dir, "graph_test.go"), `package graph
func TestA(t *testing.T) {}
func TestB(t *testing.T) {}
func BenchmarkC(b *testing.B) {}
func helper() {}
`)
	// A second test file with one Example.
	mkdirAll(t, filepath.Join(dir, "sub"))
	writeFile(t, filepath.Join(dir, "sub", "util_test.go"), `package sub
func ExampleThing() {}
`)
	// A non-test file is ignored entirely.
	writeFile(t, filepath.Join(dir, "graph.go"), `package graph
func TestLooking() {}
`)
	// Pruned directories are skipped even if they hold test files.
	mkdirAll(t, filepath.Join(dir, "vendor"))
	writeFile(t, filepath.Join(dir, "vendor", "dep_test.go"), `package dep
func TestVendored(t *testing.T) {}
`)

	inv := collectTestInventory(dir)
	if got := inv["graph_test.go"]; got != 3 {
		t.Errorf("graph_test.go: expected 3 test funcs, got %d", got)
	}
	if got := inv[filepath.Join("sub", "util_test.go")]; got != 1 {
		t.Errorf("sub/util_test.go: expected 1 test func, got %d", got)
	}
	if _, ok := inv["graph.go"]; ok {
		t.Errorf("non-test file should not be in inventory")
	}
	if _, ok := inv[filepath.Join("vendor", "dep_test.go")]; ok {
		t.Errorf("vendored test file should be pruned")
	}
}

func TestTestsRemoved_DetectsRegressions(t *testing.T) {
	base := testInventory{"a_test.go": 3, "b_test.go": 2, "c_test.go": 1}

	// No regression: a file grew, others held steady.
	if got := testsRemoved(base, testInventory{"a_test.go": 4, "b_test.go": 2, "c_test.go": 1}); got != "" {
		t.Errorf("growth/steady should not be flagged, got: %q", got)
	}

	// Adding a brand-new test file is fine.
	if got := testsRemoved(base, testInventory{"a_test.go": 3, "b_test.go": 2, "c_test.go": 1, "d_test.go": 5}); got != "" {
		t.Errorf("new file should not be flagged, got: %q", got)
	}

	// Whole file deleted.
	got := testsRemoved(base, testInventory{"a_test.go": 3, "c_test.go": 1})
	mustContain(t, got, "b_test.go")
	mustContain(t, got, "entire test file deleted")

	// Function count dropped within a file.
	got = testsRemoved(base, testInventory{"a_test.go": 1, "b_test.go": 2, "c_test.go": 1})
	mustContain(t, got, "a_test.go")
	mustContain(t, got, "3 → 1")
}

func TestBuildFixPrompt_ForbidsTestDeletion(t *testing.T) {
	// The template must carry an explicit, non-loophole instruction that
	// deleting tests is never a valid fix.
	if !strings.Contains(buildFixPromptTemplate, "Deleting tests is never a valid fix") {
		t.Errorf("prompt template missing the test-deletion prohibition")
	}
	if !strings.Contains(buildFixPromptTemplate, "Keep every existing test") {
		t.Errorf("prompt template missing the positive keep-tests instruction")
	}
}

// --- test helpers ---------------------------------------------------

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("expected %q to contain %q", s, sub)
	}
}

// errString is the smallest error implementation usable in tests; we
// don't want to import errors here to keep the test file dependency-
// free, and fmt.Errorf would work too but adds noise.
type errStringT string

func (e errStringT) Error() string { return string(e) }

func errString(s string) error { return errStringT(s) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
