package views

import (
	"strings"
	"testing"
)

// TestStepParser_StripsHeaderAndSteps: a complete STEPS: block plus
// trailing prose. Marker lines are stripped from Display; Block
// surfaces the parsed step descriptions.
func TestStepParser_StripsHeaderAndSteps(t *testing.T) {
	var p stepParser
	in := "STEPS:\n1. Read auth.py\n2. Add middleware\n3. Write tests\n\nNow I'll start.\n"
	ev := p.Feed(in)
	if len(ev.Block) != 3 {
		t.Fatalf("expected 3 steps, got %d: %v", len(ev.Block), ev.Block)
	}
	for i, want := range []string{"Read auth.py", "Add middleware", "Write tests"} {
		if ev.Block[i] != want {
			t.Errorf("step %d = %q, want %q", i, ev.Block[i], want)
		}
	}
	if !strings.Contains(ev.Display, "Now I'll start.") {
		t.Errorf("post-block prose missing from Display: %q", ev.Display)
	}
	if strings.Contains(ev.Display, "STEPS:") || strings.Contains(ev.Display, "1. Read") {
		t.Errorf("marker text leaked into Display: %q", ev.Display)
	}
}

// TestStepParser_StreamingDeltas: the same input split across many
// small deltas should produce the same Block as a single Feed call.
// This is the realistic streaming case.
func TestStepParser_StreamingDeltas(t *testing.T) {
	var p stepParser
	chunks := []string{
		"STEP", "S:\n", "1. fir", "st step\n", "2. second step\n",
		"\n", "Working on it.\n",
	}
	var allBlock []string
	var displayed strings.Builder
	for _, c := range chunks {
		ev := p.Feed(c)
		if len(ev.Block) > 0 {
			allBlock = append(allBlock, ev.Block...)
		}
		displayed.WriteString(ev.Display)
	}
	if len(allBlock) != 2 || allBlock[0] != "first step" || allBlock[1] != "second step" {
		t.Errorf("expected [first step, second step], got %v", allBlock)
	}
	if !strings.Contains(displayed.String(), "Working on it.") {
		t.Errorf("prose missing: %q", displayed.String())
	}
	if strings.Contains(displayed.String(), "STEPS:") {
		t.Errorf("marker leaked: %q", displayed.String())
	}
}

// TestStepParser_StepDoneStrips: STEP_DONE: N is stripped and
// surfaced via DoneIdx (0-indexed; protocol uses 1-indexed).
func TestStepParser_StepDoneStrips(t *testing.T) {
	var p stepParser
	ev := p.Feed("STEP_DONE: 2\nNext bit of prose.\n")
	if ev.DoneIdx != 1 {
		t.Errorf("DoneIdx = %d, want 1 (0-indexed for protocol step 2)", ev.DoneIdx)
	}
	if !strings.Contains(ev.Display, "Next bit of prose.") {
		t.Errorf("post-marker prose missing: %q", ev.Display)
	}
	if strings.Contains(ev.Display, "STEP_DONE") {
		t.Errorf("marker leaked into display: %q", ev.Display)
	}
}

// TestStepParser_NoMarkers: a stream with no markers passes through
// untouched. Block stays empty, DoneIdx stays -1, Display matches
// input. This is the fallback for prompts the model didn't follow.
func TestStepParser_NoMarkers(t *testing.T) {
	var p stepParser
	in := "Sure, I'll edit auth.py to add rate limiting.\n"
	ev := p.Feed(in)
	if len(ev.Block) != 0 {
		t.Errorf("unexpected Block: %v", ev.Block)
	}
	if ev.DoneIdx != -1 {
		t.Errorf("DoneIdx = %d, want -1", ev.DoneIdx)
	}
	if ev.Display != in {
		t.Errorf("Display = %q, want unchanged %q", ev.Display, in)
	}
}

// TestStepParser_StepsHeaderCaseInsensitive: tolerant to model
// stylistic variation.
func TestStepParser_StepsHeaderCaseInsensitive(t *testing.T) {
	var p stepParser
	ev := p.Feed("steps:\n1. one\n2. two\n\n")
	if len(ev.Block) != 2 {
		t.Errorf("lowercase header should still parse: got %d steps", len(ev.Block))
	}
}

// TestStepParser_NumberedFormatTolerant: accepts "1." and "1)" forms
// per parseNumberedStep's contract.
func TestStepParser_NumberedFormatTolerant(t *testing.T) {
	var p stepParser
	ev := p.Feed("STEPS:\n1) first\n2) second\n\n")
	if len(ev.Block) != 2 {
		t.Errorf("'1)' form should parse: got %v", ev.Block)
	}
}

// TestStepParser_FinalizeBlock: when the stream ends mid-block (no
// closing blank line), FinalizeBlock recovers what was parsed so a
// partial checklist still renders.
func TestStepParser_FinalizeBlock(t *testing.T) {
	var p stepParser
	p.Feed("STEPS:\n1. one\n2. two\n")
	// No closing line yet — stream just ended.
	left := p.FinalizeBlock()
	if len(left) != 2 {
		t.Errorf("FinalizeBlock should return 2 pending steps, got %v", left)
	}
	// Idempotent: calling again returns nil.
	if again := p.FinalizeBlock(); again != nil {
		t.Errorf("second FinalizeBlock should be nil, got %v", again)
	}
}

// TestStepParser_DonePrefixOnlyRejected: bare "STEP_DONE" without a
// number must not match — easy false positive otherwise.
func TestStepParser_DonePrefixOnlyRejected(t *testing.T) {
	if _, ok := parseStepDone("STEP_DONE"); ok {
		t.Error("bare STEP_DONE should not match")
	}
	if _, ok := parseStepDone("STEP_DONE:"); ok {
		t.Error("STEP_DONE: with no number should not match")
	}
	if _, ok := parseStepDone("STEP_DONE_FOO"); ok {
		t.Error("STEP_DONE_FOO should not match")
	}
}

// TestStepParser_NotAStepsHeaderInProse: "next steps:" or "the steps:"
// in mid-paragraph prose must not open a block. The header check is
// strict.
func TestStepParser_NotAStepsHeaderInProse(t *testing.T) {
	var p stepParser
	in := "Here are the next steps:\nFirst, I'll read the file.\n"
	ev := p.Feed(in)
	if len(ev.Block) != 0 {
		t.Errorf("prose 'next steps:' must not open a block: %v", ev.Block)
	}
	if !strings.Contains(ev.Display, "Here are the next steps:") {
		t.Errorf("prose should pass through: %q", ev.Display)
	}
}
