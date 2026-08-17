package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"kai/internal/kitlauncher"
)

// TestDoCommand_ForwardsWithDoSubcommand drives the real cobra dispatch
// with `do make the careers page --cycles 2` and asserts kit receives
// the "do" subcommand back plus every trailing arg verbatim. Unlike
// `code` (bare kit IS the code experience), Jeff lives at `kit do`, so
// the passthrough must re-prepend the subcommand — this test pins that
// difference.
func TestDoCommand_ForwardsWithDoSubcommand(t *testing.T) {
	binDir := t.TempDir()
	kitPath := fakeManagedKit(t, binDir)

	var gotArgv []string
	orig := codeLauncher
	codeLauncher = func() *kitlauncher.Launcher {
		l := kitlauncher.Default()
		l.BinDir = binDir
		l.LookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
		l.Exec = func(argv0 string, argv []string, env []string) error {
			gotArgv = argv
			return nil // pretend the handoff succeeded
		}
		l.Stderr = &bytes.Buffer{}
		return l
	}
	t.Cleanup(func() { codeLauncher = orig })

	rootCmd.SetArgs([]string{"do", "make", "the", "careers", "page", "--cycles", "2"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("`kai do` execute failed (cobra likely parsed a kit flag): %v", err)
	}

	want := []string{kitPath, "do", "make", "the", "careers", "page", "--cycles", "2"}
	if fmt.Sprint(gotArgv) != fmt.Sprint(want) {
		t.Errorf("forwarded argv = %v, want %v", gotArgv, want)
	}
}
