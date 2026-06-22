package errors

import (
	"context"
	stderr "errors"
	"fmt"
	"strings"
	"testing"

	"kai/api/provider"
)

// TestClassify_Patterns pins each known pattern → expected
// kind + severity. New rules in classifyKnown must add a row
// here. This is the regression net for the most important
// invariant of the package: raw error text from upstream is
// never the source of the user-facing message — the kind is.
func TestClassify_Patterns(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantKind    string
		wantSev     Severity
		wantsRepair bool
	}{
		{
			name:     "missing blobs auto-repairs",
			err:      stderr.New("orchestrator preflight: object store is missing blobs the snapshot references — run kai capture to rebuild"),
			wantKind: "preflight.missing_blobs",
			wantSev:  Info,
		},
		{
			name:     "sqlite lock contention",
			err:      stderr.New("database is locked"),
			wantKind: "sqlite.locked",
			wantSev:  Info,
		},
		{
			name:     "auth expired (401)",
			err:      stderr.New("kailab provider: 401: unauthorized"),
			wantKind: "auth.expired",
			wantSev:  Block,
		},
		{
			name:     "auth expired (token invalid string)",
			err:      stderr.New("token invalid or expired"),
			wantKind: "auth.expired",
			wantSev:  Block,
		},
		{
			name:     "model not found",
			err:      stderr.New(`openai provider: 404: {"error":{"message":"The model x does not exist or you do not have access to it"}}`),
			wantKind: "api.model_not_found",
			wantSev:  Block,
		},
		{
			name:     "local context too small (LM Studio)",
			err:      stderr.New("openai provider: : The number of tokens to keep from the initial prompt is greater than the context length"),
			wantKind: "local.context_too_small",
			wantSev:  Block,
		},
		{
			name:     "unknown error falls through to internal.unknown",
			err:      stderr.New("some weird new failure mode the classifier hasn't seen"),
			wantKind: "internal.unknown",
			wantSev:  Block,
		},
		{
			// Real-world cancellation string from the kailab
			// provider (Esc / Ctrl+C / queued-item replace).
			// Used to render as internal.unknown with the scary
			// "Something unexpected happened" + /copy 4 + kai
			// diagnose framing. Now classifies to user.cancelled
			// with Info severity.
			name:     "kailab stream cancellation",
			err:      stderr.New("planner: agent run: kailab provider: stream read: context canceled"),
			wantKind: "user.cancelled",
			wantSev:  Info,
		},
		{
			name:     "bare context.Canceled value",
			err:      context.Canceled,
			wantKind: "user.cancelled",
			wantSev:  Info,
		},
		{
			name:     "wrapped context.Canceled (fmt.Errorf %w)",
			err:      fmt.Errorf("agent: %w", context.Canceled),
			wantKind: "user.cancelled",
			wantSev:  Info,
		},
		{
			name:     "nil error → none",
			err:      nil,
			wantKind: "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if tc.wantKind != "none" && got.Severity != tc.wantSev {
				t.Errorf("severity = %v, want %v", got.Severity, tc.wantSev)
			}
		})
	}
}

// TestClassify_NeverLeaksRawIntoHeadline is the critical
// invariant: the user-facing Headline must NEVER contain raw
// error text. This is the whole point of the classifier — we
// translate gnarly upstream errors into friendly headlines.
// If a future rule accidentally puts err.Error() into Headline,
// this test fails.
func TestClassify_NeverLeaksRawIntoHeadline(t *testing.T) {
	rawSnippets := []string{
		"object store is missing blobs the snapshot references",
		"database is locked",
		"401",
		"openai provider:",
		"tokens to keep from the initial prompt",
	}
	rawErr := stderr.New("a wild raw error appears: " + strings.Join(rawSnippets, " // "))
	ue := Classify(rawErr)
	for _, snip := range rawSnippets {
		if strings.Contains(ue.Headline, snip) {
			t.Errorf("Headline %q leaked raw snippet %q — user should NEVER see raw error text",
				ue.Headline, snip)
		}
	}
	// LogContext, however, MUST contain the raw text — that's
	// what gets logged + reported to telemetry for triage.
	if !strings.Contains(ue.LogContext, "wild raw error") {
		t.Errorf("LogContext should preserve raw error for telemetry; got %q", ue.LogContext)
	}
}

// TestClassify_CapExceededRoutesToTypedError ensures the typed
// CapExceededError from the provider package is handled
// specifically (not via string matching). Future field additions
// on CapExceededError should flow through without needing a
// classify rule update.
func TestClassify_CapExceededRoutesToTypedError(t *testing.T) {
	ce := &provider.CapExceededError{
		Message:      "Daily usage cap reached ($10.00)",
		BYOMHint:     "Set ANTHROPIC_API_KEY and run KAI_PROVIDER=anthropic kai",
		DailyCostUSD: 10.23,
		DailyCapUSD:  10.00,
	}
	ue := Classify(ce)
	if ue.Kind != "api.cap_exceeded" {
		t.Errorf("kind = %q, want api.cap_exceeded", ue.Kind)
	}
	if !strings.Contains(ue.Headline, "10.00") {
		t.Errorf("headline should carry cap message: %q", ue.Headline)
	}
	if !strings.Contains(ue.Detail, "ANTHROPIC_API_KEY") {
		t.Errorf("detail should carry BYOM hint: %q", ue.Detail)
	}
}

