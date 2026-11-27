package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func plainLines(t *testing.T, out string) []string {
	t.Helper()
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("plain output has a carriage return or escape code: %q", out)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func noColor(t *testing.T) {
	t.Helper()
	prev := stderrColor
	stderrColor = false
	t.Cleanup(func() { stderrColor = prev })
}

// The graph build's parse count fills 0–80% in 20% steps and finishing
// supplies 100%, the way kai init wires it.
func TestInitStepPlainGraphBuild(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	s := newStep(&buf, "Building semantic graph", 20, false)
	initProgress = func(parsed float64) { s.progress(0.8 * parsed) }
	t.Cleanup(func() { initProgress = nil })

	for file := 1; file <= 10; file++ {
		reportInitProgress(file, 10)
	}
	s.finish(nil)

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Building semantic graph... 0%",
		"  Building semantic graph... 20%",
		"  Building semantic graph... 40%",
		"  Building semantic graph... 60%",
		"  Building semantic graph... 80%",
		"  Building semantic graph... 100%",
		"  ✓ Building semantic graph",
	})
}

// A count that isn't a multiple of the step prints the step it has
// reached, and reaching 100% on its own isn't printed twice.
func TestInitStepPlainUnevenCount(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	s := newStep(&buf, "Importing git history", 20, false)
	for commit := 1; commit <= 3; commit++ {
		s.progress(float64(commit) / 3)
	}
	s.finish(nil)

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Importing git history... 0%",
		"  Importing git history... 20%",
		"  Importing git history... 60%",
		"  Importing git history... 100%",
		"  ✓ Importing git history",
	})
}

func TestInitStepPlainFailureStopsWhereItGot(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	s := newStep(&buf, "Importing git history", 20, false)
	s.progress(0.45)
	s.finish(errors.New("reading commit abc: bad object"))

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Importing git history... 0%",
		"  Importing git history... 40%",
		"  ✗ Importing git history reading commit abc: bad object",
	})
}

func TestInitStepPlainUnmeasured(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	s := newStep(&buf, "Pushing to kaicontext.com", 0, false)
	s.progress(0.5)
	s.finish(nil)

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Pushing to kaicontext.com...",
		"  ✓ Pushing to kaicontext.com",
	})
}

// finish stops the animation before it writes the result, so no frame
// lands after the ✓ line.
func TestInitStepAnimatedEndsWithResult(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	s := newStep(&buf, "Building semantic graph", 20, true)
	s.progress(0.5)
	s.finish(nil)

	out := buf.String()
	if strings.Contains(out, "%") {
		t.Fatalf("animated step printed a percentage: %q", out)
	}
	if want := "\r  ✓ Building semantic graph\033[K\n"; !strings.HasSuffix(out, want) {
		t.Fatalf("animated output ends %q, want suffix %q", out[max(0, len(out)-80):], want)
	}
}

func TestStderrInteractiveDumbTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if stderrInteractive() {
		t.Fatal("TERM=dumb counted as interactive")
	}
}

func TestReportInitProgressWithoutListener(t *testing.T) {
	initProgress = nil
	reportInitProgress(1, 2) // must not panic
}
