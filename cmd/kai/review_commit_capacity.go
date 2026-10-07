package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/provider"
)

// Provider retries have already been exhausted. Splitting a capacity failure
// into parallel singleton calls increases demand rather than repairing it.
func rcCapacityFailure(err error) bool {
	if err == nil {
		return false
	}
	if provider.IsCapExceeded(err) {
		return false
	}
	if rcCapacityStatus.MatchString(err.Error()) {
		return true
	}
	return strings.Contains(err.Error(), "in_flight_budget_exhausted") && strings.Contains(err.Error(), "402")
}

var rcCapacityStatus = regexp.MustCompile(`provider(?: \([a-z ]+\))?: 429:`)

var rcRetryAfterBody = regexp.MustCompile(`(?i)Retry-After"\s*:\s*"?(\d+)`)

func rcCapacityDelay(err error) time.Duration {
	delay := time.Minute
	if strings.Contains(err.Error(), "in_flight_budget_exhausted") {
		delay = 2 * time.Minute
	}
	// Provider bodies can contain the upstream Retry-After hint. Never retry
	// sooner than that hint; the caller's deadline bounds the wait.
	if m := rcRetryAfterBody.FindStringSubmatch(err.Error()); m != nil {
		if seconds, e := strconv.Atoi(m[1]); e == nil && seconds > 0 && seconds <= 86400 {
			if hint := time.Duration(seconds) * time.Second; hint > delay {
				delay = hint
			}
		}
	}
	return delay
}

func rcCapacityWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// One shared cooldown followed by at most one serial recovery per failed batch.
// Successful checks are retained; persistent failures remain explicitly unchecked.
func rcRecoverCapacityBatches(ctx context.Context, results []*rcChallengeResult, errs []error, run func(int) (*rcChallengeResult, error), wait func(context.Context, time.Duration) error) {
	delay := time.Duration(0)
	for _, err := range errs {
		if !rcCapacityFailure(err) {
			continue
		}
		d := rcCapacityDelay(err)
		if d > delay {
			delay = d
		}
	}
	if delay == 0 || ctx.Err() != nil {
		return
	}
	// Leave enough time for the recovery to do useful work within the job clock.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay+30*time.Second {
		return
	}
	fmt.Fprintf(os.Stderr, "  challenge: capacity failure; cooling down for %s before serial recovery\n", delay)
	if wait(ctx, delay) != nil {
		return
	}
	for i, err := range errs {
		if !rcCapacityFailure(err) || ctx.Err() != nil {
			continue
		}
		results[i], errs[i] = run(i)
	}
}
