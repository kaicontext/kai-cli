package views

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kaicontext/kai-engine/drive"
	"github.com/kaicontext/kai-engine/orchestrator"
	"github.com/kaicontext/kai-engine/tasksmd"
)

// driveContinueSentinel is the prompt a bare-Enter "continue" submits
// while drive mode is running. It is recognized in submitLine so a
// continue does NOT disengage drive (only genuine user steering does).
const driveContinueSentinel = "Continue with the pending tasks in TASKS.md."

// driveGateTimeout bounds an acceptance-gate command. go test on a
// fresh module or an npm build can take a while; 120s is generous
// enough not to false-fail a real check while still catching a hung
// dev-server-style acceptance command.
const driveGateTimeout = 120 * time.Second

// DriveGateMsg carries the result of a task's acceptance gate, run
// asynchronously after a successful execute so the Bubble Tea update
// loop never blocks on `go test`.
type DriveGateMsg struct {
	TaskSubject string
	Result      drive.GateResult
}

// maybeEngageDriveFromSpec inspects a fresh user submission. When it
// decomposes into two or more vertical slices it writes them to the
// workspace TASKS.md as Pending, promotes the first to In progress,
// switches the REPL into drive mode, and returns a focused dispatch
// prompt for that first slice plus a scrollback summary. When the input
// is a single request it returns ("", "", false) and the caller
// dispatches the original line unchanged.
func (r *REPL) maybeEngageDriveFromSpec(line string) (dispatch, summary string, engaged bool) {
	tasks := drive.Decompose(line)
	if len(tasks) < 2 {
		return "", "", false
	}
	t, err := tasksmd.Load(r.workDir)
	if err != nil {
		// A malformed existing ledger shouldn't swallow the spec; fall
		// back to a normal one-shot dispatch.
		return "", "", false
	}
	if t.Path == "" {
		t.Path = filepath.Join(r.workDir, "TASKS.md")
	}
	t.Pending = append(t.Pending, tasks...)
	if err := t.Save(); err != nil {
		return "", "", false
	}
	started, ok, err := tasksmd.StartNext(r.workDir)
	if err != nil || !ok {
		return "", "", false
	}
	r.driveActive = true
	r.driveFiles = map[string][]string{}

	var b strings.Builder
	fmt.Fprintf(&b, "📋 Drive mode: decomposed into %d slices → TASKS.md. Working them one gated slice at a time.\n", len(tasks))
	for i, tk := range tasks {
		marker := "·"
		if i == 0 {
			marker = "▸"
		}
		fmt.Fprintf(&b, "  %s %s\n", marker, tk.Subject)
	}
	b.WriteString("After each slice's acceptance gate I'll pause — press Enter to continue, or type to redirect.")

	return buildDriveTaskPrompt(started), strings.TrimRight(b.String(), "\n"), true
}

// buildDriveTaskPrompt produces the per-slice dispatch prompt. It pins
// the agent to the single in-progress slice — the fix for the failure
// where a multi-slice spec drifted across slices instead of finishing
// S0 before starting S1.
func buildDriveTaskPrompt(t tasksmd.Task) string {
	var b strings.Builder
	b.WriteString("You are working a TASKS.md worklist one slice at a time. ")
	b.WriteString("Focus ONLY on the current in-progress slice and implement it completely:\n\n")
	fmt.Fprintf(&b, "Slice: %s\n", t.Subject)
	if t.Body != "" {
		fmt.Fprintf(&b, "Detail: %s\n", t.Body)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "Acceptance gate: %s\n", t.Acceptance)
	}
	b.WriteString("\nDo not start later slices — the harness runs the acceptance gate and advances you to the next slice only after this one passes. Make the acceptance criterion verifiably true.")
	return b.String()
}

// driveWorklistLive reports whether the durable ledger shows a drive
// worklist still in flight: an in-progress slice that has sibling
// slices (already Done, or still Pending). This is the source of truth
// for "is drive engaged?" — derived from TASKS.md, not from an
// in-memory flag — so drive survives a restart and a typed continue
// like "do it" (which is not the exact driveContinueSentinel). A lone
// in-progress task with no siblings is treated as an ordinary one-shot
// request, not a worklist.
func (r *REPL) driveWorklistLive() bool {
	t, err := tasksmd.Load(r.workDir)
	if err != nil || len(t.InProgress) == 0 {
		return false
	}
	return len(t.Done) > 0 || len(t.Pending) > 0
}

