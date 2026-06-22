package views

import (
	"strconv"
	"strings"
)

// stepParser is a streaming sentinel scanner for the STEPS: /
// STEP_DONE: marker protocol the coding/debug system prompts ask
// the model to follow.
//
// Wire shape: the parser receives every assistant text delta as it
// streams in. It accumulates a line buffer, scans for marker lines,
// strips them from the displayable text, and emits parser events
// (block discovered, step transitioned). The REPL's stream pipeline
// pipes the parser's "display" output into the on-screen buffer
// instead of the raw model text.
//
// Why streaming-aware: we can't wait for the full response to detect
// markers. The whole point of the inline checklist is to update as
// the model works. So the parser handles partial lines (a delta that
// ends mid-line is buffered until the newline arrives).
type stepParser struct {
	// buf is the unflushed tail of the current line. As deltas
	// stream in, completed lines are processed and removed; any
	// partial trailing line stays here until the next newline.
	//
	// Plain string (not strings.Builder) because REPL embeds
	// stepParser by value, and Bubble Tea's value-receiver
	// Update copies REPL every tick. strings.Builder explicitly
	// forbids being copied after first use — its copyCheck
	// panics on the next WriteString. A plain string survives
	// the copies cleanly; the perf cost is negligible since
	// each delta is small and concatenation is O(n) per line.
	buf string

	// inSteps signals we're inside a STEPS: block, expecting
	// numbered step lines. Cleared when we hit the first
	// non-numbered, non-empty line.
	inSteps bool
	// pendingSteps accumulates step descriptions while inSteps is
	// true. Emitted as a stepBlockEvent on the line that closes
	// the block.
	pendingSteps []string
}

// stepEvent is what the parser emits for the REPL to consume.
// One of `block` or `done` is non-empty per event.
type stepEvent struct {
	// Display is the (possibly empty) chunk of post-strip text the
	// REPL should render in scrollback / streaming buffer. Marker
	// lines are stripped here, so this is what the developer sees.
	Display string
	// Block is non-nil when the parser just finished consuming a
	// STEPS: block. Each entry is a step description, in order.
	Block []string
	// DoneIdx is non-negative when STEP_DONE: N was just seen. The
	// REPL maps N (1-indexed in the protocol) to a 0-indexed step
	// in TaskProgress.
	DoneIdx int
}

// newStepEvent returns a zero-value-friendly event with DoneIdx=-1
// so the "no done" case is unambiguous (0 is a real step index).
func newStepEvent() stepEvent {
	return stepEvent{DoneIdx: -1}
}

// Feed processes a streaming delta. Returns one event per call,
// summarizing every transition the delta caused. When a delta spans
// multiple events (e.g. crosses a STEP_DONE: line plus more prose),
// the returned event collapses them: Display contains the surviving
// text and DoneIdx contains the LAST step transition seen. Block is
// returned when the delta closed a STEPS: block.
//
// Callers feed the same parser instance across the whole turn. The
// parser is single-goroutine-only — Bubble Tea's Update drives it
// serially.
func (p *stepParser) Feed(delta string) stepEvent {
	ev := newStepEvent()
	pending := p.buf + delta
	p.buf = ""
	// Process complete lines; keep any trailing partial line in
	// the buffer for the next delta.
	for {
		nl := strings.IndexByte(pending, '\n')
		if nl < 0 {
			// Incomplete trailing line. Three cases:
			//   - Inside a STEPS: block: hold for the next delta;
			//     it might be a numbered step.
			//   - Could be a marker opening (e.g. "STE", "STEPS",
			//     "STEP_DON"): hold so we don't leak the marker
			//     prefix into the displayed prose.
			//   - Plain prose: flush so the streaming UX feels
			//     live (text appears char-by-char).
			if p.inSteps || partialMightBeMarker(pending) {
				p.buf = pending
			} else {
				ev.Display += pending
			}
			break
		}
		line := pending[:nl]
		rest := pending[nl+1:]
		pending = rest

		// STEPS: header opens a block.
		if !p.inSteps && isStepsHeader(line) {
			p.inSteps = true
			p.pendingSteps = p.pendingSteps[:0]
			// Strip the marker; do NOT emit to display.
			continue
		}
		// Inside a STEPS: block: parse "1. desc" / "2. desc" etc.
		// Stop the block on the first non-step line.
		if p.inSteps {
			if step, ok := parseNumberedStep(line); ok {
				p.pendingSteps = append(p.pendingSteps, step)
				continue
			}
			// Block closed. Emit it and fall through to handle
			// the current line normally.
			ev.Block = append([]string{}, p.pendingSteps...)
			p.pendingSteps = p.pendingSteps[:0]
			p.inSteps = false
			// An empty closing line is a natural separator; don't
			// pollute the display with it. Non-empty closer is
			// real prose and falls through to the display path.
			if strings.TrimSpace(line) == "" {
				continue
			}
		}
		// STEP_DONE: N marker outside a STEPS: block.
		if idx, ok := parseStepDone(line); ok {
			ev.DoneIdx = idx
			continue
		}
		// Everything else is real assistant prose; preserve the
		// newline.
		ev.Display += line + "\n"
	}
	return ev
}

