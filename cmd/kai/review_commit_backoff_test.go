package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const (
	rcTempPaymentErr  = "upstream: HTTP 402 Request held back while in-flight requests settle; retry after they finish"
	rcRealOutOfCredit = "upstream: HTTP 402 Insufficient credits. Add more using https://openrouter.ai/settings/credits"
)

func TestTemporaryPaymentRequiredClassifier(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want bool
	}{
		{rcTempPaymentErr, true},
		{"challenge call: " + rcTempPaymentErr, true},
		{rcRealOutOfCredit, false},
		// A message that names both is out of credit: the balance wins.
		{"upstream: HTTP 402 insufficient credits while in-flight requests settle", false},
		{"upstream: HTTP 402", false},
		{"upstream: HTTP 429 in-flight limit, retry after 3s", false},
		{"upstream: HTTP 500 in-flight requests settle", false},
	} {
		if got := rcTemporaryPaymentRequired(errors.New(tc.err)); got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.err, got, tc.want)
		}
	}
	if rcTemporaryPaymentRequired(nil) {
		t.Error("nil error classified as temporary")
	}
}

func TestBackoffDelayGrowsAndIsCapped(t *testing.T) {
	for attempt, wantMax := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		for i := 0; i < 50; i++ {
			d := rcBackoffDelay(attempt)
			if d < wantMax/2 || d > wantMax {
				t.Fatalf("attempt %d: delay %s outside [%s, %s]", attempt, d, wantMax/2, wantMax)
			}
		}
	}
}

// noSleep records the waits instead of making them.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	old := rcBackoffSleep
	rcBackoffSleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { rcBackoffSleep = old })
	return &waits
}

func rcOneIssueChallengeAnswer(t *testing.T) string {
	a := rcCDChecks()
	a.Checks = a.Checks[:1]
	return rcTestAnswer(t, a)
}

// The fake provider turns the request away with the temporary 402 twice, then
// answers: the allegation is checked, not lost.
func TestChallengeRetriesTemporary402ThenSucceeds(t *testing.T) {
	waits := noSleep(t)
	var calls int32
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return provider.Response{}, errors.New(rcTempPaymentErr)
		}
		return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcOneIssueChallengeAnswer(t)}}}, nil
	}}
	// One allegation: the batch of one that used to be lost for good.
	res, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), rcCDSources[:1], nil)
	if err != nil || res == nil {
		t.Fatalf("temporary 402 lost the check: %+v %v", res, err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("made %d calls, want 3 (two refused, one answered)", got)
	}
	if len(*waits) != 2 {
		t.Fatalf("waited %d times, want 2: %v", len(*waits), *waits)
	}
}

func TestChallengeDoesNotRetryOutOfCredit402(t *testing.T) {
	waits := noSleep(t)
	var calls int32
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		atomic.AddInt32(&calls, 1)
		return provider.Response{}, errors.New(rcRealOutOfCredit)
	}}
	_, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), rcCDSources[:1], nil)
	if err == nil || !strings.Contains(err.Error(), "Insufficient credits") {
		t.Fatalf("out-of-credit 402 was swallowed: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 || len(*waits) != 0 {
		t.Fatalf("out-of-credit 402 retried: %d calls, waits %v", calls, *waits)
	}
}

func TestChallengeGivesUpAfterBoundedRetries(t *testing.T) {
	noSleep(t)
	var calls int32
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		atomic.AddInt32(&calls, 1)
		return provider.Response{}, errors.New(rcTempPaymentErr)
	}}
	_, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), rcCDSources[:1], nil)
	if err == nil {
		t.Fatal("endless temporary 402 reported success")
	}
	if got := int(atomic.LoadInt32(&calls)); got != rcBackoffRetries+1 {
		t.Fatalf("made %d calls, want %d", got, rcBackoffRetries+1)
	}
}

func TestChallengeStopsRetryingWhenContextEnds(t *testing.T) {
	old := rcBackoffSleep
	rcBackoffSleep = func(ctx context.Context, d time.Duration) error { return context.Canceled }
	t.Cleanup(func() { rcBackoffSleep = old })
	var calls int32
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		atomic.AddInt32(&calls, 1)
		return provider.Response{}, errors.New(rcTempPaymentErr)
	}}
	if _, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcFalseCDIssue), rcCDSources[:1], nil); err == nil {
		t.Fatal("expected the error back")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("kept calling after the wait was cancelled: %d", calls)
	}
}

// Re-checking a failed batch runs at the lower concurrency, so the retry does
// not add load just when the provider asked us to ease off.
func TestRetryOfFailedBatchRunsAtLowerConcurrency(t *testing.T) {
	waits := noSleep(t)
	var cur, peak int32
	p := rcChallengeProvider{send: func(ctx context.Context, req provider.Request) (provider.Response, error) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return provider.Response{}, errors.New("provider failed")
	}}
	issues := []string{"- a", "- b", "- c", "- d", "- e", "- f"}
	batches := [][]string{issues}
	errs := []error{errors.New(rcTempPaymentErr)}
	rcRetryFailedBatches(context.Background(), p, "test", "prose", nil, batches, make([]*rcChallengeResult, 1), errs, rcCDSources, nil, nil)
	if got := atomic.LoadInt32(&peak); got > int32(rcChallengeRetryParallel) || got < 1 {
		t.Fatalf("peak concurrency %d, want 1..%d", got, rcChallengeRetryParallel)
	}
	if len(*waits) == 0 {
		t.Fatal("no pause before re-checking after a temporary 402")
	}
}
