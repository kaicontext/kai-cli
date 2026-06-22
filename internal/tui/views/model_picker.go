package views

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"kai/api/provider"
)

// fallbackModels is the static menu used when the kailab catalog
// endpoint is unavailable (older server, network failure, kailab
// kind not in use). Kept narrow — first-class + the OpenAI shortlist
// the CLI shipped before the catalog endpoint existed. New models
// should be added to the SERVER catalog (kailab-control internal/api/
// models_catalog.go), not here. This list exists so the CLI keeps
// working against pre-feature servers.
var fallbackModels = map[provider.Kind][]string{
	provider.KindKailab: {
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-haiku-4-5-20251001",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		// Open models via OpenRouter (switched from Together 2026-06-05).
		"deepseek/deepseek-v4-pro",
		"z-ai/glm-5.1",
		"moonshotai/kimi-k2.6",
		"qwen/qwen3.5-397b-a17b",
		"qwen/qwen3-coder-next",
	},
	provider.KindAnthropic: {
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-haiku-4-5-20251001",
	},
	provider.KindOpenAI: {
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		// Local / OpenAI-compatible endpoints (Ollama, vLLM, etc.)
		// override KAI_OPENAI_BASE_URL and pick whatever model their
		// server exposes; the menu here is just the api.openai.com
		// shortlist.
	},
}

// knownModels resolves the model menu for a provider kind, preferring
// the live kailab catalog when the kailab client is reachable and the
// kind is `kailab`. Falls back to the static `fallbackModels` map
// otherwise. Experimental-tier models are filtered out unless
// KAI_EXPERIMENTAL_PROVIDERS contains the provider name (e.g.
// "groq" — comma-separated for multiple).
//
// The lookup is cached per-process for `catalogCacheTTL` so the
// menu doesn't refetch on every keystroke. Cache is best-effort:
// if the server is briefly unreachable we keep using the last good
// catalog rather than collapsing to the fallback mid-session.
func knownModels(kind provider.Kind) []string {
	// Live catalog is only meaningful for the kailab kind — the
	// catalog describes what kailab will proxy. Anthropic/OpenAI
	// direct paths use their own provider's well-known model ids
	// and don't go through kailab.
	if kind == provider.KindKailab {
		if entries := getCachedCatalog(); entries != nil {
			ids := filterCatalogForPicker(entries, experimentalProvidersFromEnv())
			if len(ids) > 0 {
				return ids
			}
		}
	}
	return fallbackModels[kind]
}

// filterCatalogForPicker reduces a catalog response to the model id
// list the picker should display. Experimental-tier entries are
// included only when their provider is in `enabledExperimental`.
// Order: first-class, second-class, then experimental — same shape
// the catalog ships in, but we re-sort defensively in case the
// server's order ever changes.
func filterCatalogForPicker(entries []RemoteCatalogEntry, enabledExperimental map[string]bool) []string {
	type bucket struct {
		first, second, experimental []string
	}
	var b bucket
	for _, e := range entries {
		switch e.Tier {
		case "first_class":
			b.first = append(b.first, e.ID)
		case "second_class":
			b.second = append(b.second, e.ID)
		case "experimental":
			if enabledExperimental[strings.ToLower(e.Provider)] {
				b.experimental = append(b.experimental, e.ID)
			}
		}
	}
	out := make([]string, 0, len(b.first)+len(b.second)+len(b.experimental))
	out = append(out, b.first...)
	out = append(out, b.second...)
	out = append(out, b.experimental...)
	return out
}

// experimentalProvidersFromEnv parses KAI_EXPERIMENTAL_PROVIDERS
// into a lookup set. Comma-separated, case-insensitive. Empty env
// var → empty set → no experimental models in the picker.
func experimentalProvidersFromEnv() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("KAI_EXPERIMENTAL_PROVIDERS"))
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out[p] = true
		}
	}
	return out
}

// RemoteCatalogEntry is the local mirror of remote.CatalogEntry.
// Duplicated as a tiny struct rather than imported because the
// views package can't depend on the remote client without a
// dependency-direction reversal in the TUI bootstrap. Kept to the
// fields the picker actually reads.
type RemoteCatalogEntry struct {
	ID       string
	Provider string
	Tier     string
}

// catalog cache state. Populated lazily by SetCatalog (called once
// at TUI startup with the result of remote.Client.ListModels) and
// read by getCachedCatalog from the picker. Read-mostly: the cache
// updates exactly once per process today, so a sync.RWMutex is
// overkill — a plain Mutex is fine.
var (
	catalogMu      sync.Mutex
	catalogEntries []RemoteCatalogEntry
)