// driveEngaged is true when drive should gate-and-advance the ledger:
// either the in-memory flag is set this session, or the durable ledger
// shows a live worklist (the restart / typed-continue recovery path).
func (r *REPL) driveEngaged() bool {
	return r.driveActive || r.driveWorklistLive()
}

// DriveReconcileMsg carries the result of the pre-dispatch reconcile
// gate. Before re-running the agent on a continue, a slice whose
// acceptance carries an explicit, slice-specific command is checked
// against disk: if it already passes, the slice is ticked off and
// skipped instead of needlessly re-executed (the self-heal for a ledger
// left behind by work that already landed).
type DriveReconcileMsg struct {
	TaskSubject string
	Result      drive.GateResult
	Dispatch    string // prompt to dispatch when the slice is NOT yet satisfied
}

// maybeDriveReconcileCmd returns (cmd, true) when a continue should
// first reconcile the in-progress slice against disk: drive is live AND
// that slice carries an explicit acceptance command (not the generic
// project-default fallback). The explicit-only restriction is
// deliberate — a prose slice gated only by `go test ./...` must not be
// auto-ticked from a repo that happens to be green for unrelated
// reasons; those fall through to a normal dispatch so the agent (and
// the human at the pause) still own the call. Returns (nil, false)
// otherwise.
func (r *REPL) maybeDriveReconcileCmd(dispatch string) (tea.Cmd, bool) {
	if !r.driveWorklistLive() {
		return nil, false
	}
	t, err := tasksmd.Load(r.workDir)
	if err != nil || len(t.InProgress) == 0 {
		return nil, false
	}
	cur := t.InProgress[0]
	if _, ok := drive.ExtractGateCommand(cur.Acceptance); !ok {
		return nil, false // prose slice: let the agent work it
	}
	workDir := r.workDir
	return func() tea.Msg {
		res := drive.RunGate(workDir, cur, driveGateTimeout)
		return DriveReconcileMsg{TaskSubject: cur.Subject, Result: res, Dispatch: dispatch}
	}, true
}

// handleDriveReconcileMsg applies a pre-dispatch reconcile result: a
// slice already satisfied on disk is ticked off and the worklist
// advances without re-running the agent; an unsatisfied slice falls
// through to the normal dispatch so the agent does the work.
func (r *REPL) handleDriveReconcileMsg(msg DriveReconcileMsg) (REPL, tea.Cmd) {
	r.spinnerText = ""
	r.clearTransient()
	if msg.Result.Outcome == drive.GatePass {
		tasksmd.CompleteCurrent(r.workDir, time.Now().Format("2006-01-02"))
		r.write(styleStepDone.Render(fmt.Sprintf("✓ %q already satisfied (%s) — ticked off without re-running", msg.TaskSubject, msg.Result.Command)))
		return r.driveAdvance()
	}
	return r.dispatch(msg.Dispatch)
}

// driveGateCmdAfterExecute is called from the ExecuteDoneMsg success
// path while drive mode is active. It returns a command that runs the
// in-progress slice's acceptance gate off the update loop, or nil when
// there is nothing in progress to gate (in which case drive mode winds
// down).
func (r *REPL) driveGateCmdAfterExecute() tea.Cmd {
	t, err := tasksmd.Load(r.workDir)
	if err != nil || len(t.InProgress) == 0 {
		r.driveActive = false
		return nil
	}
	cur := t.InProgress[0]
	if cmd, ok := drive.ResolveGateCommand(r.workDir, cur.Acceptance); ok {
		r.write(styleDim.Render("· running acceptance gate: " + cmd))
	} else {
		r.write(styleDim.Render("· no automated gate for this slice — will pause for you to verify"))
	}
	return runDriveGate(r.workDir, cur)
}

