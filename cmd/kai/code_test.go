package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCodeHelpReproducesKitRoot verifies that `kai code -h` reproduces the
// retired `kit -h`: the full grouped menu of every kai subcommand, rendered
// under the `kai code` path — not merely code's own flags. kit's bare binary
// launched the TUI, so `kit -h` was the root help; `kai code` is its drop-in.
func TestCodeHelpReproducesKitRoot(t *testing.T) {
	got := renderCodeHelp(t)

	// Usage advertises the code path in both runnable and subcommand forms.
	for _, want := range []string{"Usage:", "kai code [flags]", "kai code [command]"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q\n---\n%s", want, got)
		}
	}

	// The full grouped menu is present — group headings AND representative
	// subcommands that live on the root, not under code.
	for _, want := range []string{
		"Getting Started:", "Diff & Review:", "Advanced:", "Additional Commands:",
		"snapshot", "diff", "gate", "stats",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing menu entry %q\n---\n%s", want, got)
		}
	}

	// code's own flags stay visible (no regression vs the old subcommand help,
	// which is strictly better than kit, where they hid behind `kit code -h`).
	for _, want := range []string{"-p, --prompt", "--auto", "--session"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing code flag %q\n---\n%s", want, got)
		}
	}

	// The listed subcommands resolve under `kai`, so the footer points there
	// (not the unusable `kai code <command>`).
	if !strings.Contains(got, `Use "kai [command] --help"`) {
		t.Errorf("help missing root-path footer\n---\n%s", got)
	}
}

// TestCodeHelpMenuMatchesRoot locks the grouped command listing to cobra's own
// rendering of the root command, so cobra template or alignment drift can't
// silently desync `kai code -h` from `kai -h`.
func TestCodeHelpMenuMatchesRoot(t *testing.T) {
	code := menuSection(renderCodeHelp(t))

	var rootBuf bytes.Buffer
	rootCmd.SetOut(&rootBuf)
	t.Cleanup(func() { rootCmd.SetOut(nil) })
	_ = rootCmd.Help()
	root := menuSection(rootBuf.String())

	if root == "" {
		t.Fatal("root help produced no menu section")
	}
	if code != root {
		t.Errorf("code menu != root menu (alignment/content drift)\n--- root ---\n%s\n--- code ---\n%s", root, code)
	}
}

// renderCodeHelp renders printCodeHelp into a string, the same output every
// `kai code` help path produces (it is codeCmd's HelpFunc).
func renderCodeHelp(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	codeCmd.SetOut(&buf)
	t.Cleanup(func() { codeCmd.SetOut(nil) })
	printCodeHelp(codeCmd, nil)
	return buf.String()
}

// menuSection returns the grouped command listing: the lines from the first
// group heading up to (but excluding) the Flags: section.
func menuSection(help string) string {
	if len(rootCmd.Groups()) == 0 {
		return ""
	}
	firstGroup := rootCmd.Groups()[0].Title
	lines := strings.Split(help, "\n")
	start := -1
	for i, ln := range lines {
		if ln == firstGroup {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if lines[i] == "Flags:" {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}
