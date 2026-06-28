package orchestrator

import (
	"errors"
	"testing"

	"github.com/kaicontext/kai-engine/safetygate"
)

// TestRunOutcome covers the result-tally classification, with the
// regression case front and centre: an agent that errored AFTER
// writing edits (context deadline exceeded mid-verify) has its work
// rescued by the integrate phase and must count as Held, not Failed —
// the user-facing outcome is "there is a change to review."
func TestRunOutcome(t *testing.T) {
	deadline := errors.New("agent x: context deadline exceeded")

	cases := []struct {
		name string
		run  AgentRun
		want runOutcomeKind
	}{
		{
			name: "clean auto-promote",
			run:  mkRun("a", string(safetygate.Auto), nil, nil),
			want: outcomeAuto,
		},
		{
			name: "clean held for review",
			run:  mkRun("a", string(safetygate.Review), nil, nil),
			want: outcomeHeld,
		},
		{
			name: "errored with nothing to rescue",
			run:  mkRun("a", "", deadline, nil),
			want: outcomeFailed,
		},
		{
			name: "REGRESSION: errored but edits rescued and held",
			run:  mkRun("a", string(safetygate.Review), deadline, nil),
			want: outcomeHeld,
		},
		{
			name: "integrate failed",
			run:  mkRun("a", "", nil, errors.New("absorb failed")),
			want: outcomeFailed,
		},
		{
			name: "ran clean, changed nothing",
			run:  mkRun("a", "", nil, nil),
			want: outcomeNone,
		},
		{
			name: "edits on disk, no verdict (legacy)",
			run:  AgentRun{ChangedPaths: []string{"x.go"}},
			want: outcomeHeld,
		},
		// 2026-05-25: verify-gating cases. Gate verdict Auto +
		// VerifySummary non-empty means verify actually ran; the
		// verify outcome then decides whether auto-promote
		// proceeds. VerifyOutcome 1 = verifyPassed (the only auto
		// case); anything else holds.
		{
			name: "auto + verify passed",
			run:  withVerify(mkRun("a", string(safetygate.Auto), nil, nil), 1, "✓ verified"),
			want: outcomeAuto,
		},
		{
			name: "auto + verify blocked → held",
			run:  withVerify(mkRun("a", string(safetygate.Auto), nil, nil), 2, "⚠ blocked"),
			want: outcomeHeld,
		},
		{
			name: "auto + verify applied additional edits → held",
			run:  withVerify(mkRun("a", string(safetygate.Auto), nil, nil), 3, "⚠ applied"),
			want: outcomeHeld,
		},
		{
			name: "auto + verify incomplete → held",
			run:  withVerify(mkRun("a", string(safetygate.Auto), nil, nil), 4, "⚠ incomplete"),
			want: outcomeHeld,
		},
		{
			name: "auto + verify unknown signal → held",
			run:  withVerify(mkRun("a", string(safetygate.Auto), nil, nil), 0, "⚠ unclear"),
			want: outcomeHeld,
		},
		{
			name: "auto + verify did not run (empty summary) → auto",
			run:  mkRun("a", string(safetygate.Auto), nil, nil),
			want: outcomeAuto,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.run
			if got := runOutcome(&r); got != c.want {
				t.Errorf("runOutcome = %d, want %d", got, c.want)
			}
		})
	}
}

// withVerify adds a verify-pass result to an AgentRun. Stamps
// VerifySummary (the "did verify run" signal) and VerifyOutcome.
func withVerify(r AgentRun, outcome int, summary string) AgentRun {
	r.VerifyOutcome = outcome
	r.VerifySummary = summary
	return r
}
