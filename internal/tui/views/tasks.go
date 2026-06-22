package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// TaskProgress is the inline checklist that renders in the REPL
// scrollback as the agent works through declared steps. It's a value
// type — Bubble Tea copies models on every Update, so embedding any
// non-zero strings.Builder or sync primitive here would panic. Stick
// to plain fields.
//
// Two ingestion paths feed it:
//
//   - Chat mode: the REPL's stream parser watches for STEPS:/STEP_DONE:
//     markers in the model's output and creates / advances steps via
//     the messages below.
//   - Orchestrator: spawns/finishes/fails fire StepStartedMsg/
//     StepDoneMsg/StepFailedMsg directly. (Phase 2 — not wired yet.)
//
// Either way, the component is dumb: it holds the steps and renders
// them. State transitions land via Update.
type TaskProgress struct {
	Title     string
	Steps     []Step
	StartedAt time.Time
	// TokensIn snapshots the cumulative session input-token count
	// at the moment a step started, so per-step deltas can be
	// computed. Updated by StepStartedMsg.
	stepStartTokensIn int
}

// Step is one row in the checklist.
type Step struct {
	Description string
	Status      StepStatus
	StartedAt   time.Time
	Duration    time.Duration
	TokensUsed  int
	// Reason populates when Status == StepFailed; rendered after
	// the description as "✗ description — reason".
	Reason string
}

// StepStatus enumerates the lifecycle of a single step. Order
// matters: zero value is StepPending so a freshly parsed step list
// starts with everything pending.
type StepStatus int

const (
	StepPending StepStatus = iota
	StepInProgress
	StepDone
	StepFailed
)

// NewTaskProgress builds a fresh progress block with `descriptions`
// as the initial step list, all marked Pending. Title is shown above
// the steps; pass "" for chat-mode (no plan title) and the orchestrator
// passes WorkPlan.Summary for orchestrated runs.
func NewTaskProgress(title string, descriptions []string) TaskProgress {
	steps := make([]Step, len(descriptions))
	for i, d := range descriptions {
		steps[i] = Step{Description: d, Status: StepPending}
	}
	return TaskProgress{
		Title:     title,
		Steps:     steps,
		StartedAt: time.Now(),
	}
}

// Active reports whether at least one step is in progress. The REPL
// uses this to decide whether to keep the component rendered or fold
// it into static scrollback after completion.
func (t TaskProgress) Active() bool {
	for _, s := range t.Steps {
		if s.Status == StepInProgress {
			return true
		}
	}
	return false
}

// AllDone reports whether every step has terminated (Done or Failed).
// True doesn't imply success — pair with HasFailures.
func (t TaskProgress) AllDone() bool {
	if len(t.Steps) == 0 {
		return false
	}
	for _, s := range t.Steps {
		if s.Status == StepPending || s.Status == StepInProgress {
			return false
		}
	}
	return true
}

// HasFailures returns true if any step is StepFailed.
func (t TaskProgress) HasFailures() bool {
	for _, s := range t.Steps {
		if s.Status == StepFailed {
			return true
		}
	}
	return false
}

// StartStep marks step `idx` as in-progress and stamps its start time.
// No-op when the index is out of range; an out-of-band STEP_DONE
// from the model shouldn't crash the TUI.
func (t *TaskProgress) StartStep(idx int, sessionTokensIn int) {
	if idx < 0 || idx >= len(t.Steps) {
		return
	}
	t.Steps[idx].Status = StepInProgress
	t.Steps[idx].StartedAt = time.Now()
	t.stepStartTokensIn = sessionTokensIn
}

// FinishStep marks step `idx` Done with the given duration + token
// delta. The REPL computes tokens by snapshotting session totals at
// step start and step finish; the component just stores the delta.
func (t *TaskProgress) FinishStep(idx int, sessionTokensIn int) {
	if idx < 0 || idx >= len(t.Steps) {
		return
	}
	s := &t.Steps[idx]
	s.Status = StepDone
	if !s.StartedAt.IsZero() {
		s.Duration = time.Since(s.StartedAt)
	}
	delta := sessionTokensIn - t.stepStartTokensIn
	if delta > 0 {
		s.TokensUsed = delta
	}
}