// runDriveGate executes one task's acceptance gate and reports it as a
// DriveGateMsg. Pure command — no REPL state touched here.
func runDriveGate(workDir string, task tasksmd.Task) tea.Cmd {
	return func() tea.Msg {
		res := drive.RunGate(workDir, task, driveGateTimeout)
		return DriveGateMsg{TaskSubject: task.Subject, Result: res}
	}
}

// handleDriveGateMsg applies a gate result to the ledger and sets up the
// pause-at-each-gate interaction:
//
//   - pass    → mark the slice Done, promote the next, arm continue.
//   - unknown → no automated check; mark Done with a caveat, advance,
//     arm continue (the user verifies during the pause).
//   - fail    → annotate the slice Blocked, leave it In progress, arm
//     continue so the next Enter retries/fixes the SAME slice.
func (r *REPL) handleDriveGateMsg(msg DriveGateMsg) (REPL, tea.Cmd) {
	date := time.Now().Format("2006-01-02")
	res := msg.Result

	// Attribute the files the slice's run changed to this slice, for the
	// completion summary and /drive. Accumulate across retries so a fix
	// round doesn't drop earlier edits.
	if len(r.driveLastChanged) > 0 {
		if r.driveFiles == nil {
			r.driveFiles = map[string][]string{}
		}
		r.driveFiles[msg.TaskSubject] = unionPaths(r.driveFiles[msg.TaskSubject], r.driveLastChanged)
	}
	files := r.driveFiles[msg.TaskSubject]

	switch res.Outcome {
	case drive.GateFail:
		reason := res.Command + " failed"
		if res.Output != "" {
			reason += ": " + firstLine(res.Output)
		}
		tasksmd.BlockCurrent(r.workDir, reason)
		r.write(styleError.Render(fmt.Sprintf("✗ gate failed for %q%s", msg.TaskSubject, filesSuffix(files))))
		if res.Output != "" {
			r.write(styleDim.Render(indentBlock(res.Output)))
		}
		r.write(styleDim.Render("press Enter to keep working this slice until the gate passes, or type to redirect"))
		r.continueArmed = true
		return *r, nil

	case drive.GateUnknown:
		tasksmd.CompleteCurrent(r.workDir, date)
		r.write(styleDim.Render(fmt.Sprintf("• %q done — no automated gate; sanity-check it yourself%s", msg.TaskSubject, filesSuffix(files))))
		return r.driveAdvance()

	default: // GatePass
		// Distinguish a slice-specific gate from the generic project
		// suite. When the acceptance is prose, ResolveGateCommand fell
		// back to `go test ./...` (etc.), so a green run advances the
		// slice but does NOT confirm the acceptance itself — saying "gate
		// passed" there would imply a verification that didn't happen.
		explicit := false
		if t, err := tasksmd.Load(r.workDir); err == nil && len(t.InProgress) > 0 {
			_, explicit = drive.ExtractGateCommand(t.InProgress[0].Acceptance)
		}
		tasksmd.CompleteCurrent(r.workDir, date)
		if explicit {
			r.write(styleStepDone.Render(fmt.Sprintf("✓ gate passed for %q (%s)%s", msg.TaskSubject, res.Command, filesSuffix(files))))
		} else {
			r.write(styleStepDone.Render(fmt.Sprintf("✓ %q advanced — %s is green, but its acceptance isn't machine-checkable; confirm it by hand%s", msg.TaskSubject, res.Command, filesSuffix(files))))
		}
		return r.driveAdvance()
	}
}

// driveAdvance promotes the next pending slice and arms the
// pause-at-each-gate continue, or winds drive mode down when the
// worklist is drained.
func (r *REPL) driveAdvance() (REPL, tea.Cmd) {
	next, ok, _ := tasksmd.StartNext(r.workDir)
	if ok {
		r.write(styleDim.Render(fmt.Sprintf("▸ next slice: %s — press Enter to continue, or type to redirect", next.Subject)))
		r.continueArmed = true
		return *r, nil
	}
	r.driveActive = false
	r.write(r.renderDriveSummary())
	return *r, nil
}

