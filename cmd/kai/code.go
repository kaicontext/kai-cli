package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// runCode is codeCmd.RunE. It runs the coding experience in-process via
// runCodeTUI — the Bubble Tea TUI and headless -p path. codeCmd sets
// DisableFlagParsing so `args` is os.Args after `code`, untouched by cobra.
// A leading -h/--help is answered with kai's own help. Otherwise the code
// flags are parsed here before handing off to runCodeTUI.
func runCode(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return cmd.Help()
	}
	fs := cmd.Flags()
	fs.ParseErrorsWhitelist.UnknownFlags = true
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runCodeTUI(cmd, fs.Args())
}

// codeHelpHeader is the one-paragraph lede of `kai code`'s help, the kai-native
// parallel of the retired `kit -h` header. kit's bare binary launched the TUI,
// so `kit -h` was the root help that listed every subcommand; `kai code` is
// that binary's drop-in, so its help opens with the same kind of overview.
const codeHelpHeader = "Launch the interactive kai coding experience (in-process TUI + agent) — " +
	"REPL, live-sync, and safety-gate panes. All kai subcommands (snapshot, diff, " +
	"stats, gate, etc.) are listed below and run as 'kai <command>'."

// printCodeHelp renders `kai code`'s help as a faithful reproduction of the
// retired `kit -h`: the header above followed by the FULL grouped listing of
// every kai subcommand (the same menu kit showed at its root), then code's own
// flags. It is wired as codeCmd's HelpFunc (see init in tui.go) so every help
// path is identical — `kai code -h`, `kai help code`, and the non-TTY fallback
// in runCodeTUI all land here.
//
// The grouped listing is generated from the ROOT command's groups and
// subcommands, mirroring cobra's defaultUsageFunc, so it stays byte-aligned
// with `kai -h` (NamePadding resolves to root's column width for every child).
// Those subcommands live under `kai`, not `kai code`; they are surfaced here
// for the kit-style overview and invoked as `kai <command>` (see the footer).
func printCodeHelp(cmd *cobra.Command, _ []string) {
	root := cmd.Root()
	out := cmd.OutOrStdout()

	fmt.Fprintln(out, codeHelpHeader)
	fmt.Fprintln(out)

	// Usage — code is runnable AND fronts the full subcommand menu, so show
	// both the [flags] (launch the TUI) and [command] forms, exactly as kit's
	// runnable root did.
	path := cmd.CommandPath() // "kai code"
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintf(out, "  %s [flags]\n", path)
	fmt.Fprintf(out, "  %s [command]\n", path)

	// Grouped command listing, taken from the ROOT command so it matches
	// `kai -h` line-for-line. rpad + NamePadding reproduce cobra's alignment.
	cmds := root.Commands()
	for _, group := range root.Groups() {
		fmt.Fprintf(out, "\n%s\n", group.Title)
		for _, sub := range cmds {
			if sub.GroupID == group.ID && (sub.IsAvailableCommand() || sub.Name() == "help") {
				fmt.Fprintf(out, "  %s %s\n", rpad(sub.Name(), sub.NamePadding()), sub.Short)
			}
		}
	}
	if !root.AllChildCommandsHaveGroup() {
		fmt.Fprintf(out, "\nAdditional Commands:\n")
		for _, sub := range cmds {
			if sub.GroupID == "" && (sub.IsAvailableCommand() || sub.Name() == "help") {
				fmt.Fprintf(out, "  %s %s\n", rpad(sub.Name(), sub.NamePadding()), sub.Short)
			}
		}
	}

	// code's own flags (-p/--auto/--session/...) plus inherited globals. kit -h
	// only showed root flags; keeping code's flags here is strictly more useful
	// and loses no parity (kit hid them behind a separate `kit code -h`, which
	// has no analog now that code is the launcher).
	if lf := cmd.LocalFlags(); lf.HasAvailableFlags() {
		fmt.Fprintf(out, "\nFlags:\n%s", lf.FlagUsages())
	}
	if gf := cmd.InheritedFlags(); gf.HasAvailableFlags() {
		fmt.Fprintf(out, "\nGlobal Flags:\n%s", gf.FlagUsages())
	}

	// The listed subcommands resolve under `kai`, so point the reader at the
	// real path (`kai <command> --help`) rather than the unusable
	// `kai code <command>`.
	fmt.Fprintf(out, "\nUse \"%s [command] --help\" for more information about a command.\n", root.CommandPath())
}

// rpad right-pads s with spaces to the given width, matching cobra's internal
// rpad so the grouped listing aligns identically to `kai -h`.
func rpad(s string, padding int) string {
	return fmt.Sprintf("%-*s", padding, s)
}