// TestTag_RoundTrip pins the tagged-error escape hatch: when
// upstream knows the error kind better than the classifier
// could (orchestrator preflight, etc.), it can Tag() the error
// and have IsKind() match later. Used sparingly but useful for
// owned error sites.
func TestTag_RoundTrip(t *testing.T) {
	wrapped := Tag("preflight.missing_blobs", stderr.New("raw"))
	if !IsKind(wrapped, "preflight.missing_blobs") {
		t.Error("IsKind should match the tag we just attached")
	}
	if IsKind(wrapped, "different.kind") {
		t.Error("IsKind must not match a different kind")
	}
	if IsKind(stderr.New("untagged"), "preflight.missing_blobs") {
		t.Error("IsKind must not match an untagged error")
	}
}

// TestClassify_NoSnapshotsCovered pins the May-2026 fix:
// before this, "resolving --from \"@snap:last\": not found:
// @snap:last~0" fell through to the generic
// "Something unexpected happened" fallback (visible in
// errors.log entries from the day this was filed). The
// classifier must turn it into the actionable
// preflight.no_snapshots kind so the user sees
// "No snapshots in this project yet — run kai capture".
func TestClassify_NoSnapshotsCovered(t *testing.T) {
	cases := []string{
		`orchestrator preflight: spawn check failed: exit status 1
Error: resolving --from "@snap:last": not found: @snap:last~0`,
		`no snapshots in DB`,
		`not found: @snap:last`,
	}
	for _, raw := range cases {
		ue := Classify(errStub(raw))
		if ue.Kind != "preflight.no_snapshots" {
			t.Errorf("expected preflight.no_snapshots for %q, got %s", raw, ue.Kind)
		}
		// No user-facing Action: auto_repair.go runs `kai capture`
		// in the background for this kind, so telling the user to
		// run it themselves would race the auto-repair.
		if ue.Action != "" {
			t.Errorf("expected empty action (auto-repaired), got %q", ue.Action)
		}
		if ue.Detail == "" {
			t.Errorf("expected non-empty Detail for transient line, got empty")
		}
	}
}

// TestClassify_NoRemoteCovered pins the matching rule for
// "--sync full requires a remote" — same fallback bug,
// same user pain. Errors.log on May-2026 had three of these
// in a row before this rule landed.
func TestClassify_NoRemoteCovered(t *testing.T) {
	raw := `orchestrator preflight: spawn check failed: exit status 1
Error: --sync full requires a remote; run ` + "`kai remote set origin <url>`" + ` first or pass --sync none`
	ue := Classify(errStub(raw))
	if ue.Kind != "preflight.no_remote" {
		t.Errorf("expected preflight.no_remote, got %s", ue.Kind)
	}
}

// TestClassify_MultiRootDivergence pins that the alignment-guard
// error from internal/orchestrator (cwd's kai dir != db handle's
// kai dir) gets a typed UserError with the actionable hint as the
// Action — not buried under "Something unexpected happened" with
// the body in `details`. Confirmed May 2026 against the user's
// session where the guard fired but the friendly hint was hidden.
func TestClassify_MultiRootDivergence(t *testing.T) {
	raw := `orchestrator: working directory and graph DB don't agree on which kai project to use
  cwd resolves kai dir to: /Users/x/projects/kai/.kai
  db handle is opened at:  /Users/x/projects/kai/inner/.kai

This usually means your multi-root config picked a different project as primary than the directory you're running from. Two ways to fix:
  1. cd into the project that owns the populated DB (the one listed under "db handle" above), or
  2. mark the directory you want as primary by adding ` + "`pinned: true`" + ` to its entry in kai.projects.yaml.`
	ue := Classify(errStub(raw))
	if ue.Kind != "config.multiroot_divergence" {
		t.Fatalf("expected config.multiroot_divergence, got %q", ue.Kind)
	}
	if !strings.Contains(ue.Headline, "don't agree") {
		t.Errorf("headline should be the first line of the orchestrator message, got %q", ue.Headline)
	}
	if !strings.Contains(ue.Action, "pinned: true") {
		t.Errorf("Action should carry the fix hint; got %q", ue.Action)
	}
	if !strings.Contains(ue.Action, "/Users/x/projects/kai/inner/.kai") {
		t.Errorf("Action should preserve both paths so the user can act; got %q", ue.Action)
	}
	if ue.Severity != Block {
		t.Errorf("expected Block severity (run halts), got %v", ue.Severity)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
