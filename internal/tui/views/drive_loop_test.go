package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/drive"
	"github.com/kaicontext/kai-engine/tasksmd"
)

func writeTasksMD(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "TASKS.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDrive_EngageFromSpec verifies a pasted multi-slice spec becomes a
// gated worklist: slices land in TASKS.md, the first is promoted to In
// progress, drive mode turns on, and the dispatch prompt is focused on
// the first slice.
func TestDrive_EngageFromSpec(t *testing.T) {
	dir := t.TempDir()
	r := &REPL{workDir: dir}

	spec := `Build it in slices.

S0 skeleton: http server + healthz
Accept: go test ./... passes

S1 feature: the real thing
Accept: go test ./... passes`

	dispatch, summary, engaged := r.maybeEngageDriveFromSpec(spec)
	if !engaged {
		t.Fatal("expected drive to engage on a 2-slice spec")
	}
	if !r.driveActive {
		t.Fatal("driveActive should be true after engage")
	}
	if !strings.Contains(dispatch, "S0 skeleton") {
		t.Errorf("dispatch prompt should focus S0, got: %q", dispatch)
	}
	if !strings.Contains(summary, "2 slices") {
		t.Errorf("summary should mention 2 slices, got: %q", summary)
	}

	tk, _ := tasksmd.Load(dir)
	if len(tk.InProgress) != 1 || tk.InProgress[0].Subject != "S0 skeleton: http server + healthz" {
		t.Fatalf("S0 should be in progress: %+v", tk.InProgress)
	}
	if len(tk.Pending) != 1 || tk.Pending[0].Subject != "S1 feature: the real thing" {
		t.Fatalf("S1 should be pending: %+v", tk.Pending)
	}
}

func TestDrive_EngageRequiresMultipleSlices(t *testing.T) {
	dir := t.TempDir()
	r := &REPL{workDir: dir}
	_, _, engaged := r.maybeEngageDriveFromSpec("just fix the login bug")
	if engaged {
		t.Fatal("a single request should not engage drive")
	}
	if r.driveActive {
		t.Fatal("driveActive should stay false")
	}
}

// TestDrive_GatePassAdvancesThenCompletes walks the full loop: two
// slices, each passes its gate, advancing to the next and finally
// draining the worklist.
func TestDrive_GatePassAdvancesThenCompletes(t *testing.T) {
	dir := t.TempDir()
	writeTasksMD(t, dir, `# Tasks

## In progress
- [ ] S0
  Acceptance: run `+"`true`"+`

## Pending
- [ ] S1
  Acceptance: run `+"`true`"+`

## Done
`)
	r := &REPL{workDir: dir, driveActive: true}

	// S0 gate passes → S0 done, S1 promoted, continue armed, still driving.
	next, _ := r.handleDriveGateMsg(DriveGateMsg{TaskSubject: "S0", Result: drive.GateResult{Outcome: drive.GatePass, Command: "true"}})
	if !next.continueArmed {
		t.Error("continue should be armed after a passing gate with more work")
	}
	if !next.driveActive {
		t.Error("drive should still be active mid-worklist")
	}
	tk, _ := tasksmd.Load(dir)
	if len(tk.Done) != 1 || tk.Done[0].Subject != "S0" {
		t.Fatalf("S0 should be done: %+v", tk.Done)
	}
	if len(tk.InProgress) != 1 || tk.InProgress[0].Subject != "S1" {
		t.Fatalf("S1 should be promoted: %+v", tk.InProgress)
	}

	// S1 gate passes → worklist drained, drive disengages.
	next, _ = next.handleDriveGateMsg(DriveGateMsg{TaskSubject: "S1", Result: drive.GateResult{Outcome: drive.GatePass, Command: "true"}})
	if next.driveActive {
		t.Error("drive should disengage when the worklist is drained")
	}
	tk, _ = tasksmd.Load(dir)
	if len(tk.Done) != 2 || len(tk.InProgress) != 0 || len(tk.Pending) != 0 {
		t.Fatalf("worklist should be fully done: %+v", tk)
	}
}

// TestDrive_RenderSummary checks the roll-up reflects done/blocked/
// pending state from TASKS.md and overlays per-slice changed files.
func TestDrive_RenderSummary(t *testing.T) {
	dir := t.TempDir()
	writeTasksMD(t, dir, `# Tasks

## In progress
- [ ] S2 one target
  Blocked: go test ./... failed: TestTranslate

## Pending
- [ ] S3 fan-out

## Done
- [x] S0 skeleton (2026-06-07)
- [x] S1 transcript (2026-06-07)
`)
	r := &REPL{
		workDir: dir,
		driveFiles: map[string][]string{
			"S0 skeleton": {"main.go", "static/index.html", "main_test.go"},
			"S1 transcript": {"transcriber.go"},
		},
	}
	out := r.renderDriveSummary()

	for _, want := range []string{
		"2/4 slices done",
		"✓ S0 skeleton",
		"main.go", "static/index.html", // S0 files shown
		"transcriber.go",               // S1 file shown
		"✗ S2 one target",              // blocked slice
		"go test ./... failed",         // blocked reason
		"○ S3 fan-out",                 // pending slice
		"⚠ not finished",               // overall verdict
		"2 succeeded", "1 blocked", "1 pending",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n--- full ---\n%s", want, out)
		}
	}
}

