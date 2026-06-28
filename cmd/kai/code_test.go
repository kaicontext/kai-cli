package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/kitlauncher"
)

// fakeManagedKit writes an executable stub at <dir>/kit so resolveKitPath
// succeeds without a download, and returns its path.
func fakeManagedKit(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "kit")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCodeCommand_ForwardsFlagsThroughCobra drives the REAL cobra dispatch
// (`rootCmd.Execute`) with `code --kitflag x -- pos` and asserts the args
// reach kit verbatim. If DisableFlagParsing weren't set, cobra would reject
// `--kitflag` as an unknown flag and Execute would error — so this test
// proves both that `kai` does not eat kit's flags and that os.Args[2:] is
// forwarded exactly, including `--` and flag-shaped args.
func TestCodeCommand_ForwardsFlagsThroughCobra(t *testing.T) {
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

	rootCmd.SetArgs([]string{"code", "--kitflag", "x", "--", "pos"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("`kai code` execute failed (cobra likely parsed a kit flag): %v", err)
	}

	want := []string{kitPath, "--kitflag", "x", "--", "pos"}
	if fmt.Sprint(gotArgv) != fmt.Sprint(want) {
		t.Errorf("forwarded argv = %v, want %v", gotArgv, want)
	}
}

// TestCodeMain_FailureIsLoudAndNonZero is the command-boundary version of
// the non-silence guarantee: a failure yields BOTH a non-zero exit code AND
// a non-empty, actionable message on stderr — never a silent exit 0.
func TestCodeMain_FailureIsLoudAndNonZero(t *testing.T) {
	l := kitlauncher.Default()
	l.GOOS = "windows" // unsupported → install errors before any network call
	l.BinDir = t.TempDir()
	l.LookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	var stderr bytes.Buffer
	l.Stderr = &stderr // keep the launcher's progress lines off the real console
	code := codeMain(l, context.Background(), nil, &stderr)

	if code == 0 {
		t.Error("a failure must produce a non-zero exit code")
	}
	if stderr.Len() == 0 {
		t.Error("a failure must print to stderr (no silent failure)")
	}
	if !strings.Contains(stderr.String(), "Couldn't launch the code experience") {
		t.Errorf("expected an actionable message, got %q", stderr.String())
	}
}

// TestCodeMain_SuccessfulHandoff confirms a clean handoff returns 0 and
// prints nothing to stderr.
func TestCodeMain_SuccessfulHandoff(t *testing.T) {
	binDir := t.TempDir()
	fakeManagedKit(t, binDir)

	l := kitlauncher.Default()
	l.BinDir = binDir
	l.LookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
	l.Exec = func(string, []string, []string) error { return nil }

	var stderr bytes.Buffer
	l.Stderr = &stderr
	code := codeMain(l, context.Background(), []string{"--foo"}, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0 on successful handoff, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr on success, got %q", stderr.String())
	}
}