// renderDriveSummary builds the end-of-drive roll-up: every slice with
// its outcome (✓ done / ✗ blocked / ○ pending), the files each changed,
// and a one-line overall verdict. Structure comes from TASKS.md (the
// durable ledger — correct even after a restart); the per-slice file
// lists come from the in-session driveFiles map. Also used by /drive on
// demand.
func (r *REPL) renderDriveSummary() string {
	t, err := tasksmd.Load(r.workDir)
	if err != nil || (len(t.Done)+len(t.InProgress)+len(t.Pending) == 0) {
		return styleDim.Render("drive: no worklist to summarize (TASKS.md is empty).")
	}
	total := len(t.Done) + len(t.InProgress) + len(t.Pending)

	var b strings.Builder
	b.WriteString(styleStepDone.Render(fmt.Sprintf("━━ Drive summary: %d/%d slices done ━━", len(t.Done), total)))

	for _, tk := range t.Done {
		fmt.Fprintf(&b, "\n%s%s", styleStepDone.Render("  ✓ "+tk.Subject), styleDim.Render(filesSuffix(r.driveFiles[tk.Subject])))
	}
	var blocked, inflight int
	for _, tk := range t.InProgress {
		if reason := blockedReason(tk); reason != "" {
			blocked++
			fmt.Fprintf(&b, "\n%s%s\n%s", styleError.Render("  ✗ "+tk.Subject),
				styleDim.Render(filesSuffix(r.driveFiles[tk.Subject])),
				styleDim.Render("      "+reason))
		} else {
			inflight++
			fmt.Fprintf(&b, "\n%s", styleWarn.Render("  ⟳ "+tk.Subject+" (in progress)"))
		}
	}
	for _, tk := range t.Pending {
		fmt.Fprintf(&b, "\n%s", styleDim.Render("  ○ "+tk.Subject+" (not started)"))
	}

	// Overall verdict line.
	uniqueFiles := map[string]bool{}
	for _, fs := range r.driveFiles {
		for _, f := range fs {
			uniqueFiles[f] = true
		}
	}
	parts := []string{fmt.Sprintf("%d succeeded", len(t.Done))}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	if inflight > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", inflight))
	}
	if len(t.Pending) > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", len(t.Pending)))
	}
	verdict := "✓ all slices complete"
	if blocked > 0 || inflight > 0 || len(t.Pending) > 0 {
		verdict = "⚠ not finished"
	}
	fmt.Fprintf(&b, "\n%s", styleDim.Render(fmt.Sprintf("Overall: %s — %s. %d file(s) changed.",
		verdict, strings.Join(parts, ", "), len(uniqueFiles))))
	return b.String()
}

// blockedReason returns the "Blocked: ..." annotation from a task body,
// or "" when the task carries none (it's genuinely in progress, not
// failed).
func blockedReason(t tasksmd.Task) string {
	for _, ln := range strings.Split(t.Body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(strings.ToLower(ln), "blocked:") {
			return ln
		}
	}
	return ""
}

// changedPathsFromResult unions the files every agent run changed into a
// single deduped, order-preserving list.
func changedPathsFromResult(res *orchestrator.Result) []string {
	if res == nil {
		return nil
	}
	var out []string
	for _, run := range res.Runs {
		out = unionPaths(out, run.ChangedPaths)
	}
	return out
}

// unionPaths appends the elements of add not already in base, preserving
// order and de-duplicating.
func unionPaths(base, add []string) []string {
	seen := map[string]bool{}
	for _, p := range base {
		seen[p] = true
	}
	for _, p := range add {
		if !seen[p] {
			seen[p] = true
			base = append(base, p)
		}
	}
	return base
}

// filesSuffix renders a compact " · file, file" tail for a slice's
// changed-file list, collapsing long lists to a count.
func filesSuffix(files []string) string {
	switch n := len(files); {
	case n == 0:
		return ""
	case n <= 3:
		return "  · " + strings.Join(files, ", ")
	default:
		return fmt.Sprintf("  · %d files (incl. %s)", n, files[0])
	}
}

// firstLine returns the first non-empty line of s, for compact Blocked:
// annotations.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			return strings.TrimSpace(ln)
		}
	}
	return ""
}

// indentBlock prefixes each line with two spaces for scrollback display
// of gate output.
func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n")
}
