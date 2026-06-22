package agent

import (
	"encoding/json"
	"fmt"
)

// view_range_dedupe.go: range-overlap deduplication for the view tool.
// Complements the exact-match dedupeCache already used in
// dispatchToolCalls — that catches identical (file, offset, limit)
// triples; this catches the harder pattern that ate 28 of 50 turns
// in the add-config-show-command failure: 30–80-line slices at
// adjacent offsets, each technically a different request but each
// re-fetching content the previous slice already pulled. The agent
// kept paying for tokens to re-read the same regions of main.go.
//
// Heuristic: if any prior view of the same file covered at least
// viewRangeOverlapPct of the new request AND happened within the
// last viewRangeRecentTurns turns, return a stub instead of
// running the view again. The viewLog at the tool level handles
// exact repeats with an mtime guard; this layer handles adjacent
// overlaps with a turn-window guard.

const (
	viewRangeOverlapPct   = 0.80
	viewRangeRecentTurns  = 15
	viewDefaultLineWindow = 2000 // matches viewTool's default limit
)

type viewRange struct {
	Start int // 0-indexed line offset
	End   int // exclusive
	Turn  int // 1-indexed turn the read happened on
}

type viewRangeTracker struct {
	byFile map[string][]viewRange
	// deduped records (file:start:end) requests we've ALREADY served a
	// dedup stub for. If the agent re-requests the same range after
	// being nudged, we serve the real view instead of refusing again —
	// refusing twice corners the model (it can't use buried/large
	// context, can't re-read, and escapes to `bash cat`). First dedup
	// nudges; a repeat serves.
	deduped map[string]bool
}

func newViewRangeTracker() *viewRangeTracker {
	return &viewRangeTracker{byFile: map[string][]viewRange{}, deduped: map[string]bool{}}
}

func dedupeKey(file string, start, end int) string {
	return fmt.Sprintf("%s:%d:%d", file, start, end)
}

// ShouldServeAfterDedup reports whether this exact range has already
// been deduped once — if so the caller should SERVE the re-read rather
// than refuse again. The first call for a range returns false (and
// marks it), so the first overlap still gets the nudge stub.
func (t *viewRangeTracker) ShouldServeAfterDedup(file string, start, end int) bool {
	if t == nil {
		return false
	}
	if t.deduped == nil {
		t.deduped = map[string]bool{}
	}
	k := dedupeKey(file, start, end)
	if t.deduped[k] {
		return true
	}
	t.deduped[k] = true
	return false
}

// Overlap reports whether the requested range is sufficiently covered
// by a recent prior read of the same file. Returns the matching prior
// range so the stub message can quote its turn number.
func (t *viewRangeTracker) Overlap(file string, start, end, currentTurn int) (bool, viewRange) {
	if t == nil || end <= start {
		return false, viewRange{}
	}
	requested := end - start
	for _, r := range t.byFile[file] {
		if currentTurn-r.Turn > viewRangeRecentTurns {
			continue
		}
		ov := minInt(end, r.End) - maxInt(start, r.Start)
		if ov <= 0 {
			continue
		}
		if float64(ov)/float64(requested) >= viewRangeOverlapPct {
			return true, r
		}
	}
	return false, viewRange{}
}

// Reset clears all recorded view ranges. The runner calls this after
// a conversation compaction: the dedupe assumes a recently-viewed
// range is still in the agent's context, but compaction drops old
// tool results — so anything recorded before the compaction is no
// longer reachable. Without the reset the tracker keeps serving stubs
// for content the agent can no longer see, starving it of reads.
func (t *viewRangeTracker) Reset() {
	if t == nil {
		return
	}
	t.byFile = map[string][]viewRange{}
	t.deduped = map[string]bool{}
}

// Record stores a successful view of [start, end) at currentTurn so
// future overlapping reads of the same file can be deduped.
func (t *viewRangeTracker) Record(file string, start, end, currentTurn int) {
	if t == nil || file == "" || end <= start {
		return
	}
	t.byFile[file] = append(t.byFile[file], viewRange{Start: start, End: end, Turn: currentTurn})
}

// parseViewRange extracts the (file_path, start, end) triple from a
// view tool call's JSON input. Mirrors viewTool's parameter handling:
// offset defaults to 0, limit defaults to viewDefaultLineWindow. End
// is exclusive: [offset, offset+limit).
func parseViewRange(inputJSON string) (file string, start, end int, ok bool) {
	var p struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &p); err != nil {
		return "", 0, 0, false
	}
	if p.FilePath == "" {
		return "", 0, 0, false
	}
	limit := p.Limit
	if limit <= 0 {
		limit = viewDefaultLineWindow
	}
	return p.FilePath, p.Offset, p.Offset + limit, true
}

// viewRangeStub is the user-facing message returned in place of a
// re-read. Names the file, the prior range, and the turn it was
// fetched on, then tells the agent two concrete things to do
// depending on whether its target is inside or outside that range.
// Earlier rev said "content is unchanged" — true but unhelpful; the
// failing dogfood transcript showed agents looping ~5 times on
// "I need to see runConfigShow" before they pivoted, because the
// stub didn't tell them what to do. Now it does, explicitly.
func viewRangeStub(file string, prior viewRange) string {
	return fmt.Sprintf(
		`[runner] DUPLICATE VIEW: you already fetched %s lines %d-%d on turn %d (within the last %d turns). The result is still in your context above — scroll up to find it.

If your target IS inside lines %d-%d: use the content already in your context. Do NOT re-view this file.
If your target is OUTSIDE that range: change file_path, or set offset/limit to a strictly non-overlapping window (e.g. offset < %d or offset > %d).

Repeated overlapping views in one run will keep being intercepted. If you can't find what you need without re-viewing, switch to kai_grep, kai_context, kai_callers, or kai_symbols — they return targeted answers without re-fetching the whole window.`,
		file, prior.Start, prior.End, prior.Turn, viewRangeRecentTurns,
		prior.Start, prior.End,
		prior.Start, prior.End)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
