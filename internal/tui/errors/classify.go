// Package errors is the central chokepoint for every error that
// would otherwise reach the user via the TUI.
//
// The architecture: every TUI render-an-error site converts the
// raw Go error to a UserError via Classify(), then the renderer
// shows ONLY the UserError's user-facing fields. The raw error
// is logged to .kai/errors.log + reported to telemetry, never
// to the screen.
//
// This exists because raw errors leak implementation details
// users can't act on ("object store is missing blobs the
// snapshot references"), don't tell them what to do, and don't
// give the kai team visibility into how often each failure
// happens. Routing through one classifier solves all three.
//
// Adding a new error pattern: append to the rules slice in
// classifyKnown(). Each rule is a (matcher, handler) pair. The
// matcher decides if this rule fires; the handler returns the
// UserError fields. Rules are tried in order; first match wins.
package errors

import (
	"context"
	stderr "errors"
	"strings"

	"kai/api/provider"
	"kai/api/planner"
)

// Severity drives the TUI's render style. Info is dim; Warn is
// yellow; Block is red and means "the requested action did not
// happen and won't happen until the user does something."
type Severity int

const (
	Info Severity = iota
	Warn
	Block
)

// UserError is what the renderer sees. The classifier translates
// a raw Go error into one of these. The raw form is preserved in
// LogContext for the local log + telemetry, never shown to the
// user directly.
type UserError struct {
	// Kind is the stable taxonomy id (e.g. "preflight.missing_blobs",
	// "api.cap_exceeded", "internal.unknown"). Used for
	// telemetry grouping and for the registry of known patterns.
	// New kinds get added by appending to the rules slice.
	Kind string

	// Headline is the one-line lede the user reads first. Friendly,
	// action-implying language ("Snapshot needs rebuilding", not
	// "object store is missing blobs"). Avoid jargon.
	Headline string

	// Detail is an optional second line with concrete context
	// ("Reindexing 247 files…"). Empty when the headline is enough.
	Detail string

	// Action is the next step the user could take, when applicable.
	// Often empty for self-healed errors. Examples:
	//   "Run `kai auth login` to refresh your session."
	//   "This is unusual — `kai diagnose` will help us fix it."
	Action string

	// Severity controls the render style.
	Severity Severity

	// AutoRepair, when non-nil, is run by the dispatcher to attempt
	// self-healing. The classifier picks it; the dispatcher decides
	// whether to invoke it (some sites can retry; others can't).
	// AutoRepair must be idempotent and bounded — the runner won't
	// add a timeout for you.
	AutoRepair func() error

	// LogContext is the raw error message + any structured
	// breadcrumbs for the local log and telemetry. NEVER shown to
	// the user — that's the whole point of the classifier.
	LogContext string

	// Context carries additional structured fields for telemetry
	// (mode, tool name, turn number, etc.). Whitelisted to avoid
	// accidentally shipping PII.
	Context map[string]any
}

// Classify converts a raw error into a UserError.
//
// Always returns a non-nil UserError, even for nil input (a
// "no error" UserError so callers don't have to nil-check both
// the error AND the result). Wraps unknown errors in the
// Internal handler so nothing bypasses the classifier.
func Classify(err error) UserError {
	if err == nil {
		return UserError{Kind: "none"}
	}
	if ue, ok := classifyKnown(err); ok {
		ue.LogContext = err.Error()
		return ue
	}
	return UserError{
		Kind:     "internal.unknown",
		Headline: "Something unexpected happened",
		// /copy 4 ships the last few scrollback blocks to clipboard
		// so the user can paste them into a bug report. (Earlier
		// copy mentioned a `kai diagnose` companion command; that
		// command never landed and the suggestion is stripped.)
		Action:   "Type `/copy 4` to put the last few blocks on your clipboard and share them in a bug report.",
		Severity: Block,
		// LogContext carries the raw form for the local log AND
		// PostHog. Never shown to the user via the renderer; the
		// renderer only reads the headline/detail/action.
		LogContext: err.Error(),
	}
}

