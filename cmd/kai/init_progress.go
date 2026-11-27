package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

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

// progressBar shows the progress of one kai init task on stderr, from start
// until finish.
//
// On an interactive terminal it animates a pulse bar. Anywhere else (a CI
// log, a pipe, a file) every animation frame would be kept as a line of its
// own, so it prints plain lines instead: "label... N%" each time progress
// crosses a multiple of pctInterval, starting at 0%. A task that can't
// measure its progress (pctInterval 0) prints "label..." once.
//
//	importProgressBar := newProgressBar("Importing git history", 20)
//	importProgressBar.start()
//	err := runGitImport(db, importProgressBar) // calls progress as commits land
//	importProgressBar.finish(err)
type progressBar struct {
	w           io.Writer
	label       string
	pctInterval int
	animate     bool

	printed int // last percentage printed; plain mode only

	stop     chan struct{} // closed by finish to end the animation
	animDone chan struct{} // closed by the animation once it has stopped drawing
}

// newProgressBar builds a progress bar on stderr, animated when stderr is an
// interactive terminal. It shows nothing until start.
func newProgressBar(label string, pctInterval int) *progressBar {
	return &progressBar{
		w:           os.Stderr,
		label:       label,
		pctInterval: pctInterval,
		animate:     stderrInteractive(),
	}
}

// start shows the progress bar: the first animation frame, or the first
// plain line.
func (p *progressBar) start() {
	if p.animate {
		p.stop = make(chan struct{})
		p.animDone = make(chan struct{})
		// First frame right away so even a fast task is visible.
		fmt.Fprintf(p.w, "\r  %s %s\033[K", renderPulseBar(0), p.label)
		go p.runAnimation()
		return
	}
	if p.pctInterval > 0 {
		fmt.Fprintf(p.w, "  %s... 0%%\n", p.label)
	} else {
		fmt.Fprintf(p.w, "  %s...\n", p.label)
	}
}

// progress records that done (0 to 1) of the task is finished. The
// animation doesn't show it; plain mode prints when it crosses the next
// multiple of pctInterval.
func (p *progressBar) progress(done float64) {
	if p.animate || p.pctInterval <= 0 {
		return
	}
	// The epsilon keeps a share like 0.8*0.5 from truncating to 39%.
	pct := int(done*100 + 1e-9)
	if pct > 100 {
		pct = 100
	}
	pct -= pct % p.pctInterval
	if pct <= p.printed {
		return
	}
	p.printed = pct
	fmt.Fprintf(p.w, "  %s... %d%%\n", p.label, pct)
}

// finish ends the progress bar with ✓, or ✗ and the error. In plain mode
// a task that succeeds reports 100% first if its progress never got there,
// as a task whose last stretch can't be counted won't.
func (p *progressBar) finish(err error) {
	if p.animate {
		close(p.stop)
		<-p.animDone
	} else if err == nil {
		p.progress(1)
	}

	cr, eol := "", ""
	if p.animate {
		cr, eol = "\r", "\033[K"
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		fmt.Fprintf(p.w, "%s  %s %s %s%s\n", cr, cRed("✗"), p.label, cDim(msg), eol)
		return
	}
	fmt.Fprintf(p.w, "%s  %s %s%s\n", cr, cGreen("✓"), p.label, eol)
}

// runAnimation bounces the pulse across the bar until finish stops it.
func (p *progressBar) runAnimation() {
	defer close(p.animDone)
	t := time.NewTicker(45 * time.Millisecond)
	defer t.Stop()
	pos, dir := 1, 1
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			fmt.Fprintf(p.w, "\r  %s %s\033[K", renderPulseBar(pos), p.label)
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
