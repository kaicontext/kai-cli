package views

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"kai/api/provider"
)

// renderBanner builds the startup splash that appears once at the top
// of the REPL scrollback. Mirrors the Claude Code banner shape: a
// small mascot on the left, identity + connection details on the
// right. Designed to fit a single visual block (~7 rows) so it
// doesn't dominate the viewport on launch.
func renderBanner(s *PlannerServices) string {
	mascot := mascotArt()
	mascotLines := strings.Split(mascot, "\n")

	// Add a blank row above the icon for vertical breathing room.
	mascotLines = append([]string{strings.Repeat(" ", visibleWidth(mascotLines[0]))}, mascotLines...)

	version := "dev"
	if s != nil && s.Version != "" {
		version = s.Version
	}
	// infoLine is the provider/model line. kai routes each kind of work
	// to a different model (planner / chat / executor), so a single
	// "model" name is only meaningful for BYOM single-model providers.
	// For the kailab proxy (multi-model) we show the provider alone —
	// naming one model there misrepresents what's actually running.
	infoLine := "offline — run `kai auth login`"
	if s != nil && s.OrchestratorCfg.AgentProvider != nil {
		providerLabel := providerBannerLabel()
		kind := normalizeKindForBanner(os.Getenv("KAI_PROVIDER"))
		if kind != "" && kind != string(provider.KindKailab) {
			// BYOM: a single override model is accurate — show it.
			model := s.OrchestratorCfg.AgentModel
			if model == "" {
				model = s.PlannerCfg.Model
			}
			if model != "" {
				infoLine = providerLabel + " → " + shortModel(model)
			} else {
				infoLine = providerLabel
			}
		} else {
			// kailab proxy: multi-model per role — provider only.
			infoLine = providerLabel
		}
	}
	workspace := compactPath(workspaceFor(s))

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // amber, matches mascot
	dim := styleDim
	bullet := dim.Render("›")

	right := []string{
		titleStyle.Render("kai") + dim.Render(" v"+version),
		bullet + " " + dim.Render(infoLine),
		bullet + " " + dim.Render(workspace),
		dim.Render("Enter sends · Shift/Alt+Enter for newline · ↑/↓ history"),
		dim.Render("/command for kai subcommands · Ctrl+C twice to exit"),
		"", // blank row below for breathing room
	}

	// Pad the shorter column up to the longer one so JoinHorizontal
	// produces a clean rectangle (no ragged trailing rows).
	rows := max(len(mascotLines), len(right))
	for len(mascotLines) < rows {
		mascotLines = append(mascotLines, strings.Repeat(" ", visibleWidth(mascotLines[0])))
	}
	for len(right) < rows {
		right = append(right, "")
	}

	left := lipgloss.NewStyle().PaddingLeft(2).Render(strings.Join(mascotLines, "\n"))
	rightCol := strings.Join(right, "\n")

	return lipgloss.JoinHorizontal(lipgloss.Top,
		left,
		"  ",
		rightCol,
	)
}

// shortModel strips a Together/HF-style org prefix for display:
// "z-ai/glm-5.1" → "GLM-5.1", "gpt-4o" → "gpt-4o".
func shortModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// providerBannerLabel returns a short human-friendly tag for
// the active LLM api. Reads KAI_PROVIDER directly (cheap,
// no need to plumb the resolved Config through PlannerServices)
// so toggling the env between launches is reflected in the
// banner without code changes elsewhere.
//
// Labels:
//
//	kailab                                → kailab (default)
//	anthropic / anthropic-direct / claude → anthropic (direct)
//	openai (no base URL or api.openai.com) → openai (direct)
//	openai-compat aliases + custom URL    → openai-compatible @ <host>
//
// The "openai (direct)" vs "openai-compatible @ host" split is
// the important distinction: it tells the user at a glance
// whether they're talking to actual OpenAI, or to LM Studio /
// Ollama / Together / Groq / etc. that happens to speak the
// same protocol. Without this split, "openai → claude-..." can
// be misleading when the request is actually going to a Llama
// model on localhost.
func providerBannerLabel() string {
	kind := normalizeKindForBanner(os.Getenv("KAI_PROVIDER"))
	switch kind {
	case "", string(provider.KindKailab):
		return "kailab"
	case string(provider.KindAnthropic):
		return "anthropic (direct)"
	case string(provider.KindOpenAI):
		base := strings.TrimSpace(os.Getenv("KAI_OPENAI_BASE_URL"))
		if base == "" || strings.Contains(base, "api.openai.com") {
			return "openai (direct)"
		}
		// Compatible-but-not-OpenAI endpoint. Show the host
		// so the user sees "openai-compatible @ localhost:1234"
		// instead of "openai" which would imply api.openai.com.
		host := base
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		return "openai-compatible @ " + host
	case string(provider.KindOpenRouter):
		return "openrouter (direct)"
	default:
		return kind
	}
}