// TestDrive_WorklistLiveDerivesFromLedger checks the durable signal that
// fixes the "do it" / restart bug: drive is considered engaged whenever
// TASKS.md has an in-progress slice with sibling slices, even when the
// in-memory driveActive flag is false (as it is after a restart or a
// typed continue that isn't the exact sentinel).
func TestDrive_WorklistLiveDerivesFromLedger(t *testing.T) {
	dir := t.TempDir()
	// In-progress slice with both Done and Pending siblings → live.
	writeTasksMD(t, dir, `# Tasks

## In progress
- [ ] S3 fan-out
  Acceptance: two clients each get their language

## Pending
- [ ] S4 glossary

## Done
- [x] S0 (2026-06-07)
`)
	r := &REPL{workDir: dir, driveActive: false}
	if !r.driveWorklistLive() {
		t.Error("a worklist with an in-progress slice + siblings should be live")
	}
	if !r.driveEngaged() {
		t.Error("driveEngaged should be true from the ledger even with driveActive=false")
	}

	// Lone in-progress task, no siblings → ordinary one-shot, not a worklist.
	dir2 := t.TempDir()
	writeTasksMD(t, dir2, `# Tasks

## In progress
- [ ] just one thing
`)
	r2 := &REPL{workDir: dir2}
	if r2.driveWorklistLive() {
		t.Error("a lone in-progress task with no siblings is not a worklist")
	}

	// Nothing in progress → not live.
	dir3 := t.TempDir()
	writeTasksMD(t, dir3, `# Tasks

## Pending
- [ ] a
- [ ] b
`)
	r3 := &REPL{workDir: dir3}
	if r3.driveWorklistLive() {
		t.Error("no in-progress slice → not live")
	}
}

// TestDrive_ReconcileExplicitGateSelfHeals verifies the headline of the
// fix: a continue against an in-progress slice whose explicit gate
// already passes on disk ticks it off and advances WITHOUT re-running
// the agent. A prose-gated slice does not reconcile (it falls through to
// a normal dispatch) so a repo that is green for unrelated reasons can't
// auto-complete it.
func TestDrive_ReconcileExplicitGateSelfHeals(t *testing.T) {
	dir := t.TempDir()
	writeTasksMD(t, dir, `# Tasks

## In progress
- [ ] S3 done-in-code
  Acceptance: run `+"`true`"+`

## Pending
- [ ] S4 next

## Done
- [x] S0 (2026-06-07)
`)
	r := &REPL{workDir: dir, driveActive: false}

	cmd, ok := r.maybeDriveReconcileCmd("focus prompt")
	if !ok {
		t.Fatal("explicit-gate in-progress slice should reconcile")
	}
	msg := cmd().(DriveReconcileMsg)
	if msg.Result.Outcome != drive.GatePass {
		t.Fatalf("`true` gate should pass, got %v", msg.Result.Outcome)
	}
	next, _ := r.handleDriveReconcileMsg(msg)

	tk, _ := tasksmd.Load(dir)
	if len(tk.Done) != 2 || tk.Done[1].Subject != "S3 done-in-code" {
		t.Fatalf("S3 should be ticked off without re-running: %+v", tk.Done)
	}
	if len(tk.InProgress) != 1 || tk.InProgress[0].Subject != "S4 next" {
		t.Fatalf("S4 should be promoted: %+v", tk.InProgress)
	}
	if !next.continueArmed {
		t.Error("continue should be armed to work the next slice")
	}

	// A prose-gated in-progress slice must NOT reconcile.
	dir2 := t.TempDir()
	writeTasksMD(t, dir2, `# Tasks

## In progress
- [ ] S5 join
  Acceptance: scan resolves to the page and reconnects within 3s

## Done
- [x] S0 (2026-06-07)
`)
	r2 := &REPL{workDir: dir2}
	if _, ok := r2.maybeDriveReconcileCmd("focus"); ok {
		t.Error("prose-gated slice must not reconcile from a generic gate")
	}
}

// TestDrive_GateFailHoldsSlice verifies a failing gate keeps the slice
// In progress (annotated Blocked) and re-arms continue so the next Enter
// retries the same slice — it does NOT advance.
func TestDrive_GateFailHoldsSlice(t *testing.T) {
	dir := t.TempDir()
	writeTasksMD(t, dir, `# Tasks

## In progress
- [ ] S0
  Acceptance: run `+"`false`"+`

## Pending
- [ ] S1

## Done
`)
	r := &REPL{workDir: dir, driveActive: true}

	next, _ := r.handleDriveGateMsg(DriveGateMsg{
		TaskSubject: "S0",
		Result:      drive.GateResult{Outcome: drive.GateFail, Command: "false", Output: "exit status 1"},
	})
	if !next.continueArmed {
		t.Error("continue should be armed to retry the failed slice")
	}
	if !next.driveActive {
		t.Error("drive should remain active on a failed gate")
	}
	tk, _ := tasksmd.Load(dir)
	if len(tk.InProgress) != 1 || tk.InProgress[0].Subject != "S0" {
		t.Fatalf("failed slice must stay in progress, not advance: %+v", tk)
	}
	if len(tk.Done) != 0 {
		t.Fatalf("nothing should be done on a failed gate: %+v", tk.Done)
	}
	if !strings.Contains(tk.InProgress[0].Body, "Blocked:") {
		t.Errorf("failed slice should carry a Blocked annotation: %q", tk.InProgress[0].Body)
	}
	if len(tk.Pending) != 1 || tk.Pending[0].Subject != "S1" {
		t.Fatalf("S1 should remain pending (not started): %+v", tk.Pending)
	}
}