// FinalizeBlock returns and clears any pending STEPS: block that
// hasn't been closed yet (e.g. the model's response ended mid-block,
// or the closing prose hasn't streamed in). Called at end-of-turn so
// a half-streamed block still produces a checklist. Returns nil if
// there's nothing to emit.
func (p *stepParser) FinalizeBlock() []string {
	if !p.inSteps || len(p.pendingSteps) == 0 {
		return nil
	}
	out := append([]string{}, p.pendingSteps...)
	p.pendingSteps = p.pendingSteps[:0]
	p.inSteps = false
	return out
}

// partialMightBeMarker reports whether a partial (no-newline) line
// could still grow into a STEPS: or STEP_DONE: marker. We only need
// to consider lines that START with whitespace + the marker prefix;
// anything else is unambiguously prose. Case-insensitive comparison
// matches the markers' case-insensitive recognition.
func partialMightBeMarker(s string) bool {
	t := strings.TrimLeft(s, " \t")
	if t == "" {
		// Pure whitespace can still grow into a marker.
		return true
	}
	upper := strings.ToUpper(t)
	// The line is a possible marker opener if it's a prefix of
	// either marker name OR begins with the full marker prefix
	// followed by something we haven't decided on yet.
	for _, full := range []string{"STEPS:", "STEP_DONE"} {
		if strings.HasPrefix(full, upper) || strings.HasPrefix(upper, full) {
			return true
		}
	}
	return false
}

// isStepsHeader: line is exactly "STEPS:" (case-insensitive,
// whitespace-tolerant). Stricter than a contains-check to avoid
// matching prose like "the next steps:" mid-paragraph.
func isStepsHeader(line string) bool {
	t := strings.TrimSpace(line)
	return strings.EqualFold(t, "STEPS:")
}

// parseNumberedStep recognizes "1. description" / "2) description" /
// "  3. description" forms. Returns the trimmed description and
// whether the line was a step. Empty / whitespace-only lines also
// return false so callers can use them as block boundaries.
func parseNumberedStep(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" {
		return "", false
	}
	// Find the first non-digit position.
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", false
	}
	// Need ". " or ") " after the digits.
	if i >= len(t) {
		return "", false
	}
	if t[i] != '.' && t[i] != ')' {
		return "", false
	}
	desc := strings.TrimSpace(t[i+1:])
	if desc == "" {
		return "", false
	}
	return desc, true
}

// parseStepDone recognizes "STEP_DONE: N" (case-insensitive). Returns
// the 0-indexed step number (protocol uses 1-indexed) and whether
// the line matched. Tolerates "STEP_DONE 3" / "step_done: 3" / extra
// whitespace; rejects bare "STEP_DONE" with no number.
func parseStepDone(line string) (int, bool) {
	t := strings.TrimSpace(line)
	const prefix = "STEP_DONE"
	if len(t) <= len(prefix) || !strings.EqualFold(t[:len(prefix)], prefix) {
		return 0, false
	}
	rest := t[len(prefix):]
	rest = strings.TrimLeft(rest, ": \t")
	if rest == "" {
		return 0, false
	}
	// Take the leading integer.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 1 {
		return 0, false
	}
	return n - 1, true
}
