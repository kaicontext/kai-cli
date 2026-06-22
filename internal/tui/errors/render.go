package errors

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Render converts a UserError into the multi-line styled string
// the REPL writes into scrollback. Severity controls the color;
// Headline is bold; Detail and Action are dim follow-up lines.
//
// The renderer NEVER touches LogContext or Context for KNOWN
// kinds — those are for the local log + telemetry only. That
// separation is the whole point of the classifier: raw error
// text never reaches the user when we have a friendly form.
//
// EXCEPTION: for the "internal.unknown" fallback (no rule
// matched), we DO surface a one-line excerpt of the raw
// error. Without it the user sees nothing actionable at all
// — just "Something unexpected happened" — and has no clue
// whether it was a network hiccup, a parser error, or the
// disk filling up. The May-2026 user pain: "That doesn't
// say anything specific. We need the real error." Showing
// the raw form ONLY for unknown fallbacks preserves the
// classifier's contract for known cases (no jargon leakage)
// while giving us debuggability when the rule set is
// incomplete. Capped to 200 chars and first line only so
// even a verbose stack-trace doesn't drown the scrollback.
func Render(ue UserError) string {
	if ue.Kind == "" || ue.Kind == "none" {
		return ""
	}
	var b strings.Builder
	b.WriteString(headlineStyle(ue.Severity).Render(headlineGlyph(ue.Severity) + " " + ue.Headline))
	if d := strings.TrimSpace(ue.Detail); d != "" {
		b.WriteByte('\n')
		b.WriteString(dimStyle.Render("  " + d))
	}
	if a := strings.TrimSpace(ue.Action); a != "" {
		b.WriteByte('\n')
		b.WriteString(dimStyle.Render("  " + a))
	}
	if ue.Kind == "internal.unknown" {
		if raw := excerpt(ue.LogContext, 200); raw != "" {
			b.WriteByte('\n')
			b.WriteString(dimStyle.Render("  details: " + raw))
		}
	}
	return b.String()
}

// excerpt returns a single-line preview of s, capped at n
// runes. Strips leading/trailing whitespace, collapses any
// internal newlines into " ¶ " so the preview stays one
// line without losing the structural hint that there were
// multiple.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ¶ ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return string(r)
}

var (
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	infoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	blockStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
)

func headlineStyle(s Severity) lipgloss.Style {
	switch s {
	case Warn:
		return warnStyle
	case Block:
		return blockStyle
	default:
		return infoStyle
	}
}

// headlineGlyph picks a single-character prefix that signals
// severity at a glance. Mirrors the conventions used elsewhere
// in the TUI (✓ for success, ⚠ for warn, ✗ for block).
func headlineGlyph(s Severity) string {
	switch s {
	case Warn:
		return "⚠"
	case Block:
		return "✗"
	default:
		return "·"
	}
}
