package agent

import (
	"strings"
	"testing"
)

func TestViewRangeTracker_DetectsOverlap(t *testing.T) {
	tr := newViewRangeTracker()
	tr.Record("main.go", 3700, 3880, 12)

	// Exact repeat → 100% overlap.
	if hit, _ := tr.Overlap("main.go", 3700, 3880, 13); !hit {
		t.Errorf("exact repeat should overlap")
	}
	// Mostly overlapping (80%+ of the new request lives in the prior).
	if hit, _ := tr.Overlap("main.go", 3700, 3800, 13); !hit {
		t.Errorf("strict subset of prior range should overlap")
	}
	// Different file — no overlap.
	if hit, _ := tr.Overlap("other.go", 3700, 3880, 13); hit {
		t.Errorf("different file must not overlap")
	}
	// Disjoint range on same file — no overlap.
	if hit, _ := tr.Overlap("main.go", 5000, 5100, 13); hit {
		t.Errorf("disjoint range should not overlap")
	}
}

// TestViewRangeTracker_ResetClearsHistory is the regression test for
// the read-starvation bug: the dedupe tracker assumes a recently-viewed
// range is still in the agent's context. When the conversation is
// compacted, that content is dropped — so the runner must Reset the
// tracker, or it will keep serving stubs for content the agent can no
// longer see (observed on the DeepSeek bbolt t8 run: the agent could
// never re-read tx.go and guessed the edit, which then failed).
func TestViewRangeTracker_ResetClearsHistory(t *testing.T) {
	tr := newViewRangeTracker()
	tr.Record("tx.go", 370, 470, 5)

	// Before reset: an overlapping re-view is deduped.
	if hit, _ := tr.Overlap("tx.go", 370, 470, 6); !hit {
		t.Fatalf("precondition: overlapping re-view should hit before reset")
	}

	// Compaction just dropped the viewed content from context.
	tr.Reset()

	// After reset: the same re-view must be allowed through — the
	// agent needs the content back, it is no longer in context.
	if hit, _ := tr.Overlap("tx.go", 370, 470, 7); hit {
		t.Errorf("after Reset the tracker must not dedupe — content was compacted away")
	}
}

func TestViewRangeTracker_BelowOverlapThreshold(t *testing.T) {
	tr := newViewRangeTracker()
	tr.Record("main.go", 0, 100, 5)
	// Requested [0, 1000); prior covers [0, 100) → 10% overlap. Below 80%.
	if hit, _ := tr.Overlap("main.go", 0, 1000, 6); hit {
		t.Errorf("10%% overlap should be below the 80%% threshold")
	}
}

func TestViewRangeTracker_TurnWindowExpires(t *testing.T) {
	tr := newViewRangeTracker()
	tr.Record("main.go", 0, 200, 1)
	// 16 turns later — beyond viewRangeRecentTurns (15).
	if hit, _ := tr.Overlap("main.go", 0, 200, 17); hit {
		t.Errorf("read beyond turn window should not block")
	}
	// 15 turns later — within window (currentTurn-recordTurn == 15, <=15).
	if hit, _ := tr.Overlap("main.go", 0, 200, 16); !hit {
		t.Errorf("read at turn-window boundary should still block")
	}
}

func TestParseViewRange_DefaultsAndCustom(t *testing.T) {
	cases := []struct {
		name               string
		input              string
		wantFile           string
		wantStart, wantEnd int
		wantOK             bool
	}{
		{
			name:      "default offset and limit",
			input:     `{"file_path":"main.go"}`,
			wantFile:  "main.go",
			wantStart: 0,
			wantEnd:   viewDefaultLineWindow,
			wantOK:    true,
		},
		{
			name:      "explicit offset and limit",
			input:     `{"file_path":"main.go","offset":3700,"limit":180}`,
			wantFile:  "main.go",
			wantStart: 3700,
			wantEnd:   3880,
			wantOK:    true,
		},
		{
			name:   "missing file_path fails",
			input:  `{"offset":10}`,
			wantOK: false,
		},
		{
			name:   "bad JSON fails",
			input:  `not-json`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file, start, end, ok := parseViewRange(c.input)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if file != c.wantFile || start != c.wantStart || end != c.wantEnd {
				t.Errorf("got (%q, %d, %d), want (%q, %d, %d)",
					file, start, end, c.wantFile, c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestViewRangeStub_NamesFileAndTurn(t *testing.T) {
	stub := viewRangeStub("kai-cli/cmd/kai/main.go", viewRange{Start: 3700, End: 3880, Turn: 12})
	for _, want := range []string{"kai-cli/cmd/kai/main.go", "3700", "3880", "turn 12"} {
		if !strings.Contains(stub, want) {
			t.Errorf("stub missing %q: %s", want, stub)
		}
	}
}

// TestViewRangeStub_GivesActionableGuidance pins the message contract
// the previous rev lacked: tell the agent (a) the prior range is in
// context, (b) what to do if the target is inside or outside that
// range, (c) which alternative tools to try. The failing dogfood
// loops on "I need to see X" happened because the stub said
// "content is unchanged" — true but unhelpful.
func TestViewRangeStub_GivesActionableGuidance(t *testing.T) {
	stub := viewRangeStub("main.go", viewRange{Start: 100, End: 300, Turn: 5})
	wantFragments := []string{
		"DUPLICATE VIEW",
		"scroll up",
		"target IS inside",
		"target is OUTSIDE",
		"kai_grep",    // alternative tool suggested
		"kai_context", // alternative tool suggested
	}
	for _, want := range wantFragments {
		if !strings.Contains(stub, want) {
			t.Errorf("stub missing actionable guidance fragment %q:\n%s", want, stub)
		}
	}
}

// TestViewRangeTracker_ServesOnInsistence pins the corner-breaker:
// the first overlapping re-read of a range gets nudged (ShouldServe
// returns false → caller refuses with the stub), but a repeat request
// for the SAME range serves it (returns true → caller lets the view
// run). Refusing twice corners the model into `bash cat`.
func TestViewRangeTracker_ServesOnInsistence(t *testing.T) {
	tr := newViewRangeTracker()
	if tr.ShouldServeAfterDedup("main.go", 100, 300) {
		t.Fatalf("first dedup of a range must NOT serve (it should nudge)")
	}
	if !tr.ShouldServeAfterDedup("main.go", 100, 300) {
		t.Errorf("repeat dedup of the same range must serve, not refuse again")
	}
	// A different range starts its own nudge→serve cycle.
	if tr.ShouldServeAfterDedup("main.go", 900, 1100) {
		t.Errorf("a distinct range's first dedup must nudge, not serve")
	}
	// Reset clears the served history so the cycle restarts post-compaction.
	tr.Reset()
	if tr.ShouldServeAfterDedup("main.go", 100, 300) {
		t.Errorf("after Reset the range should nudge again, not serve")
	}
}
