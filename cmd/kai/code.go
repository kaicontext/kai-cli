package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"kai/internal/kitlauncher"
	"kai/internal/telemetry"
)

// codeLauncher builds the launcher used by `kai code`. A package var so
// tests can substitute a launcher with injected seams (fake download
// server, exec spy) without touching the network or replacing the process.
var codeLauncher = kitlauncher.Default

// runCode is codeCmd.RunE — the `kai code` passthrough. It resolves (and
// self-installs on first use) the managed kit binary and hands off to it,
// forwarding every trailing arg/flag verbatim. codeCmd sets
// DisableFlagParsing, so `args` here is os.Args after `code`, untouched by
// cobra — except a leading -h/--help, which we answer with kai's own help
// (as every other subcommand does) rather than forwarding it to kit.
//
// Exit-code handling lives here (not in codeMain) so it can call os.Exit
// with kit's exact code; codeMain stays os.Exit-free and testable.
func runCode(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return cmd.Help()
	}

	l := codeLauncher()
	// The handoff replaces this process via syscall.Exec, so cobra's
	// PersistentPostRun — which flushes telemetry — never runs. Flush right
	// before the handoff (BeforeExec) and again on the failure path below,
	// which exits via os.Exit. telemetry.Close is idempotent.
	l.BeforeExec = telemetry.Close

	code := codeMain(l, cmd.Context(), args, os.Stderr)
	telemetry.Close()
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// codeMain runs the launcher and renders any failure as a clear,
// actionable message to w. It returns the process exit code: 0 on success
// (or a successful syscall.Exec handoff, which never actually returns
// here), non-zero on failure. Split out from runCode so tests can assert
// the exit code and the user-facing message together without os.Exit
// terminating the test binary.
func codeMain(l *kitlauncher.Launcher, ctx context.Context, args []string, w io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	code, err := l.Run(ctx, args)
	if err != nil {
		fmt.Fprintf(w, "\nCouldn't launch the code experience: %v\n", err)
		fmt.Fprintf(w, "Try `kai code` again, or see https://docs.kaicontext.com for help.\n")
		if code == 0 {
			code = 1
		}
		return code
	}
	return code
}
