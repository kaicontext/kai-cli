package session

import (
	"errors"
	"testing"
	"time"
)

func TestIsSQLiteBusy(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("database is locked"), true},
		{errors.New("session.AppendMessage: insert: database is locked"), true},
		{errors.New("SQLITE_BUSY: database is locked"), true},
		{errors.New("disk I/O error (5)"), true}, // SQLITE_BUSY code
		{errors.New("syntax error near INSERT"), false},
		{errors.New("no such table: agent_messages"), false},
	}
	for _, c := range cases {
		if got := isSQLiteBusy(c.err); got != c.want {
			t.Errorf("isSQLiteBusy(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestAppendRetryDelay_MonotonicAndBounded(t *testing.T) {
	// Schedule must be non-decreasing and bounded at 3s. Total wait
	// across the full retry window (5 attempts) should not exceed
	// ~5.5s, leaving the 10s aggregate budget the doc-comment
	// promises (with the driver's 5s busy_timeout layered above).
	var prev time.Duration
	var total time.Duration
	for i := 0; i < maxAppendRetries; i++ {
		d := appendRetryDelay(i)
		if d < prev {
			t.Errorf("retry delay decreased at attempt %d: %v < %v", i, d, prev)
		}
		if d > 3*time.Second {
			t.Errorf("retry delay at attempt %d exceeds 3s cap: %v", i, d)
		}
		prev = d
		total += d
	}
	if total > 6*time.Second {
		t.Errorf("total retry wait exceeds 6s budget: %v", total)
	}
}