// SetCatalog seeds the in-process catalog from a remote.Client
// response. Called by the TUI bootstrap after kailab login;
// idempotent and safe to call multiple times (the picker will use
// the most recent data). Empty input clears the cache so the next
// picker call falls back to the static list.
func SetCatalog(entries []RemoteCatalogEntry) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogEntries = entries
}

func getCachedCatalog() []RemoteCatalogEntry {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if len(catalogEntries) == 0 {
		return nil
	}
	out := make([]RemoteCatalogEntry, len(catalogEntries))
	copy(out, catalogEntries)
	return out
}

var providerOrder = []provider.Kind{
	provider.KindKailab,
	provider.KindAnthropic,
	provider.KindOpenAI,
}

// ModelPickerState is the modal-picker UI state. Held on the REPL
// as a pointer so nil means "no picker open" (the same pattern
// pendingPlan/pendingCostCap use). The REPL routes ↑/↓/Enter/Esc
// to MoveUp/MoveDown/Selected when this is non-nil; everything
// else stays in the textarea so a stray keystroke doesn't dismiss
// the menu.
type ModelPickerState struct {
	Kind    provider.Kind
	Models  []string
	Cursor  int
	Current string // worker model id currently active — rendered with ●
	Planner string // planner model id currently active — rendered with ◇ (informational; picker only swaps worker)
}

// NewModelPicker builds picker state for kind. Returns nil when the
// kind has no resolvable model list (caller falls back to the text
// error path). The cursor starts on the currently active model when
// it appears in the list — saves the user from arrow-spamming to
// their current selection just to confirm.
func NewModelPicker(kind provider.Kind, current string) *ModelPickerState {
	models := knownModels(kind)
	if len(models) == 0 {
		return nil
	}
	cursor := 0
	for i, m := range models {
		if m == current {
			cursor = i
			break
		}
	}
	return &ModelPickerState{
		Kind:    kind,
		Models:  models,
		Cursor:  cursor,
		Current: current,
	}
}

// NewModelPickerWithPlanner is like NewModelPicker but also captures
// the active planner model. The picker only swaps the worker model,
// but showing both makes the role split obvious — without this users
// see ● next to GLM and assume "GLM is selected" without realizing
// the planner runs on a different model (Qwen by default).
func NewModelPickerWithPlanner(kind provider.Kind, worker, planner string) *ModelPickerState {
	p := NewModelPicker(kind, worker)
	if p != nil {
		p.Planner = planner
	}
	return p
}

func (p *ModelPickerState) MoveUp() {
	if p == nil || len(p.Models) == 0 {
		return
	}
	p.Cursor = (p.Cursor - 1 + len(p.Models)) % len(p.Models)
}

func (p *ModelPickerState) MoveDown() {
	if p == nil || len(p.Models) == 0 {
		return
	}
	p.Cursor = (p.Cursor + 1) % len(p.Models)
}

// Selected returns the model id under the cursor.
func (p *ModelPickerState) Selected() string {
	if p == nil || p.Cursor < 0 || p.Cursor >= len(p.Models) {
		return ""
	}
	return p.Models[p.Cursor]
}

// Render paints the picker as a vertical list. `❯` marks the cursor,
// `●` marks the currently active model (so the user can see what
// they'd be replacing). Hint line at the bottom mirrors the plan
// menu's `(←/→ … Esc to cancel)` style.
func (p *ModelPickerState) Render() string {
	if p == nil || len(p.Models) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pick a %s worker model (● = current worker, ◇ = current planner):\n", p.Kind)
	for i, m := range p.Models {
		prefix := "   "
		isPlanner := p.Planner != "" && m == p.Planner
		switch {
		case i == p.Cursor && m == p.Current && isPlanner:
			prefix = " ❯⊙"
		case i == p.Cursor && m == p.Current:
			prefix = " ❯●"
		case i == p.Cursor && isPlanner:
			prefix = " ❯◇"
		case i == p.Cursor:
			prefix = " ❯ "
		case m == p.Current && isPlanner:
			prefix = "  ⊙"
		case m == p.Current:
			prefix = "  ●"
		case isPlanner:
			prefix = "  ◇"
		}
		label := m
		if i == p.Cursor {
			fmt.Fprintf(&sb, "%s %s\n", prefix, stylePlanChoice.Render(label))
		} else {
			fmt.Fprintf(&sb, "%s %s\n", prefix, label)
		}
	}
	sb.WriteString(styleDim.Render("(↑/↓ to move, Enter swaps worker model only; planner stays put. Esc to cancel)"))
	return sb.String()
}

