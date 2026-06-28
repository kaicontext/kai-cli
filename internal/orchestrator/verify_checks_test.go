package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/planner"
)

func intp(i int) *int { return &i }

func TestRunVerifyChecks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	cases := []struct {
		name    string
		check   planner.VerifyCheck
		wantOK  bool // true = expect NO failure
	}{
		{"exit0 passes", planner.VerifyCheck{Run: "true"}, true},
		{"exit0 default catches failure", planner.VerifyCheck{Run: "false"}, false},
		{"explicit nonzero exit passes", planner.VerifyCheck{Run: "exit 3", ExpectExit: intp(3)}, true},
		{"wrong exit fails", planner.VerifyCheck{Run: "exit 2", ExpectExit: intp(0)}, false},
		{"stdout contains passes", planner.VerifyCheck{Run: "echo hello world", ExpectStdoutContains: "world"}, true},
		{"stdout missing fails", planner.VerifyCheck{Run: "echo hello", ExpectStdoutContains: "world"}, false},
		// The actual news-ticker case: a command that errors on an unknown flag.
		{"unknown-flag style: nonzero exit caught", planner.VerifyCheck{Run: "sh -c 'echo \"unknown flag: --json\" >&2; exit 1'", ExpectExit: intp(0)}, false},
		{"bad command can't run -> failure", planner.VerifyCheck{Run: "this-binary-does-not-exist-xyz"}, false},
		{"empty run is skipped (no failure)", planner.VerifyCheck{Run: "   "}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fails := runVerifyChecks(ctx, []planner.VerifyCheck{c.check}, dir)
			if c.wantOK && len(fails) != 0 {
				t.Errorf("expected pass, got failures: %v", fails)
			}
			if !c.wantOK && len(fails) == 0 {
				t.Errorf("expected a failure, got none")
			}
		})
	}
}

func TestRunVerifyChecks_FailureLineHasContext(t *testing.T) {
	fails := runVerifyChecks(context.Background(),
		[]planner.VerifyCheck{{Run: "echo nope; exit 1", ExpectExit: intp(0), Why: "ticker must receive events"}}, t.TempDir())
	if len(fails) != 1 {
		t.Fatalf("want 1 failure, got %d", len(fails))
	}
	for _, want := range []string{"verify check failed", "ticker must receive events", "exited 1", "nope"} {
		if !strings.Contains(fails[0], want) {
			t.Errorf("failure line missing %q:\n%s", want, fails[0])
		}
	}
}
