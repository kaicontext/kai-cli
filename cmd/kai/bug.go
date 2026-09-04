// `kai bug` — help users report issues. Prints a pre-filled bug report
// body (version, OS, architecture) and opens the GitHub issue tracker.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var bugNoBrowser bool

var bugCmd = &cobra.Command{
	Use:   "bug",
	Short: "Report a bug and open the issue tracker",
	Long: `Prints a pre-filled bug report body (version, OS, architecture) to stdout
and opens the GitHub issue tracker in your default browser.

Copy the printed report into the issue body, then fill in the description,
reproduction steps, and expected vs actual behavior.`,
	RunE: runBug,
}

func init() {
	bugCmd.Flags().BoolVar(&bugNoBrowser, "no-browser", false, "Print the report body without opening a browser")
	rootCmd.AddCommand(bugCmd)
}

func runBug(cmd *cobra.Command, args []string) error {
	const issuesURL = "https://github.com/kaicontext/kai-cli/issues/new/choose"

	// Build the pre-filled report body.
	var sb strings.Builder
	sb.WriteString("## Bug Report\n\n")
	sb.WriteString("### Description\n\n<!-- What happened? What did you expect? -->\n\n")
	sb.WriteString("### Steps to Reproduce\n\n<!-- Minimal steps to reproduce -->\n\n")
	sb.WriteString("### Environment\n\n")
	fmt.Fprintf(&sb, "- kai version: %s\n", Version)
	if GitSHA != "" {
		fmt.Fprintf(&sb, "- git SHA: %s\n", GitSHA)
	}
	fmt.Fprintf(&sb, "- OS: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	sb.WriteString("\n<!-- Run `kai doctor` and paste relevant output if applicable -->\n")

	fmt.Print(sb.String())

	if !bugNoBrowser {
		fmt.Fprintf(os.Stderr, "\nOpening %s in your browser...\n", issuesURL)
		openBrowser(issuesURL)
	} else {
		fmt.Fprintf(os.Stderr, "\nIssue tracker: %s\n", issuesURL)
	}

	return nil
}
