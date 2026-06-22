package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildVerifyPrompt_IncludesPlanAndDiff verifies the prompt now
// surfaces both the task description and the list of files actually
// changed, plus the two-part rubric (plan completeness + runtime).
func TestBuildVerifyPrompt_IncludesPlanAndDiff(t *testing.T) {
	original := "Update extractMutatedPaths in runner.go and update its test in runner_test.go"
	bashCmd := "go test ./internal/agent/..."
	changed := []string{"internal/agent/runner.go"} // test file deliberately missing

	out := buildVerifyPrompt(original, bashCmd, changed)

	// Task description appears verbatim so the verify agent can
	// re-read what was supposed to happen.
	if !strings.Contains(out, "extractMutatedPaths") {
		t.Errorf("prompt missing task description content: %s", out)
	}
	// File list appears so the agent can compare plan vs delivery.
	if !strings.Contains(out, "internal/agent/runner.go") {
		t.Errorf("prompt missing changed-path entry: %s", out)
	}
	// Both rubric parts are present.
	if !strings.Contains(out, "PART A") || !strings.Contains(out, "PART B") {
		t.Errorf("prompt missing two-part structure: %s", out)
	}
	// The new INCOMPLETE sentinel is documented for the agent.
	if !strings.Contains(out, "INCOMPLETE:") {
		t.Errorf("prompt missing INCOMPLETE sentinel guidance: %s", out)
	}
	// The bash command is included for the runtime re-run.
	if !strings.Contains(out, bashCmd) {
		t.Errorf("prompt missing bash command: %s", out)
	}
}

// TestBuildVerifyPrompt_EmptyChangedPaths covers the defensive path:
// the orchestrator shouldn't invoke verify with no edits, but if it
// somehow does the prompt should degrade gracefully rather than
// emit a misleading empty bullet list.
func TestBuildVerifyPrompt_EmptyChangedPaths(t *testing.T) {
	out := buildVerifyPrompt("task X", "ls", nil)
	if !strings.Contains(out, "(none recorded") {
		t.Errorf("expected explicit empty-list marker, got: %s", out)
	}
}

func TestClassifyVerifyTranscript_Sentinels(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		edits    int
		expected verifyOutcome
	}{
		{"verified clean", "VERIFIED — homepage renders.", 0, verifyPassed},
		{"blocked", "BLOCKED: dev server won't start.", 0, verifyBlocked},
		{"incomplete", "INCOMPLETE: test file in runner_test.go wasn't updated.", 0, verifyIncomplete},
		{"incomplete-beats-verified", "INCOMPLETE: foo missing. VERIFIED runtime.", 0, verifyIncomplete},
		{"edits-beat-everything", "VERIFIED but I applied a fix.", 1, verifyApplied},
		{"no sentinel", "I looked around but I'm not sure.", 0, verifyUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyVerifyTranscript(c.text, c.edits)
			if got != c.expected {
				t.Errorf("got %d, want %d (text=%q edits=%d)", got, c.expected, c.text, c.edits)
			}
		})
	}
}

func TestVerifySummary_AllOutcomes(t *testing.T) {
	cases := []struct {
		outcome  verifyOutcome
		edits    int
		contains string
	}{
		{verifyPassed, 0, "plan is complete"},
		{verifyApplied, 2, "2 additional edit"},
		{verifyIncomplete, 0, "planned work missing"},
		{verifyBlocked, 0, "could not run"},
		{verifyUnknown, 0, "without a clear pass/fail"},
	}
	for _, c := range cases {
		got := verifySummary(c.outcome, c.edits)
		if !strings.Contains(got, c.contains) {
			t.Errorf("outcome %d summary = %q; expected to contain %q", c.outcome, got, c.contains)
		}
	}
}

// TestVerifyWorkspace_PassAndFail exercises the ground-truth gate the
// gate review relies on: a workspace whose tests pass returns OK=true,
// and one whose test panics returns OK=false. This is what makes the
// review→fix loop unable to "approve" code that does not run.
func TestVerifyWorkspace_PassAndFail(t *testing.T) {
	write := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module verifytmp\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x_test.go"),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("passing suite -> OK", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "package verifytmp\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n")
		vr := VerifyWorkspace(context.Background(), dir, 2*time.Minute)
		if !vr.Ran || !vr.OK {
			t.Fatalf("expected Ran && OK, got %+v", vr)
		}
	})

	t.Run("panicking test -> not OK", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "package verifytmp\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) { panic(\"boom\") }\n")
		vr := VerifyWorkspace(context.Background(), dir, 2*time.Minute)
		if !vr.Ran || vr.OK {
			t.Fatalf("expected Ran && !OK for a panicking test, got %+v", vr)
		}
	})

	t.Run("no go module -> skipped, not a failure", func(t *testing.T) {
		vr := VerifyWorkspace(context.Background(), t.TempDir(), time.Minute)
		if vr.Ran || !vr.OK {
			t.Fatalf("empty dir should skip (Ran=false, OK=true), got %+v", vr)
		}
	})
}
