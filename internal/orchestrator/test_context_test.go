package orchestrator

import (
	"strings"
	"testing"
)

// TestTestContextBlock_GoConventionAndExample: a Go module should
// produce a TEST CONTEXT block that states the standard-testing rule,
// names the banned frameworks, and points at an existing _test.go to
// mirror.
func TestTestContextBlock_GoConventionAndExample(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "kai-cli/go.mod", "module kai\n")
	mkfile(t, root, "kai-cli/repl.go", "package views\n")
	mkfile(t, root, "kai-cli/repl_test.go", "package views\n")

	got := testContextBlock(root)
	if !strings.Contains(got, "Go:") {
		t.Errorf("block should name the Go stack, got:\n%s", got)
	}
	if !strings.Contains(got, "gomega") {
		t.Errorf("block should explicitly name banned frameworks, got:\n%s", got)
	}
	if !strings.Contains(got, "kai-cli/repl_test.go") {
		t.Errorf("block should cite an example test file, got:\n%s", got)
	}
}

// TestTestContextBlock_EmptyWhenNoStack: a directory with no known
// build marker yields no block.
func TestTestContextBlock_EmptyWhenNoStack(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "notes.md", "# notes\n")
	if got := testContextBlock(root); got != "" {
		t.Errorf("expected empty block for an unknown stack, got:\n%s", got)
	}
}
