package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildContextBlock_FindsNestedModule pins the failure mode this
// fixes: the Go module is NOT at the project root (it lives at
// kai/kai-cli/go.mod). Every dogfood trace showed the agent running
// `go build ./...` from the wrong directory. The block must name the
// nested module dir.
func TestBuildContextBlock_FindsNestedModule(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "kai/kai-cli/go.mod", "module kai\n")
	mkfile(t, root, "kai/kai-cli/main.go", "package main\n")
	mkfile(t, root, "kai/README.md", "# kai\n")

	got := buildContextBlock(root)
	if !strings.Contains(got, "kai/kai-cli") {
		t.Errorf("block should name the nested module dir kai/kai-cli, got:\n%s", got)
	}
	if !strings.Contains(got, "go build ./...") {
		t.Errorf("block should give the Go build command, got:\n%s", got)
	}
	if !strings.Contains(got, "cd kai/kai-cli && go build") {
		t.Errorf("block should tell the agent which dir to run from, got:\n%s", got)
	}
}

// TestBuildContextBlock_MultipleStacks confirms a mixed workspace
// reports each module under the right stack.
func TestBuildContextBlock_MultipleStacks(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "server/go.mod", "module server\n")
	mkfile(t, root, "frontend/package.json", `{"name":"f"}`)

	got := buildContextBlock(root)
	if !strings.Contains(got, "server (Go)") {
		t.Errorf("missing Go module, got:\n%s", got)
	}
	if !strings.Contains(got, "frontend (Node)") {
		t.Errorf("missing Node module, got:\n%s", got)
	}
	if !strings.Contains(got, "npm install") {
		t.Errorf("Node module should hint npm install, got:\n%s", got)
	}
}

// TestBuildContextBlock_SkipsVendorAndNodeModules confirms marker
// files inside excluded trees don't pollute the block — a vendored
// go.mod or node_modules/*/package.json is noise, not a build root.
func TestBuildContextBlock_SkipsVendorAndNodeModules(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "app/go.mod", "module app\n")
	mkfile(t, root, "app/vendor/dep/go.mod", "module dep\n")
	mkfile(t, root, "app/node_modules/pkg/package.json", `{"name":"pkg"}`)

	got := buildContextBlock(root)
	if strings.Contains(got, "vendor") || strings.Contains(got, "node_modules") {
		t.Errorf("excluded trees leaked into the block:\n%s", got)
	}
	if !strings.Contains(got, "app (Go)") {
		t.Errorf("real module missing, got:\n%s", got)
	}
}

// TestBuildContextBlock_EmptyWhenNoMarkers: a workspace with no build
// markers yields an empty block (nothing useful to inject).
func TestBuildContextBlock_EmptyWhenNoMarkers(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "docs/readme.md", "# docs\n")
	if got := buildContextBlock(root); got != "" {
		t.Errorf("expected empty block for marker-less workspace, got:\n%s", got)
	}
}

func mkfile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
