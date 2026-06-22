package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOverviewStructure_FindsNestedGoModules covers the kai-server
// shape the worker thrashed on all morning: a root go.mod with
// nested go.mods inside subdirs. Each nested module is its own
// build/test cwd; without this listing the worker tries
// `go build ./kailab/...` from the wrong cwd and gets "directory
// prefix kailab does not contain main module".
func TestOverviewStructure_FindsNestedGoModules(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(root, "kailab", "go.mod"), "module kailab\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(root, "github.com/kaicontext/kai-core", "go.mod"), "module kai-core\n\ngo 1.22\n")
	// node_modules + .git should be filtered out so we don't list
	// vendored modules as project structure.
	mustWriteFile(t, filepath.Join(root, "node_modules", "react", "package.json"), `{"name":"react"}`)
	mustWriteFile(t, filepath.Join(root, ".git", "config"), "")

	out := overviewStructure(root)
	for _, want := range []string{
		"go modules:",
		"cd ./",
		"module example.com/outer",
		"cd kailab/",
		"module kailab",
		"cd kai-core/",
		"module kai-core",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in structure overview, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "react") {
		t.Errorf("node_modules vendored package leaked into structure overview:\n%s", out)
	}
}

// TestOverviewStructure_NodePackageScripts surfaces script keys
// because the worker needs to know whether `npm test` is even a
// thing in this repo. Values are not surfaced — they're often
// long shell snippets and the worker can view package.json
// itself when it needs the exact command.
func TestOverviewStructure_NodePackageScripts(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "package.json"), `{
		"name": "my-app",
		"scripts": {
			"build": "vite build",
			"test":  "vitest",
			"dev":   "vite"
		}
	}`)

	out := overviewStructure(root)
	for _, want := range []string{"node packages:", "name my-app", "build", "test", "dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in structure overview, got:\n%s", want, out)
		}
	}
}

