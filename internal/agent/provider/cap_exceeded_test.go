package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/message"
)

// TestKailab_CapExceededIsNotRetried pins the most important
// behavior of the carve-out: a 429 with kai-cap-exceeded:true
// must NOT trip the existing retry loop. Otherwise the user
// would burn 5 attempts (with backoff) waiting for a counter
// that won't move until midnight UTC.
func TestKailab_CapExceededIsNotRetried(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("kai-cap-exceeded", "true")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-kai-daily-cost", "1023")
		w.Header().Set("x-kai-daily-cap", "1000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{
			"error": {
				"type":"daily_cost_cap_exceeded",
				"message":"Daily usage cap reached ($10.00). Resets at midnight UTC.",
				"daily_cost_usd":10.23,
				"daily_cap_usd":10.00,
				"resets_at":"2026-05-05T00:00:00Z",
				"pricing_note":null,
				"byom_hint":"Get a key at https://console.anthropic.com/, set ANTHROPIC_API_KEY, then run KAI_PROVIDER=anthropic kai"
			}
		}`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	k.InitialBackoff = time.Millisecond // shorten in case the test fails
	k.MaxAttempts = 5

	_, err := k.Send(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		}},
	})
	if err == nil {
		t.Fatal("expected cap-exceeded error, got nil")
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt (no retry), got %d", attempts)
	}
	if !IsCapExceeded(err) {
		t.Fatalf("expected CapExceededError, got %T: %v", err, err)
	}

	ce, ok := AsCapExceeded(err)
	if !ok {
		t.Fatal("AsCapExceeded should succeed when IsCapExceeded does")
	}
	if ce.DailyCostUSD != 10.23 || ce.DailyCapUSD != 10.00 {
		t.Errorf("amounts wrong: %+v", ce)
	}
	if !strings.Contains(ce.BYOMHint, "ANTHROPIC_API_KEY") {
		t.Errorf("BYOM hint missing key reference: %q", ce.BYOMHint)
	}
	want := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	if !ce.ResetsAt.Equal(want) {
		t.Errorf("ResetsAt: got %v want %v", ce.ResetsAt, want)
	}
}

// TestKailab_OrdinaryRateLimitStillRetries confirms we didn't
// accidentally turn off retry for the regular 429 case (Anthropic
// throttling, no cap involved). Without the kai-cap-exceeded
// header, the existing transient-retry path must still apply.
func TestKailab_OrdinaryRateLimitStillRetries(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusTooManyRequests) // no kai-cap-exceeded header
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	k := NewKailab(srv.URL, "tok")
	k.InitialBackoff = time.Millisecond
	k.MaxBackoff = 5 * time.Millisecond

	resp, err := k.Send(context.Background(), Request{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 100,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("expected eventual success, got err: %v", err)
	}
	if attempts < 2 {
		t.Errorf("expected retry, only %d attempts", attempts)
	}
	if IsCapExceeded(err) {
		t.Errorf("plain 429 should not parse as CapExceeded")
	}
	_ = resp
}

// TestParseCapExceeded_MalformedBodyStillReturnsError confirms
// best-effort parsing: kailab might (in the future) ship slightly
// different JSON, but the kai-cap-exceeded header is the
// contractual signal — we should still produce a typed error
// the runner won't retry, even on a body we couldn't fully parse.
func TestParseCapExceeded_MalformedBodyStillReturnsError(t *testing.T) {
	ce := parseCapExceeded([]byte("not json"))
	if ce == nil {
		t.Fatal("parseCapExceeded must always return a non-nil error")
	}
	if ce.Message == "" {
		t.Errorf("expected fallback Message text, got empty")
	}
}

// TestFormatHumanMessage_IncludesCriticalFacts pins the multi-
// line trailer text the TUI shows when a request is blocked.
// We assert on the substantive content (amount, BYOM hint) not
// on exact whitespace so prose tweaks don't flap the test.
func TestFormatHumanMessage_IncludesCriticalFacts(t *testing.T) {
	ce := &CapExceededError{
		DailyCostUSD: 10.23,
		DailyCapUSD:  10.00,
		ResetsAt:     time.Now().Add(4*time.Hour + 23*time.Minute),
		BYOMHint:     "Set ANTHROPIC_API_KEY and run KAI_PROVIDER=anthropic kai",
	}
	msg := ce.FormatHumanMessage()
	for _, want := range []string{"10.23", "10.00", "ANTHROPIC_API_KEY", "midnight UTC"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rendered message missing %q:\n%s", want, msg)
		}
	}
}

// TestFormatHumanMessage_PricingNoteSurfaced makes sure the
// fallback-pricing case ("we charged you at Opus rates because
// we don't have your model in the table") reaches the user
// instead of being a silent why-did-my-cap-blow-up mystery.
func TestFormatHumanMessage_PricingNoteSurfaced(t *testing.T) {
	ce := &CapExceededError{
		Message:     "Daily usage cap reached ($10.00). Resets at midnight UTC.",
		PricingNote: "model priced at fallback rate (most expensive known model)",
	}
	msg := ce.FormatHumanMessage()
	if !strings.Contains(msg, "fallback rate") {
		t.Errorf("pricing note not surfaced:\n%s", msg)
	}
}

// silence unused import lint warnings if errors is dropped later.
var _ = errors.New
var _ = fmt.Sprintf
