package agent

import (
	"strings"
	"testing"
)

func TestLoopTracker_FiresOnIdenticalRepeatedFailures(t *testing.T) {
	l := newLoopTracker()
	// First two failures: not yet a loop (trigger is 3).
	for i := 0; i < loopTrackerTrigger-1; i++ {
		l.Record("bash", `{"command":"cd /Users/x && go build"}`, true)
		if l.IsLoop("bash", `{"command":"cd /Users/x && go build"}`) {
			t.Errorf("loop fired after %d failures; trigger is %d", i+1, loopTrackerTrigger)
		}
	}
	// Third identical failure brings us to the trigger; the next
	// IsLoop call (the would-be fourth attempt) should fire.
	l.Record("bash", `{"command":"cd /Users/x && go build"}`, true)
	if !l.IsLoop("bash", `{"command":"cd /Users/x && go build"}`) {
		t.Errorf("loop should fire on attempt %d", loopTrackerTrigger+1)
	}
}

func TestLoopTracker_NotALoopIfOneSucceeded(t *testing.T) {
	l := newLoopTracker()
	l.Record("bash", "X", true)
	l.Record("bash", "X", true)
	l.Record("bash", "X", false) // one success interrupts the streak
	l.Record("bash", "X", true)
	l.Record("bash", "X", true)
	// Trailing window is [true, false, true, true] — the false breaks
	// the streak even though there are 2 trailing failures.
	if l.IsLoop("bash", "X") {
		t.Errorf("loop should not fire when a success interrupts the streak")
	}
}

func TestLoopTracker_DifferentInputResetsStreak(t *testing.T) {
	l := newLoopTracker()
	l.Record("bash", "Y", true)
	l.Record("bash", "Y", true)
	l.Record("bash", "Y", true)
	if !l.IsLoop("bash", "Y") {
		t.Fatalf("setup: 3 identical failures should be a loop for Y")
	}
	// A different input doesn't trigger — the streak is about Y.
	if l.IsLoop("bash", "Z") {
		t.Errorf("loop should not fire for different input Z when streak is for Y")
	}
}

func TestLoopTracker_DifferentToolResetsStreak(t *testing.T) {
	l := newLoopTracker()
	l.Record("bash", "X", true)
	l.Record("bash", "X", true)
	l.Record("view", "X", true)
	if l.IsLoop("bash", "X") {
		t.Errorf("interleaving a different tool should break the streak")
	}
}

func TestLoopTracker_InterceptMessageIsActionable(t *testing.T) {
	msg := loopInterceptMessage("bash")
	for _, want := range []string{"loop detected", "bash", "different", "stop and report"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("intercept message missing %q: %q", want, msg)
		}
	}
}

func TestLoopTracker_WindowBoundsMemory(t *testing.T) {
	l := newLoopTracker()
	// Fill past the window with mixed entries; the tracker should
	// only retain the last loopTrackerWindow.
	for i := 0; i < loopTrackerWindow*2; i++ {
		l.Record("noise", "n", false)
	}
	if got := len(l.recent); got > loopTrackerWindow {
		t.Errorf("tracker retained %d entries; window is %d", got, loopTrackerWindow)
	}
}
