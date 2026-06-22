package views

import (
	"strings"
	"testing"
)

func TestParseCriticOutput_Pass(t *testing.T) {
	text := `CRITIQUE: The agent correctly updated the version string and the changelog. Both files exist and contain the expected content.
VERDICT: PASS`
	critique, pass, hint := parseCriticOutput(text)
	if !pass {
		t.Errorf("expected pass=true, got false")
	}
	if critique == "" {
		t.Errorf("expected critique to be parsed")
	}
	if hint != "" {
		t.Errorf("expected no hint on pass, got %q", hint)
	}
}

func TestParseCriticOutput_Fail(t *testing.T) {
	text := `CRITIQUE: The agent created a CSS file but never imported it from index.html, and the values don't match the source theme.css.
VERDICT: FAIL
RETRY_HINT: Read both source and target theme files first, then update index.html to import the new file.`
	critique, pass, hint := parseCriticOutput(text)
	if pass {
		t.Errorf("expected pass=false, got true")
	}
	if critique == "" {
		t.Errorf("expected critique to be parsed")
	}
	if hint == "" {
		t.Errorf("expected retry hint to be parsed on fail")
	}
}

func TestParseCriticOutput_Malformed_DefaultsToPass(t *testing.T) {
	// Critic output that doesn't match the expected shape (no
	// VERDICT line) should default to pass — better to miss a
	// real FAIL than to flag a false one and erode trust.
	text := "The agent did fine. Looks good."
	_, pass, _ := parseCriticOutput(text)
	if !pass {
		t.Errorf("malformed output should default to pass=true, got false")
	}
}

func TestPendingCriticRetry_RetryPrompt(t *testing.T) {
	p := &pendingCriticRetry{
		originalRequest: "Apply the design from A to B",
		critique:        "Only copied tokens, missed component structure.",
		retryHint:       "Read both top-level files first.",
	}
	got := p.retryPrompt()
	for _, want := range []string{
		"Apply the design from A to B",
		"Prior attempt critique",
		"Only copied tokens",
		"Concrete next step",
		"Read both top-level files first",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("retryPrompt missing %q in:\n%s", want, got)
		}
	}
}