// handleModelCommand implements /model. Behavior by arg count:
//
//	/model                       → list providers + their default model
//	/model <provider>            → list models for that provider
//	/model <provider> <model>    → swap the live provider+model for the next turn
//
// The swap mutates s.OrchestratorCfg.AgentProvider in place; subsequent
// planner and orchestrator runs pick it up via the same field they
// already read each turn. No TUI restart needed — provider is just an
// interface, and PlannerAgent / runner build a fresh request per turn.
//
// Returns the text to write into the REPL transcript.
func handleModelCommand(s *PlannerServices, args []string) string {
	if s == nil {
		return "model: planner services not configured"
	}

	switch len(args) {
	case 0:
		return formatProviderList(s)
	case 1:
		return formatModelList(args[0])
	default:
		return swapModel(s, args[0], strings.Join(args[1:], " "))
	}
}

func formatProviderList(s *PlannerServices) string {
	currentKind := currentProviderKind(s)
	currentModel := s.OrchestratorCfg.AgentModel

	var sb strings.Builder
	sb.WriteString("Providers (active is marked ●):\n")
	for _, k := range providerOrder {
		marker := "  "
		if k == currentKind {
			marker = "● "
		}
		def := ""
		if models := knownModels(k); len(models) > 0 {
			def = "  default: " + models[0]
		}
		fmt.Fprintf(&sb, "  %s%-10s%s\n", marker, k, def)
	}
	fmt.Fprintf(&sb, "\nActive: %s / %s\n", currentKind, currentModel)
	sb.WriteString("\nUse `/model <provider>` to list models, or `/model <provider> <model>` to swap.")
	return sb.String()
}

// containsString reports whether needle is in haystack. Case-sensitive;
// model ids are conventionally lowercase but we don't normalize here
// because passing them through to providers verbatim avoids "I typed
// Claude and it said unknown" surprises that case-insensitive
// matching could mask.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func formatModelList(rawProvider string) string {
	kind := normalizeProviderArg(rawProvider)
	models := knownModels(kind)
	if models == nil {
		return fmt.Sprintf("model: unknown provider %q (try kailab, anthropic, or openai)", rawProvider)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Models for %s:\n", kind)
	for _, m := range models {
		fmt.Fprintf(&sb, "  %s\n", m)
	}
	fmt.Fprintf(&sb, "\nSwap with `/model %s <model-id>`.\n", kind)
	if kind == provider.KindOpenAI {
		sb.WriteString("(KAI_OPENAI_BASE_URL lets you point at Ollama/vLLM/etc.; any model id\n")
		sb.WriteString(" the local server exposes will work even if it's not in this list.)")
	}
	return sb.String()
}