// FailStep marks step `idx` Failed with `reason`. Used by the
// orchestrator when an agent crashes; chat mode doesn't currently
// emit failures (the model just stops emitting STEP_DONE).
func (t *TaskProgress) FailStep(idx int, reason string) {
	if idx < 0 || idx >= len(t.Steps) {
		return
	}
	s := &t.Steps[idx]
	s.Status = StepFailed
	s.Reason = reason
	if !s.StartedAt.IsZero() && s.Duration == 0 {
		s.Duration = time.Since(s.StartedAt)
	}
}

// View renders the checklist as a multi-line string. The REPL
// inlines this into its scrollback above the streaming response.
// Empty steps → empty string so callers can compose unconditionally.
func (t TaskProgress) View() string {
	if len(t.Steps) == 0 {
		return ""
	}
	var b strings.Builder
	if t.Title != "" {
		// Spinner-ish bullet plus title; keep simple — the REPL's
		// existing transient line already animates.
		b.WriteString(styleTaskTitle.Render("* " + t.Title))
		if total := totalTaskMetrics(t); total != "" {
			b.WriteString(" ")
			b.WriteString(styleDim.Render(total))
		}
		b.WriteString("\n")
	}
	for _, s := range t.Steps {
		b.WriteString(renderStep(s))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderStep formats one step. Status determines the glyph, color,
// and whether time/token metrics are shown.
func renderStep(s Step) string {
	glyph, descStyle, metricsVisible := stepGlyphAndStyle(s.Status)
	desc := descStyle.Render(s.Description)
	line := glyph + " " + desc
	if s.Status == StepFailed && s.Reason != "" {
		line += styleDim.Render(" — " + s.Reason)
	}
	if metricsVisible {
		if metrics := formatStepMetrics(s); metrics != "" {
			// Right-pad with two spaces before the dim metrics so
			// the "(45s · ↓ 2.1k)" floats off the description.
			line += "  " + styleDim.Render(metrics)
		}
	}
	return line
}

// stepGlyphAndStyle returns the leading glyph, the description's
// lipgloss style, and whether to render metrics. Pending steps stay
// clean (no metrics, dim text) so the eye skips them.
func stepGlyphAndStyle(st StepStatus) (string, lipgloss.Style, bool) {
	switch st {
	case StepDone:
		return styleStepDone.Render("✓"), styleDim, true
	case StepInProgress:
		return styleStepActive.Render("■"), styleStepActiveDesc, true
	case StepFailed:
		return styleStepFailed.Render("✗"), styleError, true
	default: // Pending
		return styleDim.Render("□"), styleDim, false
	}
}

// formatStepMetrics renders "(elapsed · ↓ tokens)" for in-progress
// and done steps. Falls back to bare elapsed when token delta is 0
// (the runner didn't report token telemetry yet).
func formatStepMetrics(s Step) string {
	var dur time.Duration
	switch s.Status {
	case StepInProgress:
		if !s.StartedAt.IsZero() {
			dur = time.Since(s.StartedAt)
		}
	case StepDone, StepFailed:
		dur = s.Duration
	}
	parts := []string{}
	if dur > 0 {
		parts = append(parts, formatStepDuration(dur))
	}
	if s.TokensUsed > 0 {
		parts = append(parts, "↓ "+formatStepTokens(s.TokensUsed))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

// formatStepDuration produces compact human strings: 12s, 1m 7s,
// 2m, etc. Avoids "0s" by floor-rounding to 1s minimum.
func formatStepDuration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	if secs == 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// formatStepTokens compacts large integers into k/m suffixes, matching
// the REPL's existing humanCount helper but kept local so this file
// stays self-contained for testing.
func formatStepTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// totalTaskMetrics renders a "(elapsed · ↓ total tokens)" trailer
// for the title row. Sums per-step tokens and uses StartedAt for
// elapsed.
func totalTaskMetrics(t TaskProgress) string {
	if t.StartedAt.IsZero() {
		return ""
	}
	dur := time.Since(t.StartedAt)
	totalTokens := 0
	for _, s := range t.Steps {
		totalTokens += s.TokensUsed
	}
	if dur < time.Second && totalTokens == 0 {
		return ""
	}
	parts := []string{formatStepDuration(dur)}
	if totalTokens > 0 {
		parts = append(parts, "↓ "+formatStepTokens(totalTokens)+" tokens")
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

// --- styles ---------------------------------------------------------

var (
	styleTaskTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	// Done: muted green so completed rows fade into the background.
	styleStepDone = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	// In-progress: amber, bold to draw the eye to the active row.
	styleStepActive     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleStepActiveDesc = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleStepFailed     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
)
