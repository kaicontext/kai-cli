package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// newTestProgressBar builds a progress bar that writes to w, animated or
// not, whatever the test process's stderr is.
func newTestProgressBar(w io.Writer, label string, pctInterval int, animate bool) *progressBar {
	p := newProgressBar(label, pctInterval)
	p.w, p.animate = w, animate
	return p
}

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

func TestProgressBarShowsNothingUntilStart(t *testing.T) {
	for _, animate := range []bool{false, true} {
		var buf bytes.Buffer
		graphProgressBar := newTestProgressBar(&buf, "Building semantic graph", 20, animate)
		if buf.Len() != 0 {
			t.Fatalf("animate=%v: printed before start: %q", animate, buf.String())
		}
		graphProgressBar.start()
		if buf.Len() == 0 {
			t.Fatalf("animate=%v: start printed nothing", animate)
		}
		graphProgressBar.finish(nil)
	}
}

// The graph build's parse count fills 0–80% in 20% intervals and finishing
// supplies 100%, as runCaptureWithProgress reports it.
func TestProgressBarPlainGraphBuild(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	graphProgressBar := newTestProgressBar(&buf, "Building semantic graph", 20, false)
	graphProgressBar.start()
	for file := 1; file <= 10; file++ {
		graphProgressBar.progress(0.8 * float64(file) / 10)
	}
	graphProgressBar.finish(nil)

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

// A count that isn't a multiple of the interval prints the last multiple it
// reached, and reaching 100% on its own isn't printed twice.
func TestProgressBarPlainUnevenCount(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	importProgressBar := newTestProgressBar(&buf, "Importing git history", 20, false)
	importProgressBar.start()
	for commit := 1; commit <= 3; commit++ {
		importProgressBar.progress(float64(commit) / 3)
	}
	importProgressBar.finish(nil)

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Importing git history... 0%",
		"  Importing git history... 20%",
		"  Importing git history... 60%",
		"  Importing git history... 100%",
		"  ✓ Importing git history",
	})
}

func TestProgressBarPlainFailureStopsWhereItGot(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	importProgressBar := newTestProgressBar(&buf, "Importing git history", 20, false)
	importProgressBar.start()
	importProgressBar.progress(0.45)
	importProgressBar.finish(errors.New("reading commit abc: bad object"))

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Importing git history... 0%",
		"  Importing git history... 40%",
		"  ✗ Importing git history reading commit abc: bad object",
	})
}

func TestProgressBarPlainUnmeasured(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	pushProgressBar := newTestProgressBar(&buf, "Pushing to kaicontext.com", 0, false)
	pushProgressBar.start()
	pushProgressBar.progress(0.5)
	pushProgressBar.finish(nil)

	assertLines(t, plainLines(t, buf.String()), []string{
		"  Pushing to kaicontext.com...",
		"  ✓ Pushing to kaicontext.com",
	})
}

// finish stops the animation before it writes the result, so no frame
// lands after the ✓ line.
func TestProgressBarAnimatedEndsWithResult(t *testing.T) {
	noColor(t)
	var buf bytes.Buffer
	graphProgressBar := newTestProgressBar(&buf, "Building semantic graph", 20, true)
	graphProgressBar.start()
	graphProgressBar.progress(0.5)
	graphProgressBar.finish(nil)

	out := buf.String()
	if strings.Contains(out, "%") {
		t.Fatalf("animated progress bar printed a percentage: %q", out)
	}
	if want := "\r  ✓ Building semantic graph\033[K\n"; !strings.HasSuffix(out, want) {
		t.Fatalf("animated output ends %q, want suffix %q", out[max(0, len(out)-80):], want)
	}
}

// finish on a progress bar that was never started prints just the result
// line, in both modes; an animated one has no animation to stop.
func TestProgressBarFinishWithoutStart(t *testing.T) {
	noColor(t)
	for _, tc := range []struct {
		animate bool
		want    string
	}{
		{false, "  ✓ Pushing to kaicontext.com\n"},
		{true, "\r  ✓ Pushing to kaicontext.com\033[K\n"},
	} {
		var buf bytes.Buffer
		pushProgressBar := newTestProgressBar(&buf, "Pushing to kaicontext.com", 20, tc.animate)
		pushProgressBar.progress(0.5)
		pushProgressBar.finish(nil)
		if got := buf.String(); got != tc.want {
			t.Errorf("animate=%v: got %q, want %q", tc.animate, got, tc.want)
		}
	}
}

// start and finish act only the first time, and progress after finish is
// ignored.
func TestProgressBarRepeatedCalls(t *testing.T) {
	noColor(t)
	for _, animate := range []bool{false, true} {
		var buf bytes.Buffer
		importProgressBar := newTestProgressBar(&buf, "Importing git history", 20, animate)
		importProgressBar.start()
		importProgressBar.start()
		importProgressBar.finish(nil)
		importProgressBar.finish(nil)
		importProgressBar.progress(0.5)
		importProgressBar.start()

		out := buf.String()
		if n := strings.Count(out, "✓ Importing git history"); n != 1 {
			t.Errorf("animate=%v: %d result lines, want 1: %q", animate, n, out)
		}
		if !animate {
			assertLines(t, plainLines(t, out), []string{
				"  Importing git history... 0%",
				"  Importing git history... 100%",
				"  ✓ Importing git history",
			})
		}
	}
}

func TestStderrInteractiveDumbTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if stderrInteractive() {
		t.Fatal("TERM=dumb counted as interactive")
	}
}
