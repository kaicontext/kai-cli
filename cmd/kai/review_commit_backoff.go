package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/provider"
)

// A 402 from the provider has two meanings, and only one of them is final.
//
// An account that is out of credit answers 402 until someone pays, so
// retrying it is futile and must stay a permanent failure. A provider that
// holds back a request while others are still in flight also answers 402; that
// clears in seconds. Before this, every 402 was labelled no_credit and never
// retried, so a burst of parallel fact-checks lost the allegations it could not
// place: in run 50, 243 of 1,223 allegations across 17 PRs were never checked,
// three of them for the critical cal.com#14740 admin-check bug.
//
// OpenRouter documents the temporary case (openrouter.ai/docs/api-reference/limits):
// a prepaid account whose balance is low has an in-flight spend budget, and a
// request that would exceed it is turned away with 402 even though the balance
// is positive. The message reads "This request would exceed your available
// credits given your current in-flight requests", the error metadata carries
// reason "in_flight_budget_exhausted" and limit_source "openrouter_in_flight_budget",
// and the response has a Retry-After header. The same status also means a
// request too large for the balance (reason "weight_exceeds_budget") and a
// balance that is really empty ("insufficient credits"); neither clears by waiting.
//
// kai-engine words the error two ways: a streamed call arrives as
// "upstream: HTTP 402 <message>" with the body reduced to its message, and a plain
// call as "<name> provider: 402: <raw body>" (kailab.go, openrouter.go). Headers
// are dropped either way. So the message is what we can match, and the metadata
// strings when the raw body passes through. A 402 is temporary only when that
// text names the in-flight budget and not a balance or size problem. Anything
// unrecognised stays permanent, which is the behaviour before this change.

var rcPaymentRequiredRe = regexp.MustCompile(`\b(?:http|status|provider)\s*:?\s*402\b|\b402\b[^\n]{0,20}payment required`)

// rcTemporaryMarkers say the 402 is about requests in flight, not the balance.
var rcTemporaryMarkers = []string{"in-flight requests", "in_flight_budget_exhausted", "openrouter_in_flight_budget"}

// rcOutOfCreditMarkers say the balance or the request size is the problem.
// They win over any temporary marker in the same message.
var rcOutOfCreditMarkers = []string{"insufficient credit", "add more credit", "weight_exceeds_budget", "openrouter_credits", "openrouter_key_limit", "reduce max_tokens", "lower max_tokens"}

// rcTemporaryPaymentRequired reports whether err is a 402 the provider says
// will clear by itself.
func rcTemporaryPaymentRequired(err error) bool {
	if err == nil || provider.IsCapExceeded(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !rcPaymentRequiredRe.MatchString(msg) {
		return false
	}
	for _, m := range rcOutOfCreditMarkers {
		if strings.Contains(msg, m) {
			return false
		}
	}
	for _, m := range rcTemporaryMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

var (
	// rcBackoffBase and rcBackoffMax bound the wait before a retry; the wait
	// doubles each attempt and is jittered so parallel checks do not all come
	// back in the same instant, which is the load that caused the 402.
	rcBackoffBase = 2 * time.Second
	rcBackoffMax  = 30 * time.Second
	// rcBackoffRetries is how many times one call is repeated after a temporary 402.
	rcBackoffRetries = 4
	// rcChallengeRetryParallel is the concurrency for re-checking failed batches,
	// lower than the first pass because the provider has just asked us to ease off.
	rcChallengeRetryParallel = 2
	// rcBackoffSleep waits d or until ctx ends. Tests replace it.
	rcBackoffSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// rcBackoffDelay is the wait before retry number attempt (0-based): half to
// full of base*2^attempt, capped at rcBackoffMax.
func rcBackoffDelay(attempt int) time.Duration {
	d := rcBackoffBase << uint(attempt)
	if d <= 0 || d > rcBackoffMax {
		d = rcBackoffMax
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// rcChallengeDraft checks a draft's allegations. A temporary 402 is retried
// with backoff, a single allegation included, and every other failure comes
// back as it is.
func rcChallengeDraft(ctx context.Context, prov provider.Provider, model, draft string, sources []rcSource, sandbox *rcShellSandbox) (*rcChallengeResult, error) {
	for attempt := 0; ; attempt++ {
		res, err := rcChallengeDraftOnce(ctx, prov, model, draft, sources, sandbox)
		if err == nil || attempt >= rcBackoffRetries || ctx.Err() != nil || !rcTemporaryPaymentRequired(err) {
			return res, err
		}
		wait := rcBackoffDelay(attempt)
		fmt.Fprintf(os.Stderr, "  challenge: the provider asked us to wait (%v); retrying in %s (%d of %d)\n", err, wait.Round(time.Millisecond), attempt+1, rcBackoffRetries)
		if sleepErr := rcBackoffSleep(ctx, wait); sleepErr != nil {
			return res, err
		}
	}
}
