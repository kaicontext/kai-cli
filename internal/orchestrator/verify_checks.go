package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"kai/internal/planner"
)

// verifyCheckTimeout bounds a single machine check so a hung command
// can't stall integrate.
const verifyCheckTimeout = 60 * time.Second

// runVerifyChecks executes each of the planner's machine-checkable
// acceptance checks in dir and returns a human-readable failure line
// for every check whose REAL result doesn't match what was declared.
// An empty return means all checks passed (or there were none).
//
// The whole point is that the HARNESS runs these — not the agent and
// not a verify-agent narrating prose — so a confabulated "I verified
// it" (the failure where an agent runs a command, sees it error, then
// claims it confirmed the command works) has nothing to satisfy. The
// command either exits as declared and emits the declared substring,
// or it doesn't.
func runVerifyChecks(ctx context.Context, checks []planner.VerifyCheck, dir string) []string {
	var failures []string
	for i, c := range checks {
		if strings.TrimSpace(c.Run) == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, verifyCheckTimeout)
		cmd := exec.CommandContext(cctx, "sh", "-c", c.Run)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()

		exit := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else {
				// Couldn't start (bad command, timeout, dir missing).
				// That's a failed check, not a pass.
				failures = append(failures, fmt.Sprintf(
					"verify check %d (`%s`): could not run: %v", i+1, truncForReason(c.Run, 80), err))
				continue
			}
		}

		wantExit := 0
		if c.ExpectExit != nil {
			wantExit = *c.ExpectExit
		}
		if exit != wantExit {
			failures = append(failures, verifyFailLine(c, fmt.Sprintf("exited %d, expected %d", exit, wantExit), out))
			continue
		}
		if c.ExpectStdoutContains != "" && !strings.Contains(string(out), c.ExpectStdoutContains) {
			failures = append(failures, verifyFailLine(c, fmt.Sprintf("output did not contain %q", c.ExpectStdoutContains), out))
			continue
		}
	}
	return failures
}

func verifyFailLine(c planner.VerifyCheck, mismatch string, out []byte) string {
	why := ""
	if strings.TrimSpace(c.Why) != "" {
		why = " — " + strings.TrimSpace(c.Why)
	}
	return fmt.Sprintf("verify check failed: `%s`%s: %s\n    actual output: %s",
		truncForReason(c.Run, 80), why, mismatch, truncForReason(string(out), 200))
}

func truncForReason(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
