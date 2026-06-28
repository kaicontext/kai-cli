package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// setupGoWorkspace creates a tiny Go module with one file and returns
// the workspace path and the touched file's absolute path. The caller
// can mutate the file before running runBuildAfterEdit to simulate
// either a clean build or a compile failure.
func setupGoWorkspace(t *testing.T, body string) (workspace, abs string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	abs = filepath.Join(pkg, "main.go")
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, abs
}

func TestRunBuildAfterEdit_SuccessAppendsOK(t *testing.T) {
	ws, abs := setupGoWorkspace(t, "package pkg\n\nfunc Hello() string { return \"hi\" }\n")

	calls := []message.ToolCall{
		{ID: "e1", Name: "edit", Input: `{"file_path": "` + abs + `"}`},
	}
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "[file written]"},
	}

	ran := runBuildAfterEdit(context.Background(), Options{Workspace: ws}, calls, parts)
	if !ran {
		t.Fatalf("expected build to run")
	}
	tr := parts[0].(message.ToolResult)
	if !strings.Contains(tr.Content, "[auto-build: OK") {
		t.Errorf("expected OK trailer, got: %q", tr.Content)
	}
}

func TestRunBuildAfterEdit_FailureAppendsDiagnostic(t *testing.T) {
	// File has an unused import — go build will fail.
	ws, abs := setupGoWorkspace(t, "package pkg\n\nimport \"fmt\"\n\nfunc Hello() string { return \"hi\" }\n")

	calls := []message.ToolCall{
		{ID: "e1", Name: "edit", Input: `{"file_path": "` + abs + `"}`},
	}
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "[file written]"},
	}

	ran := runBuildAfterEdit(context.Background(), Options{Workspace: ws}, calls, parts)
	if !ran {
		t.Fatalf("expected build to run")
	}
	tr := parts[0].(message.ToolResult)
	if !strings.Contains(tr.Content, "[auto-build: FAIL") {
		t.Errorf("expected FAIL trailer, got: %q", tr.Content)
	}
	if !strings.Contains(tr.Content, "imported and not used") {
		t.Errorf("expected compiler diagnostic in trailer, got: %q", tr.Content)
	}
}

func TestRunBuildAfterEdit_NoOptIn_NoOp(t *testing.T) {
	ws, abs := setupGoWorkspace(t, "package pkg\n\nfunc Hello() string { return \"hi\" }\n")

	calls := []message.ToolCall{
		{ID: "e1", Name: "edit", Input: `{"file_path": "` + abs + `"}`},
	}
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "[file written]"},
	}

	ran := runBuildAfterEdit(context.Background(), Options{Workspace: ws, NoBuildAfterEdit: true}, calls, parts)
	if ran {
		t.Errorf("expected no build when NoBuildAfterEdit is set")
	}
	tr := parts[0].(message.ToolResult)
	if strings.Contains(tr.Content, "[auto-build:") {
		t.Errorf("trailer should not appear when disabled, got: %q", tr.Content)
	}
}

func TestRunBuildAfterEdit_SkipsErroredEdits(t *testing.T) {
	ws, abs := setupGoWorkspace(t, "package pkg\n\nfunc Hello() string { return \"hi\" }\n")

	calls := []message.ToolCall{
		{ID: "e1", Name: "edit", Input: `{"file_path": "` + abs + `"}`},
	}
	parts := []message.ContentPart{
		message.ToolResult{ToolCallID: "e1", Name: "edit", Content: "rejected by gate", IsError: true},
	}

	ran := runBuildAfterEdit(context.Background(), Options{Workspace: ws}, calls, parts)
	if ran {
		t.Errorf("errored edits should not trigger a build")
	}
}

func TestPickBuildScope_GoDetection(t *testing.T) {
	ws, abs := setupGoWorkspace(t, "package pkg\n")
	s := pickBuildScope(ws, abs)
	if s.ecosystem != "go" {
		t.Errorf("ecosystem: got %q, want go", s.ecosystem)
	}
	if s.cwd != filepath.Dir(abs) {
		t.Errorf("cwd: got %q, want %q", s.cwd, filepath.Dir(abs))
	}
}

func TestPickBuildScope_UnrecognizedExtensionReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(abs, []byte("hi"), 0o644)
	s := pickBuildScope(dir, abs)
	if s.ecosystem != "" {
		t.Errorf("expected empty ecosystem for non-source extension, got %q", s.ecosystem)
	}
}
