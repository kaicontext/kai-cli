package views

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
)

// formatDiffEvent renders a ChatActivityEvent of kind "diff" as the
// inline block the user sees in the REPL: a header line summarizing
// what changed, followed by additions/removals colored green/red and
// context lines dimmed. Mirrors the Claude Code "Update(path)" style.
//
// width is the pane's wrap width; each diff line gets padded out to
// it so the colored backgrounds extend to the right edge — same as a
// real terminal diff viewer. Lines longer than width are truncated
// with an ellipsis (we do NOT word-wrap diff lines: a wrap inside a
// `+const PORT = ...` would lose the leading + and confuse the user
// about which line is the addition).
//
// We intentionally don't render the unified-diff hunk markers
// (`@@ -a,b +c,d @@`) — they're noise for an inline activity feed.
// The "Soon" spec calls for a proper scrollable diff view; until then
// the patch text is rendered as-is, line by line.
func formatDiffEvent(ev ChatActivityEvent, width int) string {
	if width <= 0 {
		width = 80
	}
	verb := "Update"
	if ev.Op == "created" {
		verb = "Create"
	}
	var b strings.Builder

	// Header: "● Update(path/to/file.go)"
	header := lipgloss.NewStyle().Bold(true).Render(
		fmt.Sprintf("● %s(%s)", verb, ev.Path))
	b.WriteString(header)
	b.WriteByte('\n')

	// Sub-header: "  └ Added N lines, removed M lines"
	stats := summarizeAddedRemoved(ev.Added, ev.Removed)
	b.WriteString(styleDim.Render("  └ " + stats))
	b.WriteByte('\n')

	// Determine the line-number column width from the largest
	// number that will appear, so all numbers right-align in a
	// fixed gutter. Default to 4 when we can't tell.
	gutterWidth := computeGutterWidth(ev.Diff)
	gutter := gutterWidth + 2 // number + " " + marker
	bodyMax := width - gutter

	// Pick a chroma lexer for the file's language so we can
	// syntax-highlight the diff body. nil → falls back to the
	// existing flat-color render. Detection is by extension; for
	// extensionless files (Dockerfile, Makefile) chroma's
	// fallback to "fallback" lexer produces sensible plain text.
	lexer := pickLexer(ev.Path)

	// Body: render each diff line. Skip the "--- a/" / "+++ b/"
	// preamble since the header already tells the user the path.
	// "@@" lines mark a hunk break — render as a dim ellipsis
	// strip so the user sees that some unchanged lines were
	// elided between hunks.
	for _, line := range strings.Split(ev.Diff, "\n") {
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			continue
		}
		if line == "" {
			continue
		}
		if line == "@@" {
			b.WriteString(styleDim.Render(strings.Repeat("·", min(width, 40))))
			b.WriteByte('\n')
			continue
		}
		// Split off the leading "<num>\x1f" produced by
		// unifiedDiff. Fall back to no-gutter rendering on
		// malformed input rather than dropping the line.
		num, rest, ok := strings.Cut(line, "\x1f")
		if !ok {
			rest = line
			num = ""
		}
		marker := byte(' ')
		body := rest
		if len(rest) > 0 {
			marker = rest[0]
			body = rest[1:]
		}
		body = truncateRunes(body, bodyMax-1)
		// No padding to terminal width — paint only the actual
		// content. Padding spaces with a colored background make
		// the diff look "boxed" but produce visual artifacts at
		// line boundaries (the background bleeds into the next
		// line in some terminal/lipgloss combos). Tighter render
		// matches `git diff`'s visual.

		// Gutter: right-aligned line number. Empty for the rare
		// case that splitting failed.
		numField := num
		if numField == "" {
			numField = strings.Repeat(" ", gutterWidth)
		} else if len(numField) < gutterWidth {
			numField = strings.Repeat(" ", gutterWidth-len(numField)) + numField
		}

		gut := styleDiffGutter.Render(numField + " ")

		// Marker (+/-/space) keeps the diff color so the eye can
		// scan additions vs removals at a glance. Body gets
		// syntax-highlighted via chroma when a lexer is available;
		// falls back to the diff color when not. Foreground only
		// — no backgrounds (those caused per-line artifacts).
		var markerStr, coloredBody string
		switch marker {
		case '+':
			markerStr = styleDiffAdd.Render("+")
		case '-':
			markerStr = styleDiffDel.Render("-")
		default:
			markerStr = styleDiffCtx.Render(" ")
		}
		if lexer != nil {
			coloredBody = highlightLine(lexer, body)
		} else {
			// No lexer → preserve old flat-color rendering for
			// the body. Keeps unsupported extensions readable.
			switch marker {
			case '+':
				coloredBody = styleDiffAdd.Render(body)
			case '-':
				coloredBody = styleDiffDel.Render(body)
			default:
				coloredBody = styleDiffCtx.Render(body)
			}
		}
		b.WriteString(gut + markerStr + coloredBody)
		b.WriteByte('\n')
	}
	// Trim the trailing newline so write() doesn't double-space the
	// scrollback's separator logic.
	return strings.TrimRight(b.String(), "\n")
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if runeCount(s) <= n {
		return s
	}
	out := make([]rune, 0, n)
	for _, r := range s {
		if len(out) >= n-1 {
			break
		}
		out = append(out, r)
	}
	return string(out) + "…"
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func summarizeAddedRemoved(added, removed int) string {
	switch {
	case added > 0 && removed > 0:
		return fmt.Sprintf("Added %d line%s, removed %d line%s",
			added, plural(added), removed, plural(removed))
	case added > 0:
		return fmt.Sprintf("Added %d line%s", added, plural(added))
	case removed > 0:
		return fmt.Sprintf("Removed %d line%s", removed, plural(removed))
	default:
		return "No line changes"
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Diff line styles. Strong saturated backgrounds so additions and
// removals are unmistakable at a glance; context lines stay neutral.
// formatGateVerdict renders a one-line, color-coded summary of the
// safety gate's classification of a freshly-mutated set of paths.
// Auto verdicts are green (no action needed), Review verdicts are
// amber (kai pane will hold the change for the user to inspect),
// Block verdicts are red (touched a protected path or exceeded the
// block threshold). Mirrors the look of the existing tool-call
// breadcrumbs so the verdict reads as a continuation of the
// preceding edit, not a separate event.
func formatGateVerdict(ev ChatActivityEvent) string {
	var glyph, label string
	var styled lipgloss.Style
	switch ev.GateVerdict {
	case "auto":
		glyph, label = "✓", "auto"
		styled = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	case "review":
		glyph, label = "⚠", "held"
		styled = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // yellow
	case "block":
		glyph, label = "✗", "blocked"
		styled = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // red
	default:
		glyph, label = "·", ev.GateVerdict
		styled = styleDim
	}
	suffix := fmt.Sprintf("%d downstream", ev.GateRadius)
	if len(ev.GateReasons) > 0 {
		suffix = ev.GateReasons[0]
	}
	pathLabel := strings.Join(ev.GatePaths, ", ")
	if pathLabel == "" {
		pathLabel = "(no paths)"
	}
	return "  " + styled.Render(fmt.Sprintf("%s %s — %s", glyph, label, suffix)) +
		styleDim.Render("  "+pathLabel)
}

// Diff line styles. Foreground-only (no backgrounds) — backgrounds
// looked nice in isolation but produced visual artifacts at line
// boundaries (per-row gaps, color bleed across newlines depending
// on terminal). Foreground-only matches `git diff` and reads
// cleanly across iTerm, kitty, Apple Terminal, tmux.
var (
	styleDiffAdd = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")) // bright green
	styleDiffDel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")) // bright red
	styleDiffCtx = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")) // dim gray
	styleDiffGutter = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")) // line numbers — same dim
)

// pickLexer returns a chroma lexer for the file at path, or nil
// when no extension match is found. Detection is purely by file
// extension — content-based detection would mean reading the file,
// and we don't want syntax-highlighting to grow into "scan source
// for shebangs and module declarations" territory.
//
// Returns chroma's coalesced lexer for compactness (merges adjacent
// same-type tokens) and best-results syntax highlighting.
func pickLexer(path string) chroma.Lexer {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return nil
	}
	lex := lexers.Match(filepath.Base(path))
	if lex == nil {
		return nil
	}
	return chroma.Coalesce(lex)
}

// highlightLine returns the line with chroma syntax tokens applied
// as ANSI escape codes. Falls back to the input string on tokenize
// failure (rare) so a single bad line never breaks the diff render.
//
// Uses the "monokai" style — a darker palette that reads cleanly
// over a black/dark terminal and matches the green/red diff
// markers without color clashes. We pick "terminal16m" (truecolor)
// formatter; modern terminals all support 24-bit color.
func highlightLine(lex chroma.Lexer, line string) string {
	if line == "" {
		return ""
	}
	iter, err := lex.Tokenise(nil, line)
	if err != nil {
		return line
	}
	style := styles.Get("monokai")
	if style == nil {
		style = styles.Fallback
	}
	formatter := formatters.Get("terminal16m")
	if formatter == nil {
		formatter = formatters.Fallback
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iter); err != nil {
		return line
	}
	// Trim trailing newline if the formatter added one — we add
	// our own at the end of the line in the diff body loop.
	return strings.TrimRight(buf.String(), "\n")
}

// computeGutterWidth scans the patch for the largest line number it
// will emit so we can right-align all gutters to a consistent width.
// Falls back to 4 (room for "9999") on malformed or empty input.
func computeGutterWidth(patch string) int {
	maxLen := 0
	for _, line := range strings.Split(patch, "\n") {
		if i := strings.IndexByte(line, '\x1f'); i > 0 {
			if i > maxLen {
				maxLen = i
			}
		}
	}
	if maxLen < 4 {
		maxLen = 4
	}
	return maxLen
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
