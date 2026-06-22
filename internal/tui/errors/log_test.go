package errors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogLocal_DoesNotCreateKaiDir is the regression: LogLocal
// used to MkdirAll the kai data directory if missing, which
// scattered rogue .kai/ folders at every workspace path that
// happened to lack one. Surfaced 2026-05-11 when the banner fix
// switched workspaceFor to InvokedFrom — a user running kai code
// in a multi-root parent (~/projects/kai) saw .kai materialize
// there because LogLocal eagerly created it on the first error.
//
// Logging is best-effort; if no .kai exists at the workspace,
// silently skipping is the right behavior.
func TestLogLocal_DoesNotCreateKaiDir(t *testing.T) {
	ws := t.TempDir()
	// Workspace has no .kai/ and no .git/ — the dangerous shape.

	LogLocal(ws, UserError{
		Kind:       "test.synthetic",
		Headline:   "synthetic test error",
		LogContext: "context body",
	}, false)

	// Verify the rogue dir was NOT created.
	candidates := []string{
		filepath.Join(ws, ".kai"),
		filepath.Join(ws, ".git", "kai"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			t.Errorf("LogLocal created %s; should have skipped silently when no kai dir exists", c)
		}
	}
}

// TestLogLocal_WritesToExistingKaiDir verifies the happy path:
// when .kai/ already exists, the entry gets appended.
func TestLogLocal_WritesToExistingKaiDir(t *testing.T) {
	ws := t.TempDir()
	kaiDir := filepath.Join(ws, ".kai")
	if err := os.MkdirAll(kaiDir, 0o755); err != nil {
		t.Fatal(err)
	}

	LogLocal(ws, UserError{
		Kind:       "test.synthetic",
		Headline:   "synthetic test error",
		LogContext: "context body",
	}, false)

	body, err := os.ReadFile(filepath.Join(kaiDir, "errors.log"))
	if err != nil {
		t.Fatalf("errors.log not written to existing .kai/: %v", err)
	}
	if !strings.Contains(string(body), "test.synthetic") {
		t.Errorf("errors.log missing expected kind, got: %s", body)
	}
}
