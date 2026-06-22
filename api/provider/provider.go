package provider

// engineprovider.go: re-exports of the symbols kai-cli/internal/tui/
// uses from kai-cli/internal/agent/engineprovider. Phase 1 of the TUI
// API extraction (see docs/architecture/tui-api-extraction.md);
// chosen first because it has the widest fanout in the TUI tree
// (11 files importing it pre-migration).
//
// Strategy: type aliases for value types, package-level function
// re-exports for helpers. The TUI never constructs concrete
// providers itself (the kai command does, via factory.New from
// CLI config), so Provider stays an interface and the
// implementations live behind it on the engine side.
//
// Anything imported here is part of the TUI's public contract
// with the engine. Adding a symbol is a deliberate API decision;
// the check-tui-imports gate forces this conversation explicitly.

import engineprovider "kai/internal/agent/provider"

// ─── Core types ──────────────────────────────────────────────────────────

// Provider is the LLM transport the TUI hands to the orchestrator,
// chat agent, triage, and gate-review. The concrete implementation
// (Anthropic, Kailab, OpenAI-compatible) is constructed by the kai
// command's startup wiring, never inside the TUI.
type Provider = engineprovider.Provider

// Request is the shape the TUI builds when it invokes the provider
// directly (chat agent fallback, triage classification, gate-review
// audit). Tool dispatch lives in the orchestrator and doesn't touch
// this from the TUI side.
type Request = engineprovider.Request

// Response is the provider's reply: parts (text + tool_use blocks),
// finish reason, token usage. The TUI reads token usage for the
// trailer line.
type Response = engineprovider.Response

// RequestState is the per-phase lifecycle event the provider emits
// via Request.OnState so the TUI can render real "sent / connected
// / streaming / done" status instead of a frozen spinner.
type RequestState = engineprovider.RequestState

// RequestPhase is the enum carried by RequestState.Phase.
type RequestPhase = engineprovider.RequestPhase

// Config is the factory input for engineprovider.New. The TUI does not
// construct one directly today; it's re-exported because
// internal/tui/app.go references it in setup-time wiring.
type Config = engineprovider.Config

// Kind identifies which provider transport to instantiate.
type Kind = engineprovider.Kind

// CapExceededError surfaces the kailab daily-cost cap to the TUI's
// trailer warning band. The TUI uses AsCapExceeded to unwrap.
type CapExceededError = engineprovider.CapExceededError

// ─── Phase constants ─────────────────────────────────────────────────────

const (
	PhaseSent              = engineprovider.PhaseSent
	PhaseConnected         = engineprovider.PhaseConnected
	PhaseStreaming         = engineprovider.PhaseStreaming
	PhaseStreamIdle        = engineprovider.PhaseStreamIdle
	PhaseDone              = engineprovider.PhaseDone
	PhaseError             = engineprovider.PhaseError
	PhaseUpstreamSent      = engineprovider.PhaseUpstreamSent
	PhaseUpstreamConnected = engineprovider.PhaseUpstreamConnected
	PhaseUpstreamError     = engineprovider.PhaseUpstreamError
)

// ─── Kind constants ──────────────────────────────────────────────────────

const (
	KindKailab    = engineprovider.KindKailab
	KindAnthropic = engineprovider.KindAnthropic
	KindOpenAI    = engineprovider.KindOpenAI
)

// ─── Package-level helpers ───────────────────────────────────────────────

// NewProvider constructs a concrete Provider from a Config. The
// TUI's startup wiring is currently the only TUI-side caller; the
// orchestrator constructs its own at the call site.
//
// (Re-exports engineprovider.New under a less-collision-prone name; the
// engine package's "New" reads fine when the package qualifier is
// "engineprovider.New", less so when it's "api.New".)
func NewProvider(cfg Config) (Provider, error) {
	return engineprovider.New(cfg)
}

// AsCapExceeded unwraps an error to a *CapExceededError if the
// underlying chain carries one (the kailab daily-cost cap path).
func AsCapExceeded(err error) (*CapExceededError, bool) {
	return engineprovider.AsCapExceeded(err)
}

// IsContextOverflow reports whether err is a provider-side
// context-window overflow (the model said "I'm out of tokens" — a
// signal for the TUI's compaction nudge).
func IsContextOverflow(err error) bool {
	return engineprovider.IsContextOverflow(err)
}

// IsTransient reports whether err is a recoverable provider error
// (timeout, 5xx, transient network) vs a hard failure. The TUI's
// auto-retry / error classification uses this.
func IsTransient(err error) bool {
	return engineprovider.IsTransient(err)
}

// DailyUsage queries the provider's optional DailyUsageReporter
// interface. Returns (cost, cap, ok=true) when the provider tracks
// daily usage (kailab does; anthropic-direct doesn't), (0, 0,
// false) otherwise.
func DailyUsage(p Provider) (cost, cap int, ok bool) {
	return engineprovider.DailyUsage(p)
}
