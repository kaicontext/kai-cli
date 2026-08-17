package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/telemetry"
)

// doCmd is `kai do` — the Jeff passthrough. Same kit-in-kai handoff as
// `kai code` (see code.go), except kit needs the subcommand name back:
// codeCmd forwards bare args because bare `kit` IS the code experience,
// while Jeff lives at `kit do`, so the handoff prepends "do" before the
// user's goal and flags.
var doCmd = &cobra.Command{
	Use:   "do <goal>",
	Short: "Hand a goal to Jeff — an outer agent that specs, dispatches, watches, and verifies kai code runs",
	Long: `Hand a plain-language goal to Jeff, the outer agent that drives the
coding harness for you: it explores the project, writes the fully
fleshed-out spec, dispatches it via the headless harness, watches the
run's output live, verifies the result with its own eyes, and
re-dispatches corrective specs until the goal is met or the cycle
budget runs out.

kai do resolves the managed kit binary (installing it on first use)
and hands off to ` + "`kit do`" + `, forwarding every argument and flag
unchanged. See ` + "`kit do --help`" + ` for Jeff's flags (--cycles, --model).`,
	// Forward everything after `do` to kit verbatim; do not let cobra
	// parse flags meant for the child.
	DisableFlagParsing: true,
	RunE:               runDo,
}

// runDo mirrors runCode (code.go): resolve kit, flush telemetry, hand
// off via syscall.Exec with kit's exact exit code on failure. The only
// difference is the re-prepended "do" subcommand.
func runDo(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return cmd.Help()
	}

	l := codeLauncher()
	l.BeforeExec = telemetry.Close

	code := codeMain(l, cmd.Context(), append([]string{"do"}, args...), os.Stderr)
	telemetry.Close()
	if code != 0 {
		os.Exit(code)
	}
	return nil
}