// swapModel rebuilds the live provider with the chosen kind+model
// and substitutes it on the orchestrator config. Credentials are
// pulled from the same env / kailab login the TUI loaded at startup,
// so swapping to openai mid-session requires OPENAI_API_KEY in env
// (we report the error as a friendly hint, not a crash).
func swapModel(s *PlannerServices, rawProvider, model string) string {
	kind := normalizeProviderArg(rawProvider)
	model = strings.TrimSpace(model)
	if model == "" {
		return "model: missing model id"
	}

	// Reject typos / random strings before building a provider with
	// them. Without this, `/model kailab v` silently set the model
	// to "v" and the next request failed with a confusing "no
	// assistant message" or "model not found" error from upstream
	// — the user had to figure out the typo themselves. Now the
	// command refuses up-front with the list of valid options.
	//
	// The openai kind keeps a relaxed match because users point
	// it at non-OpenAI hosts (Ollama, vLLM, Together) whose model
	// catalog isn't in our static list. Those need to pass any
	// non-empty id through.
	if kind != provider.KindOpenAI {
		if known := knownModels(kind); len(known) > 0 && !containsString(known, model) {
			return fmt.Sprintf("model: unknown id %q for %s. Valid options:\n  %s\nRun `/model %s` (no id) to see the full list.",
				model, kind, strings.Join(known, "\n  "), kind)
		}
	}

	cfg := provider.Config{Kind: kind, Model: model}

	// Source credentials per kind. We don't try to re-derive kailab
	// creds from disk here — if the TUI was launched with kailab
	// creds, they're held by the existing AgentProvider; we read
	// them off the env we care about, falling back to "ask user to
	// re-login" when missing.
	switch kind {
	case provider.KindKailab:
		// Reuse whatever the TUI was launched with. The current
		// provider's BaseURL/AuthToken are fields on its struct,
		// but reaching across the interface is messier than just
		// reading the same env the TUI's startup path read.
		// Easier path: build via FromEnv with the kailab creds we
		// stash at startup. That's not currently plumbed, so for
		// now we ask the user to swap kailab via env-restart and
		// only support model-level swaps WITHIN the kailab kind.
		if currentProviderKind(s) == provider.KindKailab {
			// Same kind, just a model swap. The model field is
			// passed per-request by the runner, so updating the
			// per-role model strings alone is sufficient — no
			// provider rebuild.
			//
			// Swap BOTH worker (AgentModel) and chat (ChatModel).
			// Pre-2026-05-25 only AgentModel got swapped, which
			// surprised users running /model to escape a slow
			// reasoning model on chat turns — the chat path uses
			// ChatModel and stayed on the startup default. The
			// 2026-05-25 dogfood pinned it: user typed /model
			// claude-opus-4-6 to escape DeepSeek-V4-Pro on Svelte
			// content; worker switched, chat didn't, chat turns
			// stayed slow.
			//
			// PlannerCfg.Model intentionally stays put: the planner
			// has structured-output requirements that some models
			// (notably opus-4-6 on the kai planner prompt) don't
			// handle reliably, and the failure mode is opaque
			// (garbled JSON → empty-plan fallback). Planner swap
			// remains a startup-time concern via --planner-model.
			s.OrchestratorCfg.AgentModel = model
			s.ChatModel = model
			return fmt.Sprintf("model → kailab / %s   (worker + chat; planner stays on %s)", model, s.PlannerCfg.Model)
		}
		return "model: switching TO kailab mid-session needs a TUI restart " +
			"(kailab credentials live in disk, not env). For now, set " +
			"KAI_PROVIDER=kailab in env, exit, and re-run kai code."
	case provider.KindAnthropic:
		cfg.AuthToken = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
		cfg.BaseURL = strings.TrimSpace(os.Getenv("KAI_ANTHROPIC_BASE_URL"))
		if cfg.AuthToken == "" {
			return "model: ANTHROPIC_API_KEY not set in env. " +
				"Set it and either restart the TUI, or rerun this command."
		}
	case provider.KindOpenAI:
		cfg.AuthToken = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		cfg.BaseURL = strings.TrimSpace(os.Getenv("KAI_OPENAI_BASE_URL"))
		if cfg.AuthToken == "" {
			return "model: OPENAI_API_KEY not set in env. " +
				"Set it (and KAI_OPENAI_BASE_URL for non-OpenAI hosts) and rerun."
		}
	default:
		return fmt.Sprintf("model: unknown provider %q", rawProvider)
	}

	prov, err := provider.NewProvider(cfg)
	if err != nil {
		return "model: " + err.Error()
	}
	s.OrchestratorCfg.AgentProvider = prov
	s.OrchestratorCfg.AgentModel = model
	// Chat path uses ChatModel — swap it too so /model behaves
	// uniformly across worker and chat. See kailab branch above
	// for the rationale and the planner-stays-put constraint.
	s.ChatModel = model
	return fmt.Sprintf("model → %s / %s   (worker + chat; planner stays on %s)", kind, model, s.PlannerCfg.Model)
}

// currentProviderKind reads the active provider's kind via the
// SupportsCache helper — kailab/anthropic both report cache-supporting,
// openai doesn't, but that doesn't tell kailab from anthropic. We
// fall back on the AgentModel string heuristic for the kailab/anthropic
// distinction (kailab models are claude-* through the proxy; anthropic
// direct also uses claude-*, so this can be ambiguous, which is OK
// because the only thing the picker shows is the same model menu for
// both).
func currentProviderKind(s *PlannerServices) provider.Kind {
	if s == nil || s.OrchestratorCfg.AgentProvider == nil {
		return ""
	}
	// The provider interface doesn't expose Kind directly. The
	// simplest tell is the env var that drove the original FromEnv
	// call — if it's set, we trust it; if not, the launch path
	// defaulted to kailab.
	switch normalizeProviderArg(os.Getenv("KAI_PROVIDER")) {
	case provider.KindAnthropic:
		return provider.KindAnthropic
	case provider.KindOpenAI:
		return provider.KindOpenAI
	}
	return provider.KindKailab
}

// normalizeProviderArg accepts the same aliases api.normalizeKind
// supports, but is exported through this package so the slash handler
// can pre-validate without round-tripping through api.New.
func normalizeProviderArg(raw string) provider.Kind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "openai", "openai-compat", "openai-compatible", "oai", "oai-compat", "local":
		return provider.KindOpenAI
	case "anthropic", "anthropic-direct", "claude":
		return provider.KindAnthropic
	case "kailab", "":
		return provider.KindKailab
	}
	return provider.Kind(strings.ToLower(strings.TrimSpace(raw)))
}
