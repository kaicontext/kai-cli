package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// initProgress receives the share of the running kai init step's counted
// work that is done, from 0 to 1. kai init sets it around the git history
// import (per commit) and the first capture (per file parsed); it is nil
// the rest of the time, so `kai import` and `kai capture` run on their own
// report nothing through it.
var initProgress func(done float64)

// reportInitProgress passes done/total to initProgress when kai init is
// listening.
func reportInitProgress(done, total int) {
	if initProgress != nil && total > 0 {
		initProgress(float64(done) / float64(total))
	}
}

// stderrInteractive reports whether stderr is a terminal that can redraw a
// line in place. TERM=dumb terminals (Emacs shell, some CI runners) are
// terminals but don't honour the erase-line escape.
func stderrInteractive() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fd := os.Stderr.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// initStep shows one kai init step on stderr until finish is called.
//
// On an interactive terminal it animates a pulse bar. Anywhere else (a CI
// log, a pipe, a file) every animation frame would be kept as a line of its
// own, so it prints plain lines instead: "label... N%" each time progress
// crosses a multiple of pctStep, starting at 0%. A step that can't measure
// its progress (pctStep 0) prints "label..." once.
//
//	s := startStep("Importing git history", 20)
//	err := doWork(s.progress)
//	s.finish(err)
type initStep struct {
	w       io.Writer
	label   string
	pctStep int
	animate bool

	printed int // last percentage printed; plain mode only

	stop     chan struct{} // closed by finish to end the animation
	animDone chan struct{} // closed by the animation once it has stopped drawing
}

func startStep(label string, pctStep int) *initStep {
	return newStep(os.Stderr, label, pctStep, stderrInteractive())
}

func newStep(w io.Writer, label string, pctStep int, animate bool) *initStep {
	s := &initStep{w: w, label: label, pctStep: pctStep, animate: animate}
	if animate {
		s.stop = make(chan struct{})
		s.animDone = make(chan struct{})
		// First frame right away so even a fast step is visible.
		fmt.Fprintf(w, "\r  %s %s\033[K", renderPulseBar(0), label)
		go s.runAnimation()
		return s
	}
	if pctStep > 0 {
		fmt.Fprintf(w, "  %s... 0%%\n", label)
	} else {
		fmt.Fprintf(w, "  %s...\n", label)
	}
	return s
}

// progress records that done (0 to 1) of the step's work is finished. The
// animation doesn't show it; plain mode prints when it crosses the next
// multiple of pctStep.
func (s *initStep) progress(done float64) {
	if s.animate || s.pctStep <= 0 {
		return
	}
	// The epsilon keeps a share like 0.8*0.5 from truncating to 39%.
	pct := int(done*100 + 1e-9)
	if pct > 100 {
		pct = 100
	}
	pct -= pct % s.pctStep
	if pct <= s.printed {
		return
	}
	s.printed = pct
	fmt.Fprintf(s.w, "  %s... %d%%\n", s.label, pct)
}

// finish ends the step with ✓, or ✗ and the error. A plain-mode step that
// succeeds reports 100% first if its progress never got there, as a step
// whose last stretch can't be counted won't.
func (s *initStep) finish(err error) {
	if s.animate {
		close(s.stop)
		<-s.animDone
	} else if err == nil {
		s.progress(1)
	}

	cr, eol := "", ""
	if s.animate {
		cr, eol = "\r", "\033[K"
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		fmt.Fprintf(s.w, "%s  %s %s %s%s\n", cr, cRed("✗"), s.label, cDim(msg), eol)
		return
	}
	fmt.Fprintf(s.w, "%s  %s %s%s\n", cr, cGreen("✓"), s.label, eol)
}

// runAnimation bounces the pulse across the bar until finish stops it.
func (s *initStep) runAnimation() {
	defer close(s.animDone)
	t := time.NewTicker(45 * time.Millisecond)
	defer t.Stop()
	pos, dir := 1, 1
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			fmt.Fprintf(s.w, "\r  %s %s\033[K", renderPulseBar(pos), s.label)
			pos += dir
			if pos >= pulseBarWidth-1 {
				dir = -1
			} else if pos <= 0 {
				dir = 1
			}
		}
	}
}

const (
	pulseBarWidth = 16
	pulseHalf     = 3
)

// renderPulseBar draws the bar with a soft glow centred on pos: a blue
// gradient when stderr has colour, heavier dashes when it doesn't.
func renderPulseBar(pos int) string {
	pulseColors := []string{"240", "246", "74", "81", "74", "246"}
	const dimColor = "238"

	var b strings.Builder
	for i := 0; i < pulseBarWidth; i++ {
		dist := i - pos
		if dist < 0 {
			dist = -dist
		}
		if dist > pulseHalf {
			if stderrColor {
				fmt.Fprintf(&b, "\033[38;5;%sm─\033[0m", dimColor)
			} else {
				b.WriteRune('─')
			}
			continue
		}
		lvl := pulseHalf - dist
		idx := lvl * (len(pulseColors) - 1) / pulseHalf
		if stderrColor {
			fmt.Fprintf(&b, "\033[38;5;%sm─\033[0m", pulseColors[idx])
		} else {
			b.WriteRune('━')
		}
	}
	return b.String()
}
