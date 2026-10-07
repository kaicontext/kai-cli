package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kaicontext/kai-engine/finding"
	"os"
	"strings"
	"testing"
	"time"
)

// Actual run25 allegation: the checker corrected the mechanism, but the
// machine coda used by the renderer retained a different original claim.
func TestPublicationSafeParseWrapperRegression(t *testing.T) {
	raw, err := os.ReadFile("testdata/benchmark-losses/safeparse-wrapper.json")
	if err != nil {
		t.Fatal(err)
	}
	var a rcAllegationResult
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	review := rcAssembleReview(nil, nil, []rcAllegationResult{a}, nil, finding.MatchVerified, 2, "one finding")
	_, issues, _, _, _, _ := rcParseReviewOutput(review)
	if len(issues) != 1 || !strings.Contains(issues[0], "Google stores that wrapper object") {
		t.Fatalf("corrected mechanism missing: %v", issues)
	}
	path, line, ok := rcIssueLocation(issues[0])
	if !ok || path != "packages/app-store/googlecalendar/lib/CalendarService.ts" || line != 80 {
		t.Fatalf("location lost: %v", issues)
	}
	if strings.Contains(issues[0], "accumulated credential object") {
		t.Fatalf("old mechanism published: %v", issues)
	}
	if a.Issue == issues[0] {
		t.Fatal("fixture did not exercise a corrected allegation")
	}
}

func TestCapacityRecoveryRetainsSuccessfulChecks(t *testing.T) {
	success := &rcChallengeResult{}
	results := []*rcChallengeResult{success, nil, nil, nil}
	errs := []error{nil, errors.New("kailab provider (openai): 429: slow down"), errors.New(`kailab provider: 402: {"reason":"in_flight_budget_exhausted"}`), errors.New("provider: 402: payment required")}
	var calls []int
	recovered := &rcChallengeResult{}
	rcRecoverCapacityBatches(context.Background(), results, errs, func(i int) (*rcChallengeResult, error) { calls = append(calls, i); return recovered, nil }, func(ctx context.Context, d time.Duration) error {
		if d != 2*time.Minute {
			t.Fatalf("cooldown %s", d)
		}
		return nil
	})
	if len(calls) != 2 || calls[0] != 1 || calls[1] != 2 || results[0] != success || errs[3] == nil {
		t.Fatalf("calls=%v results=%v errors=%v", calls, results, errs)
	}
}

func TestCapacityRecoveryCancellationAndDeadline(t *testing.T) {
	for _, shortDeadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if shortDeadline {
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		} else {
			cancel()
		}
		rcRecoverCapacityBatches(ctx, []*rcChallengeResult{nil}, []error{errors.New("kailab provider: 429: retry")}, func(int) (*rcChallengeResult, error) { t.Fatal("retry after cancellation/deadline"); return nil, nil }, func(context.Context, time.Duration) error { t.Fatal("unnecessary wait"); return nil })
		cancel()
	}
}

func TestFailedChecksAreMachineReadable(t *testing.T) {
	res, err := rcMergeBatches([][]string{{"ok.go:1 — real"}, {"bad.go:2 — unchecked"}}, nil, []*rcChallengeResult{{Allegations: []rcAllegationResult{{Issue: "ok.go:1 — real", Status: rcStatusSupported, Finding: "real"}}, match: finding.MatchVerified, proposed: 2}, nil}, []error{nil, errors.New("rate limited")})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !res.VerificationIncomplete || res.FailedChecks != 1 || !strings.Contains(string(raw), `"unchecked":true`) {
		t.Fatalf("missing failed-check metadata: %s", raw)
	}
	settled := &rcChallengeResult{Allegations: []rcAllegationResult{{Status: rcStatusUnresolved, Reason: "external evidence missing"}}, match: finding.MatchVerified, proposed: 2}
	rcFinalize(settled)
	if settled.VerificationIncomplete {
		t.Fatal("semantic uncertainty misclassified as operational failure")
	}
}

func TestCapacityRecoveryHonorsHintAndStopsAfterOneAttempt(t *testing.T) {
	err := errors.New(`kailab provider: 429: {"headers":{"Retry-After":"180"}}`)
	if got := rcCapacityDelay(err); got != 3*time.Minute {
		t.Fatalf("hint ignored: %s", got)
	}
	results := []*rcChallengeResult{nil}
	errs := []error{err}
	calls := 0
	rcRecoverCapacityBatches(context.Background(), results, errs, func(int) (*rcChallengeResult, error) { calls++; return nil, err }, func(context.Context, time.Duration) error { return nil })
	if calls != 1 || errs[0] != err {
		t.Fatalf("unbounded retry or lost error: %d %v", calls, errs)
	}
}
