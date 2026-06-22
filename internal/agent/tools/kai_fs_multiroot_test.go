package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/projects"
)

// helper: build a multi-root projects.Set with two project dirs
// and a marker file in each so tests can prove the walks reach
// both roots.
func setupTwoRootSet(t *testing.T) (*projects.Set, string, string) {
	t.Helper()
	parent := t.TempDir()
	rootA := filepath.Join(parent, "kai")
	rootB := filepath.Join(parent, "kai-server")
	for _, p := range []string{rootA, rootB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootA, "primary-marker.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "docs-site"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "docs-site", "config.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "docs-site", "page.md"), []byte("# Page\nFind me.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := projects.New(parent, []*projects.Project{
		{Name: "kai", Path: rootA},
		{Name: "kai-server", Path: rootB},
	})
	return set, rootA, rootB
}

// TestKaiFiles_MultiRootFindsSiblingProjects pins the May-5 bug
// the user found: a kai_files call with no `path` arg in a
// multi-root workspace must walk EVERY project, not just the
// primary. Without the fix, "find docs-site" silently misses
// kai-server/docs-site/ because the tool was scoped to kai/
// only.
func TestKaiFiles_MultiRootFindsSiblingProjects(t *testing.T) {
	set, rootA, _ := setupTwoRootSet(t)
	tool := &kaiFilesTool{workspace: rootA, set: set}

	resp, err := tool.Run(context.Background(), ToolCall{
		Input: `{"pattern":"config.ts"}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := resp.Content
	if !strings.Contains(out, "kai-server/docs-site/config.ts") {
		t.Errorf("expected sibling-root match prefixed with project name, got:\n%s", out)
	}
	if !strings.Contains(out, "across 2 roots") {
		t.Errorf("expected multi-root indicator in summary, got:\n%s", out)
	}
}

// TestKaiFiles_SingleRootBackCompat: the prefix is omitted when
// only one root is configured (the legacy single-root behavior).
// Existing callers + tests that don't supply a Set must not see
// new prefixes appear in their results.
func TestKaiFiles_SingleRootBackCompat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiFilesTool{workspace: dir} // no set

	resp, err := tool.Run(context.Background(), ToolCall{
		Input: `{"pattern":"*.go"}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := resp.Content
	if strings.Contains(out, "across") {
		t.Errorf("single-root output should not have multi-root indicator: %s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected main.go in output, got:\n%s", out)
	}
}

// TestKaiGrep_MultiRootScansAllProjects: same coverage as
// kai_files for grep. The user's "can you look for this text?"
// flow worked but only because grep happened to use the right
// scope; without the fix it'd have missed the same way kai_files
// did.
func TestKaiGrep_MultiRootScansAllProjects(t *testing.T) {
	set, rootA, _ := setupTwoRootSet(t)
	tool := &kaiGrepTool{workspace: rootA, set: set}

	resp, err := tool.Run(context.Background(), ToolCall{
		Input: `{"query":"Find me"}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := resp.Content
	if !strings.Contains(out, "kai-server/docs-site/page.md") {
		t.Errorf("expected hit in sibling root with project prefix, got:\n%s", out)
	}
}

// TestKaiTree_MultiRootRendersAllProjects: kai_tree with no
// path must show one labeled subtree per root, not just primary.
// Empty-path is the canonical "show me the workspace" call —
// missing roots there is the failure mode that broke the user's
// "where do the docs live" flow.
func TestKaiTree_MultiRootRendersAllProjects(t *testing.T) {
	set, rootA, _ := setupTwoRootSet(t)
	tool := &kaiTreeTool{workspace: rootA, set: set}

	resp, err := tool.Run(context.Background(), ToolCall{Input: `{}`})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := resp.Content
	for _, want := range []string{
		"multi-root workspace (2 projects",
		"── Project: kai (",
		"── Project: kai-server (",
		"docs-site",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-root tree missing %q, got:\n%s", want, out)
		}
	}
}

// TestKaiTree_PathArgScopesToOneRoot: when the agent explicitly
// asks for a subdirectory, multi-root expansion should NOT fire
// — scope to the primary workspace + that subpath, same as the
// single-root behavior. (The agent uses path="<project>/..."
// to drill into one root.)
func TestKaiTree_PathArgScopesToOneRoot(t *testing.T) {
	set, rootA, _ := setupTwoRootSet(t)
	if err := os.MkdirAll(filepath.Join(rootA, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "subdir", "x.go"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiTreeTool{workspace: rootA, set: set}

	resp, err := tool.Run(context.Background(), ToolCall{
		Input: `{"path":"subdir"}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := resp.Content
	if strings.Contains(out, "multi-root workspace") {
		t.Errorf("path arg should bypass multi-root mode, got:\n%s", out)
	}
	if !strings.Contains(out, "x.go") {
		t.Errorf("expected x.go in scoped tree, got:\n%s", out)
	}
}
