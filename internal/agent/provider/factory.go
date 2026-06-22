package provider

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Kind names the provider implementation a config selects.
type Kind string

const (
	// KindKailab routes through the kai-server proxy at
	// /api/v1/llm/messages. Default. Server holds the upstream key,
	// the user only needs a kailab bearer.
	KindKailab Kind = "kailab"

	// KindAnthropic talks directly to api.anthropic.com using a
	// per-developer ANTHROPIC_API_KEY. Identical request/response
	// shape and prompt-caching support; differs only in transport
	// and auth headers.
	KindAnthropic Kind = "anthropic"

	// KindOpenAI talks to any OpenAI-compatible endpoint
	// (api.openai.com, Together, Groq, vLLM, Ollama's OpenAI
	// compat, etc.) using the chat.completions API. Tool-use is
	// translated bidirectionally; prompt caching is unsupported on
	// the wire so reported cache stats are always zero.
	KindOpenAI Kind = "openai"
)

// Config is the materialized selection a caller hands to New. All
// fields except Kind are optional — defaults and env-var overrides
// fill in the gaps.
type Config struct {
	// Kind is the provider implementation. When empty, falls back
	// to KailabConfig if a bearer is set, otherwise an error from
	// New (won't silently default to a key that isn't there).
	Kind Kind

	// BaseURL overrides the provider's default endpoint. For
	// kailab, this is the kai-server URL the user logged into.
	// For anthropic, leave empty to use api.anthropic.com. For
	// openai, leave empty to use api.openai.com/v1 — set this to
	// point at any OpenAI-compatible vendor (e.g.
	// "https://api.together.xyz/v1", "http://localhost:11434/v1"
	// for Ollama).
	BaseURL string

	// AuthToken is the bearer/api-key used for requests. For
	// kailab, this is the user's bearer from `kai auth login`.
	// For anthropic, the ANTHROPIC_API_KEY. For openai, the
	// OPENAI_API_KEY (or whatever the chosen vendor calls it).
	AuthToken string

	// Model is the model id the runner sends on each Request. The
	// caller can override per-request; this is the default. We
	// pin it on Config so the trailer can surface "which model am
	// I actually talking to" without plumbing it through every
	// path that builds a Request.
	Model string
}

// FromEnv reads provider-selection env vars and returns a Config
// reflecting them. Used by the TUI startup. Order of precedence:
//
//  1. KAI_PROVIDER selects the kind ("kailab" | "anthropic" | "openai")
//  2. Per-kind env vars supply BaseURL / AuthToken / Model
//
// kailab is the default when KAI_PROVIDER is unset — preserves
// existing behavior. The caller passes in the kailab bearer and
// server URL because they live in the credentials store, not env.
func FromEnv(kailabBaseURL, kailabToken, defaultModel string) Config {
	kind := normalizeKind(os.Getenv("KAI_PROVIDER"))
	if kind == "" {
		kind = KindKailab
	}
	switch kind {
	case KindAnthropic:
		return Config{
			Kind:      KindAnthropic,
			BaseURL:   strings.TrimSpace(os.Getenv("KAI_ANTHROPIC_BASE_URL")),
			AuthToken: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
			Model:     pickModel(os.Getenv("KAI_ANTHROPIC_MODEL"), defaultModel),
		}
	case KindOpenAI:
		return Config{
			Kind:      KindOpenAI,
			BaseURL:   strings.TrimSpace(os.Getenv("KAI_OPENAI_BASE_URL")),
			AuthToken: strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
			Model:     pickModel(os.Getenv("KAI_OPENAI_MODEL"), defaultModel),
		}
	default:
		return Config{
			Kind:      KindKailab,
			BaseURL:   kailabBaseURL,
			AuthToken: kailabToken,
			Model:     defaultModel,
		}
	}
}

// normalizeKind accepts the canonical kind name AND a set of
// aliases that more honestly describe what the provider actually
// is. The OpenAI compat case is the most ambiguous in practice:
// "openai" implies api.openai.com, but the same wire protocol is
// used by Together, Groq, Ollama, vLLM, LM Studio, and many other
// non-OpenAI vendors. Letting users write "openai-compatible" or
// "oai-compat" keeps the env honest about what they actually
// pointed it at.
//
// Aliases:
//
//	openai            → openai (canonical)
//	openai-compat     → openai
//	openai-compatible → openai
//	oai               → openai
//	oai-compat        → openai
//	local             → openai (we assume local servers speak
//	                    the openai protocol; documented in README)
//
//	anthropic         → anthropic (canonical)
//	anthropic-direct  → anthropic
//	claude            → anthropic
//
//	kailab            → kailab (canonical)
//	"" (unset)        → kailab (default)
//
// Returns the canonical Kind. Unknown strings pass through as a
// Kind value so the existing "unknown kind" error in New() still
// fires with the user's input verbatim.
func normalizeKind(raw string) Kind {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return ""
	case "openai", "openai-compat", "openai-compatible", "oai", "oai-compat", "local":
		return KindOpenAI
	case "anthropic", "anthropic-direct", "claude":
		return KindAnthropic
	case "kailab":
		return KindKailab
	}
	return Kind(s)
}

func pickModel(env, fallback string) string {
	if s := strings.TrimSpace(env); s != "" {
		return s
	}
	return fallback
}

// New builds a Provider from a Config. Validation errors are
// returned with messages a non-engineer can act on (e.g. "set
// ANTHROPIC_API_KEY") rather than stack traces.
func New(cfg Config) (Provider, error) {
	switch cfg.Kind {
	case "", KindKailab:
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("provider kailab: missing server URL (run `kai auth login`)")
		}
		if cfg.AuthToken == "" {
			return nil, fmt.Errorf("provider kailab: not logged in (run `kai auth login`)")
		}
		k := NewKailab(cfg.BaseURL, cfg.AuthToken)
		if hint := strings.TrimSpace(os.Getenv("KAI_KAILAB_UPSTREAM")); hint != "" {
			k.ProviderHint = hint
		}
		return k, nil

	case KindAnthropic:
		if cfg.AuthToken == "" {
			return nil, fmt.Errorf("provider anthropic: ANTHROPIC_API_KEY not set")
		}
		base := cfg.BaseURL
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return NewAnthropic(base, cfg.AuthToken), nil

	case KindOpenAI:
		if cfg.AuthToken == "" {
			// Many local servers don't enforce a key (Ollama,
			// vLLM with auth disabled). Allow empty for those —
			// the request will succeed if the endpoint accepts
			// it, fail with a 401 if it doesn't. Don't pre-judge.
			cfg.AuthToken = ""
		}
		base := cfg.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return NewOpenAI(base, cfg.AuthToken), nil
	}
	return nil, fmt.Errorf("provider: unknown kind %q (want kailab|anthropic|openai)", cfg.Kind)
}

// sharedHTTPClient returns the standard non-streaming client used by
// the direct providers. Matches kailab's 120s — same upstream
// timeout, same failure mode story.
func sharedHTTPClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}