// classifyKnown walks the rules in order and returns the first
// match. Rules are intentionally hardcoded (not a registration
// system) — the cost of "where do I add a new pattern" is one
// case in this file, vs. the cost of a plugin abstraction is
// debugging which init order registered which handler.
func classifyKnown(err error) (UserError, bool) {
	// User-initiated cancellation. The TUI's Esc / Ctrl+C / queued-
	// item-replace paths all trip the run's context, which surfaces
	// here as context.Canceled (often wrapped by the provider —
	// "kailab provider: stream read: context canceled"). Without
	// this rule it falls through to internal.unknown, which
	// renders as "Something unexpected happened" with the scary
	// /copy 4 + kai diagnose framing. Cancellation isn't
	// unexpected — it's exactly what the user asked for.
	//
	// Match both the typed error (errors.Is) and the literal
	// string ("context canceled") because the kailab provider
	// returns a plain fmt.Errorf with the message rather than
	// wrapping context.Canceled, so errors.Is alone misses some
	// callers. Severity Info so the TUI renders it dim, not red.
	if stderr.Is(err, context.Canceled) ||
		strings.Contains(strings.ToLower(err.Error()), "context canceled") {
		return UserError{
			Kind:     "user.cancelled",
			Headline: "Cancelled",
			Severity: Info,
		}, true
	}

	// Run hit its wall-clock budget. The chat/agent run wraps itself in
	// context.WithTimeout (5 min for the default model, 15 for a
	// reasoning model); a slow or stuck step exhausts it and surfaces as
	// context.DeadlineExceeded — often wrapped by the provider as a
	// plain string ("…: context deadline exceeded"), so match the string
	// too. Without this it falls through to internal.unknown ("Something
	// unexpected happened / file a bug report"), which is wrong and
	// alarming: a timeout is a recoverable budget limit, not a crash.
	// (Common trigger before kit 0.33.63: a first-use kai_search index
	// backfill that ran for minutes and ate the whole budget.)
	if stderr.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return UserError{
			Kind:     "run.timeout",
			Headline: "Run hit its time budget",
			Detail:   "The agent ran out of wall-clock time mid-task (5 min for the default model, 15 for a reasoning model) — usually a slow or stuck step, not a crash.",
			Action:   "Re-ask in the same session to continue, or narrow the request. If a first-use workspace search/index was the slow step, the next run is faster (the index is cached).",
			Severity: Warn,
		}, true
	}

	// Provider cap-exceeded is already a typed error; check for
	// it explicitly so future field additions on CapExceededError
	// flow through without changing classifyKnown.
	// Planner couldn't extract a concrete target from the request
	// and the dispatcher's chat fallback didn't intercept (either
	// the fallback itself errored, or a caller bypassed the
	// fallback path). Surface a friendly nudge instead of leaking
	// the raw "planner: request too vague…" string as
	// "Something unexpected happened" via internal.unknown.
	if stderr.Is(err, planner.ErrTooVague) {
		return UserError{
			Kind:     "planner.too_vague",
			Headline: "Couldn't tell what to plan",
			Detail:   "Name a file, package, or feature so the planner has a concrete target.",
			Severity: Warn,
		}, true
	}

	if ce, ok := provider.AsCapExceeded(err); ok {
		return UserError{
			Kind:     "api.cap_exceeded",
			Headline: ce.Message,
			Detail:   ce.BYOMHint,
			Severity: Block,
		}, true
	}

	msg := strings.ToLower(err.Error())

	// Build gate: the integrate-time build check found the change broke
	// the build (newly-failing packages vs the pre-run baseline). The
	// orchestrator already composed a clear multi-line message
	// (formatBuildRegressionReason) whose first line is a stable lede we
	// match here. Without this rule it falls through to internal.unknown
	// and renders as "Something unexpected happened / file a bug report"
	// with the real compiler errors buried in LogContext where the user
	// never sees them — the exact 2026-05-29 paper cut. Pass the
	// orchestrator's message through as a real gate Block instead: the
	// headline is the lede, the detail carries the failing packages +
	// compiler excerpt, and the action names the override.
	if strings.Contains(msg, "the change broke the build") {
		raw := err.Error()
		head, rest, _ := strings.Cut(raw, "\n")
		return UserError{
			Kind:     "gate.build_regression",
			Headline: strings.TrimSpace(head),
			Detail:   strings.TrimSpace(rest),
			Action:   "Fix the errors above, or re-run with `KAI_SKIP_BUILD_CHECK=1` to spawn against a broken tree anyway.",
			Severity: Block,
		}, true
	}

	// Generic provider rate-limit error. Kailab passes through
	// upstream rate-limit responses (was the Groq-specific 6K TPM
	// trap until the 2026-05-12 migration; now relevant for any
	// upstream that throttles). Surface it with a /model nudge.
	if strings.Contains(msg, "rate_limit_exceeded") || strings.Contains(msg, "tokens per minute") {
		return UserError{
			Kind:     "api.rate_limit",
			Headline: "Model provider rate limit hit",
			Detail:   "The upstream model provider rejected the request as rate-limited (typically tokens-per-minute on a free tier).",
			Action:   "Switch with `/model kailab qwen/qwen3.5-397b-a17b` (or any other model). The kai-server logs show which upstream returned the limit.",
			Severity: Warn,
		}, true
	}

	// Empty completion from the provider — the model returned zero
	// content and no finish_reason. Seen with qwen/qwen3-32b through
	// kailab: in=0 out=0 cached=0, finish=unknown. Without this rule
	// the user sees "Something unexpected happened" with the /copy 4
	// framing, which is the wrong nudge — the request was fine, the
	// model just produced nothing. Steer them at /model instead.
	if strings.Contains(msg, "assistant returned no text") ||
		strings.Contains(msg, "no assistant message in transcript") {
		return UserError{
			Kind:     "api.empty_completion",
			Headline: "Model returned no text",
			Detail:   "The provider accepted the request but the model produced an empty response. Common with reasoning models (Qwen3 family) when the silent <think> step consumes the full completion budget.",
			Action:   "Try `/model` and pick a different model — deepseek/deepseek-v4-pro or moonshotai/kimi-k2.6 are reliable defaults.",
			Severity: Warn,
		}, true
	}

	// Preflight: not in a kai repo. The user invoked the TUI from
	// a directory without a .kai/ — every spawn would fail with
	// the same message. Surface a single actionable line.
	if strings.Contains(msg, "not in a kai repo") {
		return UserError{
			Kind:     "preflight.no_kai_repo",
			Headline: "This directory isn't a kai repo",
			Action:   "Run `kai init` here, or `cd` to a directory that already has a `.kai/`.",
			Severity: Block,
		}, true
	}

	// Preflight: no snapshots in the DB. spawn requires a
	// snapshot to base the workspace on; without one its
	// resolver fails with "not found: @snap:last~0" (the ~0
	// is the resolver's canonical form for "0th most recent
	// snapshot"). Phrasing here doesn't expose that internal
	// detail to the user.
	if strings.Contains(msg, "no snapshots") ||
		strings.Contains(msg, "no snapshot to spawn from") ||
		strings.Contains(msg, `not found: @snap:last`) ||
		strings.Contains(msg, `resolving --from "@snap:last"`) {
		// Auto-repaired: auto_repair.go runs `kai capture` in the
		// background for this kind. Surface as Info with a Detail
		// the dispatcher can pin into the transient line, and no
		// Action — telling the user to "run `kai capture`" while
		// we're already running it produced the May-6 dogfood
		// confusion ("it told me to capture but it was already
		// capturing").
		return UserError{
			Kind:     "preflight.no_snapshots",
			Headline: "Creating the first snapshot",
			Detail:   "Capturing the workspace…",
			Severity: Info,
		}, true
	}

	// Preflight: --sync full requires a remote. Triggered when
	// the user runs kai code in a project that hasn't been
	// linked to a kailab remote. Hint reproduces the exact
	// command the underlying spawn error already suggests, so
	// the user doesn't have to context-switch to remember it.
	if strings.Contains(msg, "requires a remote") ||
		strings.Contains(msg, "remote set origin") {
		return UserError{
			Kind:     "preflight.no_remote",
			Headline: "No remote configured for sync",
			Action:   "Run `kai remote set origin <url>` to link this project, or pass `--sync none`.",
			Severity: Block,
		}, true
	}

	// Preflight: object store missing blobs the snapshot
	// references. Auto-repairable via `kai capture` which
	// rebuilds the missing references from the working tree.
	// The user's May-5 frustration: "ok we can't surface that
	// to the user" — this rule is the answer.
	if strings.Contains(msg, "missing blobs") || strings.Contains(msg, "missing snapshot blobs") {
		return UserError{
			Kind:     "preflight.missing_blobs",
			Headline: "Snapshot needs rebuilding",
			Detail:   "Reindexing the workspace…",
			Severity: Info,
		}, true
	}

	// Multi-root divergence guard. orchestrator.checkRepoDBAlignment
	// fires when cwd and the primary project's DB point at different
	// .kai/ directories — running here would silently target two
	// different stores. The orchestrator already crafted a friendly
	// multi-line message with both paths and two fix paths; we
	// recognize the prefix and pass it through verbatim instead of
	// dumping under the "Something unexpected" generic.
	// `msg` is lowercased above, so the search string is too. The
	// original orchestrator message is preserved in err.Error() and
	// is what we slice for the headline/Action (so capitalization
	// and paths come through as the user expects).
	if strings.Contains(msg, "working directory and graph db don't agree") {
		raw := err.Error()
		// Strip the "orchestrator: " package prefix so the headline
		// reads like a direct user-facing message.
		body := strings.TrimSpace(strings.TrimPrefix(raw, "orchestrator: "))
		// Split on the first newline: headline + the rest as Action
		// (which Render prints below the headline as the "what to
		// do" section).
		head, rest, _ := strings.Cut(body, "\n")
		return UserError{
			Kind:     "config.multiroot_divergence",
			Headline: head,
			Action:   strings.TrimSpace(rest),
			Severity: Block,
		}, true
	}

	// SQLite WAL contention — another kai operation is touching
	// the DB. Usually transient (the other op finishes within a
	// few seconds). The runner's busy_timeout already retries;
	// this kicks in when even that gives up.
	if strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy") {
		return UserError{
			Kind:     "sqlite.locked",
			Headline: "Working — another kai operation is finishing",
			Severity: Info,
		}, true
	}

	// Auth: 401 / unauthorized. The right remediation depends on
	// which provider failed — kailab session refresh is wrong
	// when the user is on openai-direct or anthropic-direct. The
	// provider error strings prefix themselves so we can detect.
	if strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "token invalid") {
		switch {
		case strings.Contains(msg, "kailab provider") || strings.Contains(msg, "token invalid"):
			return UserError{
				Kind:     "auth.expired",
				Headline: "Your kailab session expired",
				Action:   "Run `kai auth login` to refresh.",
				Severity: Block,
			}, true
		case strings.Contains(msg, "openai provider"):
			return UserError{
				Kind:     "auth.openai",
				Headline: "OpenAI rejected the request (401)",
				Action:   "Check OPENAI_API_KEY (and KAI_OPENAI_BASE_URL if you set one). Verify the key is valid and the model is enabled on your account.",
				Severity: Block,
			}, true
		case strings.Contains(msg, "anthropic provider"):
			return UserError{
				Kind:     "auth.anthropic",
				Headline: "Anthropic rejected the request (401)",
				Action:   "Check ANTHROPIC_API_KEY. Verify the key is valid and not revoked.",
				Severity: Block,
			}, true
		default:
			return UserError{
				Kind:     "auth.unknown",
				Headline: "Provider authentication failed (401)",
				Action:   "Check your provider credentials. Run `kai auth status` to see which provider is active.",
				Severity: Block,
			}, true
		}
	}

	// Provider transient — already retried by the provider, made
	// it past the retry budget. Tell the user what's happening
	// without the upstream's HTML stack trace.
	if provider.IsTransient(err) {
		return UserError{
			Kind:     "api.transient",
			Headline: "Upstream is temporarily unavailable",
			Detail:   "We retried a few times. The provider is throttling or down — try again in a minute.",
			Severity: Warn,
		}, true
	}

	// Provider context overflow — model said the prompt is too
	// long. Compaction already retried; this is the final form.
	// Action is concrete: shrink the conversation.
	if provider.IsContextOverflow(err) {
		return UserError{
			Kind:     "api.context_overflow",
			Headline: "Conversation outgrew the model's memory",
			Action:   "Type `/clear` to start a fresh session — the model can't fit any more turns.",
			Severity: Block,
		}, true
	}

	// LM Studio / Ollama context-too-small — the local server's
	// context is set lower than kai's prompt size. Same surface,
	// different fix.
	if strings.Contains(msg, "tokens to keep from the initial prompt") ||
		strings.Contains(msg, "context length") {
		return UserError{
			Kind:     "local.context_too_small",
			Headline: "Local model's context window is too small",
			Action:   "Bump n_ctx to at least 16384 in LM Studio (or `OLLAMA_NUM_CTX=16384`) and reload the model.",
			Severity: Block,
		}, true
	}

	// Provider model-not-found — KAI_*_MODEL points at something
	// the endpoint doesn't serve. Usually a typo or wrong
	// model name for the chosen vendor.
	if strings.Contains(msg, "model_not_found") || strings.Contains(msg, "does not exist or you do not have access") {
		return UserError{
			Kind:     "api.model_not_found",
			Headline: "The model your env points at isn't available",
			Action:   "Check KAI_OPENAI_MODEL / KAI_ANTHROPIC_MODEL against `curl -s $KAI_OPENAI_BASE_URL/models`.",
			Severity: Block,
		}, true
	}

	return UserError{}, false
}

// Wrap adds a kind hint to an error so a more general matcher can
// recognize it. Used at error-throw sites where the producer
// knows the kind better than the classifier ever could (e.g.
// orchestrator preflight). Cheap — Classify() unwraps via
// errors.As.
type Tagged struct {
	Kind    string
	Wrapped error
}

func (t *Tagged) Error() string { return t.Wrapped.Error() }
func (t *Tagged) Unwrap() error { return t.Wrapped }

// Tag attaches a kind to an error so the classifier can surface
// a specific UserError without relying on string-matching the
// message. Use sparingly — string-matching the upstream message
// is fine for stable error texts; tagging is for cases where
// we own the error site and want certainty.
func Tag(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &Tagged{Kind: kind, Wrapped: err}
}

// IsKind reports whether err carries the given Tagged kind.
// Convenience helper for tests and for the rare site that needs
// to peek at the tag without going through Classify.
func IsKind(err error, kind string) bool {
	var t *Tagged
	if !stderr.As(err, &t) {
		return false
	}
	return t.Kind == kind
}
