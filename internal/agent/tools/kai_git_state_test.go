package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestKaiGitState_ReportsUncommittedFile is the regression: the tool
// must surface working-tree state, not just commit history. Opus's
// dogfood failure on 2026-05-11 — claiming "the fix is in place" when
// the change was uncommitted — turned on the agent never asking git
// about working-tree state. This test pins the answer the tool gives
// when a file has uncommitted edits.
func TestKaiGitState_ReportsUncommittedFile(t *testing.T) {
	repo := newTestRepo(t)
	target := filepath.Join(repo, "hello.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", "hello.go")
	gitCmd(t, repo, "commit", "-m", "add hello")

	// Now mutate the file without committing — the "fix is in the
	// working tree but not committed" shape we want the agent to
	// detect.
	if err := os.WriteFile(target, []byte("package main\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &kaiGitStateTool{workspace: repo}
	resp, err := tool.Run(context.Background(), ToolCall{
		Input: `{"path":"hello.go"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := responseText(resp)
	if !strings.Contains(body, "working tree:") {
		t.Errorf("expected working-tree line in output, got:\n%s", body)
	}
	if !strings.Contains(body, "hello.go") {
		t.Errorf("expected modified file mentioned, got:\n%s", body)
	}
	if !strings.Contains(body, "last commit") {
		t.Errorf("expected last-commit line, got:\n%s", body)
	}
}

// TestKaiGitState_CleanRepo verifies the no-changes path doesn't
// misreport a clean tree as dirty. Symmetric to the dirty-tree case
// — an agent that gets the answer wrong in either direction will
// mislead the user.
func TestKaiGitState_CleanRepo(t *testing.T) {
	repo := newTestRepo(t)
	target := filepath.Join(repo, "clean.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", "clean.go")
	gitCmd(t, repo, "commit", "-m", "add clean")

	tool := &kaiGitStateTool{workspace: repo}
	resp, err := tool.Run(context.Background(), ToolCall{Input: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	body := responseText(resp)
	if !strings.Contains(body, "clean") {
		t.Errorf("expected 'clean' in output for spotless repo, got:\n%s", body)
	}
}

// TestKaiGitState_NotARepo confirms we surface a clear error rather
// than a confusing git stderr dump when the agent asks about a path
// that isn't under version control.
func TestKaiGitState_NotARepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &kaiGitStateTool{workspace: dir}
	resp, err := tool.Run(context.Background(), ToolCall{Input: `{"path":"x.txt"}`})
	if err != nil {
		t.Fatal(err)
	}
	body := responseText(resp)
	if !strings.Contains(body, "not in a git repository") {
		t.Errorf("expected not-a-repo error, got:\n%s", body)
	}
}

// TestView_AppendsGitFooter pins that the view tool surfaces git
// provenance alongside the file contents. The 2026-05-11 dogfood
// failure — opus claiming "the fix is in place" when the change was
// uncommitted — turned on the agent never seeing the working-tree
// state of the file it just read. Embedding the state in every view
// response makes it impossible to miss.
func TestView_AppendsGitFooter(t *testing.T) {
	repo := newTestRepo(t)
	target := filepath.Join(repo, "code.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", "code.go")
	gitCmd(t, repo, "commit", "-m", "add code")
	// Modify after committing so the file is dirty.
	if err := os.WriteFile(target, []byte("package main\n// uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := fileToolsForWS(repo).View()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "view",
		Input: `{"file_path":"code.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "[git:") {
		t.Errorf("expected git footer in view output, got:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "MODIFIED") {
		t.Errorf("expected MODIFIED state for dirty file, got:\n%s", resp.Content)
	}
}

// TestView_NoGitFooterWhenNotInRepo confirms the non-repo path is
// silent — no scary "not a git repo" footer, just the file. The
// absence of a footer IS the signal that this isn't version-
// controlled, and the agent shouldn't be making deployment claims
// about non-repo files anyway.
func TestView_NoGitFooterWhenNotInRepo(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "loose.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "view",
		Input: `{"file_path":"loose.txt"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "[git:") {
		t.Errorf("did NOT expect git footer outside a repo, got:\n%s", resp.Content)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	gitCmd(t, repo, "config", "user.email", "test@example.com")
	gitCmd(t, repo, "config", "user.name", "Test")
	gitCmd(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func responseText(resp ToolResponse) string {
	if resp.Content == "" {
		return resp.Metadata
	}
	return resp.Content
}
