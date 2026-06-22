package views

import (
	"strings"
	"testing"
	"time"
)

// TestTaskProgress_RenderPendingMinimal: a fresh TaskProgress with
// no transitions renders one row per step with the pending glyph
// and no metrics.
func TestTaskProgress_RenderPendingMinimal(t *testing.T) {
	tp := NewTaskProgress("Adding rate limiting", []string{"Read auth.py", "Add middleware"})
	out := tp.View()
	if !strings.Contains(out, "Read auth.py") {
		t.Errorf("missing first step: %q", out)
	}
	if !strings.Contains(out, "Add middleware") {
		t.Errorf("missing second step: %q", out)
	}
	if !strings.Contains(out, "Adding rate limiting") {
		t.Errorf("missing title: %q", out)
	}
	// Pending steps should not render metrics.
	if strings.Contains(out, "(0s") || strings.Contains(out, "(1s") {
		t.Errorf("pending steps should not show duration: %q", out)
	}
}

// TestTaskProgress_StartFinishLifecycle: starting and finishing a
// step transitions status correctly and stamps duration.
func TestTaskProgress_StartFinishLifecycle(t *testing.T) {
	tp := NewTaskProgress("", []string{"a", "b"})
	tp.StartStep(0, 100)
	if tp.Steps[0].Status != StepInProgress {
		t.Errorf("start should set InProgress, got %v", tp.Steps[0].Status)
	}
	if tp.Steps[0].StartedAt.IsZero() {
		t.Errorf("StartedAt should be stamped")
	}
	// Simulate work elapsing.
	tp.Steps[0].StartedAt = time.Now().Add(-2 * time.Second)
	tp.FinishStep(0, 250)
	if tp.Steps[0].Status != StepDone {
		t.Errorf("finish should set Done, got %v", tp.Steps[0].Status)
	}
	if tp.Steps[0].Duration < time.Second {
		t.Errorf("Duration should be ~2s, got %v", tp.Steps[0].Duration)
	}
	if tp.Steps[0].TokensUsed != 150 {
		t.Errorf("TokensUsed delta = %d, want 150", tp.Steps[0].TokensUsed)
	}
}

// TestTaskProgress_AllDoneAndFailures: the AllDone / HasFailures
// helpers reflect the right lifecycle predicates.
func TestTaskProgress_AllDoneAndFailures(t *testing.T) {
	tp := NewTaskProgress("", []string{"a", "b"})
	if tp.AllDone() {
		t.Errorf("fresh task should not be AllDone")
	}
	tp.StartStep(0, 0)
	tp.FinishStep(0, 0)
	tp.StartStep(1, 0)
	tp.FailStep(1, "boom")
	if !tp.AllDone() {
		t.Errorf("done+failed should be AllDone")
	}
	if !tp.HasFailures() {
		t.Errorf("HasFailures should be true after FailStep")
	}
	out := tp.View()
	if !strings.Contains(out, "boom") {
		t.Errorf("failure reason missing from view: %q", out)
	}
}

// TestTaskProgress_OutOfBoundsIsNoop: parser bugs shouldn't crash the
// TUI, so out-of-range index updates must be silent no-ops.
func TestTaskProgress_OutOfBoundsIsNoop(t *testing.T) {
	tp := NewTaskProgress("", []string{"a"})
	tp.StartStep(99, 0)
	tp.FinishStep(-1, 0)
	tp.FailStep(7, "x")
	if tp.Steps[0].Status != StepPending {
		t.Errorf("out-of-range writes shouldn't have touched step 0")
	}
}

// TestTaskProgress_ActiveReflectsInProgress
func TestTaskProgress_ActiveReflectsInProgress(t *testing.T) {
	tp := NewTaskProgress("", []string{"a"})
	if tp.Active() {
		t.Errorf("fresh task with all pending isn't Active")
	}
	tp.StartStep(0, 0)
	if !tp.Active() {
		t.Errorf("task with InProgress step should be Active")
	}
	tp.FinishStep(0, 0)
	if tp.Active() {
		t.Errorf("task with everything Done isn't Active")
	}
}

// TestFormatStepDuration_BoundaryCases: sub-second floors to 1s; a
// minute-plus uses "Nm Xs"; whole minutes drop the seconds part.
func TestFormatStepDuration_BoundaryCases(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond: "1s",
		12 * time.Second:       "12s",
		60 * time.Second:       "1m",
		67 * time.Second:       "1m 7s",
		120 * time.Second:      "2m",
	}
	for d, want := range cases {
		if got := formatStepDuration(d); got != want {
			t.Errorf("formatStepDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestFormatStepTokens_Suffixes: integers get k/m suffixes the way
// the spec describes — 1.2k, 3.1k, 1.0m. Below 1000 stays raw.
func TestFormatStepTokens_Suffixes(t *testing.T) {
	cases := map[int]string{
		42:        "42",
		999:       "999",
		1200:      "1.2k",
		3100:      "3.1k",
		1_500_000: "1.5m",
	}
	for n, want := range cases {
		if got := formatStepTokens(n); got != want {
			t.Errorf("formatStepTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
