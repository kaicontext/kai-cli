package views

import "testing"

// TestStripANSI covers the /copy clipboard path: lipgloss-rendered
// strings must come out as plain text when pasted into Slack /
// GitHub / email. Multi-style content (color + bold + dim + reset)
// is the realistic input shape.
func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		// Plain text passes through unchanged.
		"hello world": "hello world",

		// Single SGR (red).
		"\x1b[31merror\x1b[0m": "error",

		// Composed SGR (256-color + bold + reset).
		"\x1b[1;38;5;203m✗ Something unexpected\x1b[0m": "✗ Something unexpected",

		// Real /copy payload shape from the user's report.
		"\x1b[38;5;242m  details: planner: agent run\x1b[0m": "  details: planner: agent run",

		// Multi-line preserved; only escapes stripped.
		"\x1b[31mline1\x1b[0m\n\x1b[32mline2\x1b[0m": "line1\nline2",

		// Idempotent: stripping a clean string is a no-op.
		"already clean": "already clean",
	}
	for in, want := range cases {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}