// normalizeKindForBanner mirrors api.normalizeKind so the
// banner accepts the same aliases the factory does. Inlined
// (not importing the unexported factory helper) so the views
// package doesn't need to grow another export surface.
func normalizeKindForBanner(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return ""
	case "openai", "openai-compat", "openai-compatible", "oai", "oai-compat", "local":
		return string(provider.KindOpenAI)
	case "anthropic", "anthropic-direct", "claude":
		return string(provider.KindAnthropic)
	case "openrouter", "openrouter-direct":
		return string(provider.KindOpenRouter)
	case "kailab":
		return string(provider.KindKailab)
	}
	return s
}

// mascotArt returns the kai logo: a stylized 7-row block-character
// glyph rendered in orange. Single foreground color (no per-cell
// gradient like the original full pixel-art) so it composes
// cleanly with the right-column info panel and stays legible on
// terminals that don't render block characters perfectly. Spaces
// keep the default background so the shape reads as a silhouette
// against whatever theme the user runs.
func mascotArt() string {
	orange := lipgloss.Color("202")
	white := lipgloss.Color("231")
	fgO := lipgloss.NewStyle().Foreground(orange)
	fgW := lipgloss.NewStyle().Foreground(white)
	fgOonW := lipgloss.NewStyle().Foreground(orange).Background(white)
	fgWonO := lipgloss.NewStyle().Foreground(white).Background(orange)

	// Diamond outline in orange with a white triangle peak inside,
	// mirroring the kai favicon. Each text row encodes two pixel
	// rows via half-block characters; cells where the orange
	// diamond meets the white triangle use fg+bg to render two
	// colors in one cell.
	lines := []string{
		"  " + fgO.Render("▄██▄") + "  ",
		fgO.Render("▄█") + fgOonW.Render("▀") + fgW.Render("██") + fgOonW.Render("▀") + fgO.Render("█▄"),
		fgO.Render("█") + fgWonO.Render("▀▀▀▀▀▀") + fgO.Render("█"),
		"  " + fgO.Render("▀██▀") + "  ",
	}
	return strings.Join(lines, "\n")
}

// workspaceFor returns the workspace root for the banner. Falls
// back to the process cwd when services aren't configured.
//
// Priority order:
//
//  1. Projects.InvokedFrom — the directory the user actually ran
//     `kai code` from. In a multi-root workspace where Discover
//     walked up to a parent yaml, this is what the user expects to
//     see in the banner ("I'm in ~/projects/kai, show that"), not
//     the primary sub-project's path. Recorded by Discover before
//     the walk-up so it stays accurate even when DiscoveryRoot has
//     been rewritten.
//
//  2. s.MainRepo — legacy fallback. For single-root setups (and any
//     caller that builds PlannerServices without Projects), this
//     equals cwd or the project root. Same display behavior as
//     before this change for the non-multi-root case.
//
//  3. os.Getwd() — last resort when neither is set (tests, stripped
//     services).
//
// Surfaced in the 2026-05-11 dogfood loop: user ran `kai code` from
// ~/projects/kai (multi-root parent), banner showed
// ~/projects/kai/kai (primary.Path = "Kai" project's path) instead
// of the invocation directory. Three rounds of kai-code planning
// chased this exact bug.
func workspaceFor(s *PlannerServices) string {
	if s != nil {
		if s.Projects != nil && s.Projects.InvokedFrom != "" {
			return s.Projects.InvokedFrom
		}
		if s.MainRepo != "" {
			return s.MainRepo
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "(unknown)"
}

// compactPath replaces $HOME with "~" for terser display in the
// banner. Path stays absolute when not under home.
func compactPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// visibleWidth approximates the rendered column width of a styled
// string. Strips ANSI SGR sequences and counts runes — good enough
// for our right-padding needs (mascot lines never contain CJK).
func visibleWidth(s string) int {
	stripped := stripSGR(s)
	n := 0
	for range stripped {
		n++
	}
	return n
}

func stripSGR(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !in && ch == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			in = true
			i++ // skip '['
			continue
		}
		if in {
			if ch == 'm' {
				in = false
			}
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// helper: avoid shadowing built-in min from older Go versions.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// formatBannerError renders a single dim line for the no-services
// case so the user still sees identity at startup.
func formatBannerError() string {
	return styleDim.Render("kai TUI · /command for kai subcommands · ↑/↓ history · Ctrl+C ×2 to exit")
}