// TestOverviewStructure_MakefileTargets lists workspace-root
// Makefile targets so the worker doesn't have to view the file
// before reaching for `make install` / `make test`.
func TestOverviewStructure_MakefileTargets(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Makefile"), `
# Comment line — skipped.
VAR := value
.PHONY: install test build

install:
	go install ./cmd/foo

test:
	go test ./...

build:
	go build ./...

# Skip pattern rules
%.o: %.c
	$(CC) -c $<
`)

	out := overviewStructure(root)
	for _, want := range []string{"Makefile targets:", "install", "test", "build"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in structure overview, got:\n%s", want, out)
		}
	}
	// Pattern rules and assignments aren't targets.
	if strings.Contains(out, "%.o") || strings.Contains(out, "VAR") {
		t.Errorf("non-target match leaked into Makefile targets:\n%s", out)
	}
}

// TestOverviewStructure_BoundedDepth confirms we don't recurse
// past depth 3. A package.json at depth 4 should not appear.
func TestOverviewStructure_BoundedDepth(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a", "b", "c", "d", "package.json"), `{"name":"too-deep"}`)
	mustWriteFile(t, filepath.Join(root, "ok", "package.json"), `{"name":"ok"}`)

	out := overviewStructure(root)
	if !strings.Contains(out, "name ok") {
		t.Errorf("expected depth-1 package to be listed, got:\n%s", out)
	}
	if strings.Contains(out, "too-deep") {
		t.Errorf("depth-4 package leaked into structure overview, got:\n%s", out)
	}
}

// TestParseMakeTargets handles the structural cases the regex /
// scanner needs to get right.
func TestParseMakeTargets(t *testing.T) {
	src := `# top comment
VAR := stuff
OTHER ?= 1
include x.mk

.PHONY: all clean test

all: build test
build:
	go build ./...
test:
	go test ./...
clean:
	rm -rf dist

# pattern rule, skipped
%.o: %.c
	$(CC) $<

# double-colon, skipped
maintainer-clean:: clean
	rm -rf .cache
`
	got := parseMakeTargets(src)
	want := map[string]bool{
		"all": true, "build": true, "test": true, "clean": true,
	}
	skip := map[string]bool{
		".PHONY":           true, // declaration, not a target
		"VAR":              true, // := assignment
		"OTHER":            true, // ?= assignment
		"%.o":              true, // pattern rule
		"maintainer-clean": true, // :: rule
		"include":          true,
	}
	for _, name := range got {
		if skip[name] {
			t.Errorf("non-target %q leaked into target list", name)
		}
		if !want[name] {
			t.Errorf("unexpected target %q", name)
		}
	}
	for n := range want {
		found := false
		for _, g := range got {
			if g == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing target %q (got %v)", n, got)
		}
	}
}

// TestOverviewStructure_GoImportPathSurfaced confirms the module
// name is presented as an import path, not just a label. Today's
// dogfood worker initially imported "kai-server/kailab/store"
// (workspace path) instead of "kailab/store" (module name); the
// overview now states the import path explicitly so this class of
// error stops happening.
func TestOverviewStructure_GoImportPathSurfaced(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "kailab", "go.mod"), "module kailab\n\ngo 1.22\n")

	out := overviewStructure(root)
	if !strings.Contains(out, "import path: kailab/...") {
		t.Errorf("expected 'import path: kailab/...' in overview, got:\n%s", out)
	}
}

// TestOverviewStructure_NodeImportNameSurfaced confirms node
// packages get their import specifier (package.json name) called
// out the same way.
func TestOverviewStructure_NodeImportNameSurfaced(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "ui", "package.json"), `{"name":"@org/ui","scripts":{"build":"vite"}}`)

	out := overviewStructure(root)
	if !strings.Contains(out, "import as: @org/ui") {
		t.Errorf("expected 'import as: @org/ui' in overview, got:\n%s", out)
	}
}

// TestOverviewStructure_SharedModulesSection covers the run-4
// hallucination case: kai-server has kailab + kai-core, and
// kailab requires kai-core. The overview should surface that
// kai-core is shared so the planner doesn't assert "two copies
// drifted" without checking.
func TestOverviewStructure_SharedModulesSection(t *testing.T) {
	root := t.TempDir()
	// kai-core: shared dep
	mustWriteFile(t, filepath.Join(root, "github.com/kaicontext/kai-core", "go.mod"), "module kai-core\n\ngo 1.22\n")
	// kailab: requires kai-core
	mustWriteFile(t, filepath.Join(root, "kailab", "go.mod"), `module kailab

go 1.22

require (
	kai-core v0.0.0
	github.com/example/external v1.0.0 // external dep, should not appear
)

replace kai-core => ../kai-core
`)
	// kailab-control: also requires kai-core
	mustWriteFile(t, filepath.Join(root, "kailab-control", "go.mod"), "module kailab-control\n\ngo 1.22\n\nrequire kai-core v0.0.0\n")

	out := overviewStructure(root)
	if !strings.Contains(out, "shared modules") {
		t.Fatalf("expected 'shared modules' section, got:\n%s", out)
	}
	for _, want := range []string{"kai-core at kai-core/", "used by: kailab, kailab-control"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in shared-modules section, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "github.com/example/external") {
		t.Errorf("external dep leaked into shared-modules section:\n%s", out)
	}
}

// TestParseGoModRequires covers the require-block syntax variants
// (single-line, block form, comments, indirect markers).
func TestParseGoModRequires(t *testing.T) {
	dir := t.TempDir()
	gomod := filepath.Join(dir, "go.mod")
	mustWriteFile(t, gomod, `module example

go 1.22

require single-line v1.2.3

require (
	block-one v1.0.0
	block-two v2.0.0 // indirect
	// stand-alone comment
	block-three v3.0.0
)

// non-require trailing line
replace foo => bar
`)
	got := parseGoModRequires(gomod)
	want := map[string]bool{
		"single-line": true,
		"block-one":   true,
		"block-two":   true,
		"block-three": true,
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected require %q", g)
		}
	}
	for n := range want {
		found := false
		for _, g := range got {
			if g == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing require %q (got %v)", n, got)
		}
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
