package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"kai/internal/agent/provider"
	"kai/internal/agent/session"
	"kai/internal/agentprompt"
	"kai/internal/config"
	"kai/internal/graph"
	"kai/internal/orchestrator"
	"kai/internal/planner"
	"kai/internal/projects"
	"kai/internal/remote"
	"kai/internal/safetygate"
	"kai/internal/tui"
	"kai/internal/tui/fixxy"
	"kai/internal/tui/views"
)

// codeCmd is `kai code` — the unified entrypoint to the kai coding
// experience (kit-in-kai Phase 1). It is a thin passthrough: it resolves
// (and, on first use, self-installs) the managed `kit` binary and hands
// off to it, forwarding every trailing arg/flag verbatim. DisableFlagParsing
// keeps cobra from intercepting flags meant for kit. The command name
// `kai code` is permanent; only the implementation changes later (Phase 4
// runs the experience in-process instead of shelling out). RunE lives in
// code.go; the installer/exec logic lives in internal/kitlauncher.
//
// NOTE: runCodeTUI / runCodeHeadless / buildPlannerServices below (and the
// whole of headless.go) are the *previous* native Bubble Tea TUI that
// `kai code` used to point at. As of Phase 1 they are dead, vestigial code:
// no longer wired to any command, kept intact only so Phase 4/5 can revive
// the experience in-process or delete it. Do not mistake them for live
// behavior — `kai code` now goes through code.go → kitlauncher → kit.
var codeCmd = &cobra.Command{
	Use:   "code [-- kit args…]",
	Short: "Launch the kai coding experience (installs kit on first use)",
	Long: `Launch the interactive kai coding experience.

kai code resolves the managed kit binary — downloading it to
~/.kai/bin on first use — then hands off to it. Every argument and
flag after "code" is forwarded to kit unchanged, so:

    kai code --some-kit-flag value

behaves exactly like running kit directly. Ctrl+C, terminal resize,
and exit codes all behave as they do under kit.`,
	// Forward everything after `code` to kit verbatim; do not let cobra
	// parse flags meant for the child.
	DisableFlagParsing: true,
	RunE:               runCode,
}

// fixxyUpper is the activation level for the secret fixxy-upper
// mode. 0 = off (default), 1/2/3 = escalating intervention.
// See internal/tui/fixxy for what each mode does. Hidden from
// --help on purpose: this is a dev-loop tool, not a documented
// user feature.
var fixxyUpper int

// headlessPrompt, when non-empty, makes `kai code -p "task"` run a
// single planner+execute cycle without launching the TUI. Designed
// for cost-validation benchmarking: pair with `kai run summary` to
// produce reproducible per-turn dollar / cache-reuse numbers.
var headlessPrompt string

// PlannerModelFlag and AgentModelFlag are set by --planner-model / --agent-model CLI flags.
var PlannerModelFlag string
var AgentModelFlag string

// autoFlag is set by `kai code --auto`. It is one of three inputs to
// handsOffEnabled (flag, KAI_AUTO env, config.autonomy).
var autoFlag bool

// handsOffEnabled resolves whether the run is hands-off, with the
// flag and env var overriding the config file. Precedence: --auto or
// KAI_AUTO win when set; otherwise the project's autonomy config
// applies.
func handsOffEnabled(cfg config.Config) bool {
	if autoFlag {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAI_AUTO"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return cfg.HandsOff()
}

// consultModelFromEnv returns the model id kai_consult uses when an
// agent escalates a stuck exploration. Defaults to claude-opus-4-7 —
// the strongest available model. The whole point of escalation is
// "the cheap model is stuck, get the best possible diagnosis"; the
// per-call cost is real but ~1-2 escalations per stuck run still
// beats 20+ thrashing cheap-model turns that produce no edit.
//
// KAI_CONSULT_MODEL=<id>  → use that model (e.g. claude-sonnet-4-6 to
//
//	trade some quality for ~5× lower cost)
//
// KAI_CONSULT_MODEL=off   → disable kai_consult entirely (returns "")
//
// Single function rather than a config-file knob because the choice is
// global per run and rarely needs project-level override; KAI_*_MODEL
// is the established pattern (KAI_PLANNER_MODEL, KAI_AGENT_MODEL).
func consultModelFromEnv() string {
	v := os.Getenv("KAI_CONSULT_MODEL")
	switch v {
	case "off", "none", "disabled":
		return ""
	case "":
		return "claude-opus-4-7"
	default:
		return v
	}
}

// modelFromEnv returns the model id for a role: the env override
// (envVar) when set and non-empty, otherwise the config-supplied
// fallback. Used by buildPlannerServices to resolve the classifier /
// planner / chat / agent role models — KAI_CLASSIFIER_MODEL,
// KAI_PLANNER_MODEL, KAI_CHAT_MODEL, KAI_AGENT_MODEL respectively.
func modelFromEnv(envVar, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	return fallback
}

// resumeSessionID, when non-empty, tells the TUI to resume a prior
// session by id (set via `kai code --session <id>`). The agent
// runner already supports session resume via Options.SessionID;
// this flag just plumbs the user's choice into that path. Sessions
// persist to .kai/db.sqlite, so the agent gets back its prior
// transcript on the first turn after resume.
var resumeSessionID string

// runCodeTUI was codeCmd.RunE before kit-in-kai Phase 1 — it is the
// native Bubble Tea TUI `kai code` used to enter. It is now dead code:
// codeCmd.RunE points at runCode (the kit passthrough, in code.go). Kept
// per the note at the top of this file so Phase 4/5 can revive or delete
// it. See codeCmd for the live behavior.
//
// Refuses in the following cases:
//   - stdin or stdout is not a terminal (piped or redirected)
//   - openDB fails (no .kai directory yet — run `kai init` first)
func runCodeTUI(cmd *cobra.Command, args []string) error {
	if headlessPrompt != "" {
		return runCodeHeadless(cmd.Context(), headlessPrompt)
	}
	if !isTerminal() {
		// Non-interactive context: print help so scripts get a sensible
		// response instead of a TUI that immediately exits.
		return cmd.Help()
	}

	cwd, _ := os.Getwd()

	// Container-invariant check: refuse to launch when cwd has BOTH
	// kai.projects.yaml (claims "I'm a container") and .kai/ (claims
	// "I'm a project"). The two are mutually exclusive; running here
	// would produce silent cross-DB mismatches downstream that
	// surface as opaque SQL errors at integrate time. The error
	// message names both paths and the two safe fixes — let the
	// user pick rather than auto-resolving.
	if err := projects.CheckContainerInvariant(cwd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fmt.Errorf("refusing to launch: container/project invariant violated")
	}

	// Project discovery: figure out whether cwd is a single project,
	// a multi-root container of projects, an uninitialized project,
	// or a parent dir that holds many sibling repos. The branches
	// below tell the user what to do without launching the TUI when
	// a launch wouldn't make sense (e.g. inside ~/projects).
	set, outcome := projects.Discover(cwd)
	switch outcome {
	case projects.OutcomeContainer:
		// User ran `kai code` in a directory that holds many
		// projects (~/projects, ~/code, etc.). Don't auto-init at
		// the container level — that would create a confusing
		// "all my repos are one workspace" state. Instead, point
		// them at the projects we found and ask them to cd in.
		fmt.Fprintf(os.Stderr, "%s looks like a directory of projects, not a single project.\n", cwd)
		fmt.Fprintln(os.Stderr, "Pick a project below and run `kai code` from inside it:")
		entries, _ := os.ReadDir(cwd)
		shown := 0
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			fmt.Fprintf(os.Stderr, "  - %s\n", e.Name())
			shown++
			if shown >= 20 {
				fmt.Fprintln(os.Stderr, "  ...")
				break
			}
		}
		return nil
	case projects.OutcomeEmpty, projects.OutcomeUninitProject:
		// Container-yaml guard: if cwd has kai.projects.yaml, it's
		// declaring "I'm a container of projects" — auto-init would
		// create the .kai/ that contradicts that declaration and
		// reproduces the misconfig the user just cleaned up. Refuse
		// the auto-bootstrap with the same hint as the manual
		// `kai init` block (verified May 2026: deleting `.kai/` in a
		// container, then running `kai code`, would silently
		// recreate it via this path — the user fix loop never
		// converged).
		if _, err := os.Stat(filepath.Join(cwd, projects.ProjectsFileName)); err == nil {
			fmt.Fprintf(os.Stderr,
				"refusing to auto-init: %s exists at cwd, declaring this as a container of projects.\n"+
					"  Auto-creating .kai/ here would contradict that. Two ways forward:\n"+
					"    1. cd into one of the sub-projects listed in %s and run kai code there.\n"+
					"    2. If this dir IS the project, delete %s first.\n",
				projects.ProjectsFileName, projects.ProjectsFileName, projects.ProjectsFileName)
			return fmt.Errorf("refusing to auto-init: %s exists at cwd", projects.ProjectsFileName)
		}
		// Auto-bootstrap. The user typed `kai code` here; they
		// don't care what a "kai project" is or whether the dir
		// has a go.mod yet. Pick a smart name, run init +
		// capture silently, and drop them into the TUI. Adding
		// .kai/ is local, reversible (rm -rf), and uncontroversial
		// — much better than walling off a new user with a
		// "what's kai" prompt.
		//
		// Container outcomes stay as a hard refuse: auto-init in
		// ~/projects would be a giant footgun.
		name := ""
		if p := set.Primary(); p != nil && p.Name != "" {
			name = p.Name
		}
		if name == "" {
			name = projects.SmartName(cwd)
		}
		if name == "" {
			name = filepath.Base(cwd)
		}
		fmt.Fprintf(os.Stderr, "kai code: setting up %q (one-time, ~3 seconds)…\n", name)
		if err := bootstrapProject(cwd); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		set, outcome = projects.Discover(cwd)
		if outcome != projects.OutcomeRootsFound {
			return fmt.Errorf("kai init succeeded but rediscovery still found nothing — file an issue")
		}
	case projects.OutcomeRootsFound:
		// Fall through.
	}

	if err := set.Open(); err != nil {
		return fmt.Errorf("opening projects: %w", err)
	}
	defer set.Close()

	// Persist the discovered set so the next launch in this dir
	// rediscovers the same projects (and honors any pinned
	// additions). Best-effort: write failure is non-fatal.
	//
	// Skip the write when the discovery root IS the single project
	// (one entry pointing at "."): that case would produce a
	// kai.projects.yaml living next to the .kai/ that defines the
	// project, which is exactly the container/project invariant
	// CheckContainerInvariant warns about. The yaml is purely a
	// hint about WHERE projects live; when there's just one and it
	// lives right here, the .kai/ already says so.
	skipSave := false
	if ps := set.Projects(); len(ps) == 1 {
		// "." or "" both mean "discovery root = project root".
		p := strings.TrimSpace(ps[0].Path)
		if p == "" || p == "." || p == "./" {
			skipSave = true
		}
	}
	if !skipSave {
		if err := projects.SaveFile(set.DiscoveryRoot, set.Projects()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: saving %s: %v\n", projects.ProjectsFileName, err)
		}
	}

	// Surface what we discovered. For single-project this is a
	// one-liner; for multi-root it lists every project so the user
	// has a clear mental model of what the agent will see.
	if len(set.Projects()) == 1 {
		p := set.Primary()
		fmt.Fprintf(os.Stderr, "kai code: %s (%s)\n", p.Name, p.Path)
	} else {
		fmt.Fprintf(os.Stderr, "kai code: multi-root workspace at %s\n", set.DiscoveryRoot)
		for _, p := range set.Projects() {
			pin := ""
			if p.Pinned {
				pin = " [pinned]"
			}
			fmt.Fprintf(os.Stderr, "  - %s%s\n", p.Name, pin)
		}
	}

	// The legacy global kaiDir + db handle keep working for
	// sub-commands invoked through the REPL. We point them at the
	// primary project so single-root behavior is unchanged.
	primary := set.Primary()
	if primary == nil {
		return fmt.Errorf("no primary project resolved from workspace at %s (set.Primary() returned nil)", cwd)
	}
	kaiDir = primary.KaiDir
	db := primary.DB

	// Multi-root workspaces sometimes have pinned-but-uninitialized
	// projects: the primary has a KaiDir resolved (kaipath always
	// resolves) but no db.sqlite on disk yet, so projects.Open()
	// left primary.DB nil. Surface that as a real error with a
	// suggested fix — running session.EnsureSchema on a nil DB
	// panics inside graph.(*DB).Exec, which is what bit you when
	// `kai code` from a multi-root parent that hadn't been captured.
	if db == nil {
		return fmt.Errorf(
			"primary project %q has no graph yet — run `kai capture` inside %s first, or `cd` into an already-initialized project",
			primary.Name, primary.Path,
		)
	}

	// Ensure the agent_sessions / agent_messages tables exist.
	// Idempotent — safe on every TUI launch. Failure is non-fatal:
	// the TUI still works without persistence.
	if err := session.EnsureSchema(asGraphDB(db)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent session schema: %v\n", err)
	}

	// Resume-on-boot prompt. If the previous run died without calling
	// End() — TUI SIGKILL'd, terminal closed, machine crashed — its
	// agent_sessions row stays 'active'. Offer to pick it up if it had
	// a message in the last 30 minutes. Only fires when the user hasn't
	// already passed --session explicitly. Stdin is a terminal here
	// (the !isTerminal early-return above guarantees it).
	if resumeSessionID == "" {
		const resumeWindow = 30 * time.Minute
		// Run the lookup in a goroutine with a hard 1s timeout. On a
		// memory-starved system the DB query can take long enough to
		// be killed by the kernel before it returns; we'd rather miss
		// a resume opportunity than block startup forever or get
		// SIGKILL'd during boot. The query itself is now cheap (see
		// session.FindRecent), but the timeout stays as a safety net.
		type recentResult struct {
			recent *session.RecentSession
			err    error
		}
		ch := make(chan recentResult, 1)
		go func() {
			r, e := session.FindRecent(asGraphDB(db), primary.Path, resumeWindow)
			ch <- recentResult{r, e}
		}()
		var recent *session.RecentSession
		select {
		case r := <-ch:
			if r.err != nil {
				fmt.Fprintf(os.Stderr, "warning: session.FindRecent: %v\n", r.err)
			}
			recent = r.recent
		case <-time.After(1 * time.Second):
			fmt.Fprintln(os.Stderr, "warning: session.FindRecent timed out — skipping resume prompt")
		}
		if recent != nil {
			fmt.Fprintf(os.Stderr,
				"Found a session from %s ago (%d messages, task=%q).\nResume? [Y/n] ",
				roundDuration(recent.LastMessageAge), recent.MessageCount, recent.TaskName,
			)
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			line = strings.ToLower(strings.TrimSpace(line))
			if line == "" || line == "y" || line == "yes" {
				resumeSessionID = recent.ID
			}
		}
	}

	// Live sync setup: best-effort. If `kai live on` was run earlier,
	// subscribe a fresh channel for this TUI session so the agent's
	// edits broadcast in real time. If anything's missing (no remote,
	// no auth, sync not enabled), we just skip — the TUI still works
	// without live sync.
	liveSync, err := setupLiveSync(kaiDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if liveSync != nil {
		defer liveSync.Stop()
	}

	// MainRepo must align with the primary project's DB, not cwd.
	// In a multi-root container layout (cwd is the holding dir, db
	// belongs to one of the sub-projects), passing cwd here makes
	// downstream `kai capture` write to a different .kai/ than the
	// orchestrator queries — surfaces as the alignment-guard error
	// or, before the guard existed, as cryptic SQL errors at
	// integrate time. primary.Path is always the dir whose .kai/
	// matches `db`, by construction. Single-project layouts are
	// unaffected: primary.Path == cwd in that case.
	planner := buildPlannerServices(asGraphDB(db), kaiDir, primary.Path, liveSync, set)
	if planner != nil {
		// Pass the binary's version through so the startup banner
		// can show "kai v0.16.0" instead of "kai vdev". Single
		// source of truth lives in main.go's Version var.
		planner.Version = Version
	}
	return tui.Run(context.Background(), tui.Options{
		Projects:        set,
		DB:              asGraphDB(db),
		KaiDir:          kaiDir,
		WorkDir:         primary.Path,
		Planner:         planner,
		ResumeSessionID: resumeSessionID,
	})
}

// buildPlannerServices wires up the engine handles the REPL needs for
// natural-language input.
//
// LLM completions route through kailab-control's POST /api/v1/llm/messages
// rather than calling api.anthropic.com directly. That means the user
// must be logged in (`kai auth login`) — they don't need a personal
// ANTHROPIC_API_KEY. Returns nil with a warning if login is missing
// or any required config can't load; the REPL then falls back to
// shellout-only mode.
func buildPlannerServices(db *graph.DB, kaiDir, workDir string, liveSync *liveSyncWiring, set *projects.Set) *views.PlannerServices {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		// Bad yaml shouldn't block the TUI; log to stderr and skip
		// the planner path. The user can still use shellout commands.
		fmt.Fprintf(os.Stderr, "warning: %v (planner disabled)\n", err)
		return nil
	}

	// Apply CLI flag overrides for model selection.
	if PlannerModelFlag != "" {
		cfg.Planner.Model = PlannerModelFlag
	}
	if AgentModelFlag != "" {
		cfg.Agent.Model = AgentModelFlag
	}

	gateCfg, err := safetygate.LoadConfig(kaiDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (planner disabled)\n", err)
		return nil
	}

	// Provider selection: KAI_PROVIDER env var picks between
	// kailab (default), anthropic, openai. kailab needs login
	// credentials; the other two need their respective API keys.
	// We load creds optimistically — they're only required when
	// kailab is the chosen provider.
	creds, _ := remote.LoadCredentials()
	var kailabBase, kailabToken string
	if creds != nil {
		kailabBase = creds.ServerURL
		if t, terr := remote.GetValidAccessToken(); terr == nil {
			kailabToken = t
		}
	}
	// Seed the model picker's catalog from kailab. Best-effort: if
	// the server is older or unreachable, the picker silently falls
	// back to its hardcoded `fallbackModels` list. We don't block
	// TUI startup on this — a network blip shouldn't keep the user
	// from coding. Runs in the background so the round-trip doesn't
	// stall the spinner-less startup window.
	if kailabBase != "" {
		go seedModelCatalog(kailabBase)
	}
	pcfg := provider.FromEnv(kailabBase, kailabToken, cfg.Planner.Model)
	prov, perr := provider.New(pcfg)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (planner disabled)\n", perr)
		return nil
	}

	// Role models: kai routes each kind of LLM work to a fit-for-
	// purpose model — a strong classifier decides chat-vs-code, QWEN
	// reasons and converses, GLM writes code (see internal/config
	// Default()). This split assumes the kailab proxy, which fronts
	// all three families behind one bearer. For BYOM providers
	// (KAI_PROVIDER=anthropic/openai) those model ids aren't valid,
	// so every role collapses to the single provider-resolved model,
	// preserving pre-split behavior. Each role is overridable via its
	// KAI_*_MODEL env var.
	classifierModel := pcfg.Model
	plannerModel := pcfg.Model
	chatModel := pcfg.Model
	reviewModel := pcfg.Model
	agentModel := pcfg.Model
	// PlannerFinalizeModel is the OPTIONAL fast-writer override for
	// the planner's terminal JSON emission turn (see PlannerAgent.
	// FinalizeModel). Empty means "use the same model as planner
	// exploration." Wired purely via env — no config field — because
	// it's an experimental knob for latency tuning; the default
	// behavior (no swap) matches the prior shape exactly.
	plannerFinalizeModel := ""
	if pcfg.Kind == provider.KindKailab {
		classifierModel = modelFromEnv("KAI_CLASSIFIER_MODEL", cfg.Classifier.Model)
		plannerModel = modelFromEnv("KAI_PLANNER_MODEL", cfg.Planner.Model)
		chatModel = modelFromEnv("KAI_CHAT_MODEL", cfg.Chat.Model)
		reviewModel = modelFromEnv("KAI_REVIEW_MODEL", cfg.Review.Model)
		agentModel = modelFromEnv("KAI_AGENT_MODEL", cfg.Agent.Model)
		plannerFinalizeModel = modelFromEnv("KAI_PLANNER_FINALIZE_MODEL", "")
	}

	// Smart default: when the planner model is reasoning-class
	// (DeepSeek-V4-Pro, Qwen3, …) and the user hasn't overridden the
	// finalize model, route the single-shot JSON finalize turn to a
	// fast non-reasoning writer (GLM-5.1). A reasoning model spends its
	// token budget on the hidden <think> step and returns an EMPTY
	// completion on that schema-constrained turn, triggering
	// MaxTokens-bumped retries. Mirrors the runner's empty-completion
	// fallback chain.
	if plannerFinalizeModel == "" && provider.IsReasoningModel(plannerModel) {
		plannerFinalizeModel = "z-ai/glm-5.1"
	}

	// The planner's own LLM completer (single-shot JSON) currently
	// requires the kailab proxy. When the user picks a non-kailab
	// provider, fall back to the agent-driven planner path which
	// uses `prov` directly. The single-shot path is preserved when
	// kailab credentials are present so existing default behavior
	// is identical.
	var llm planner.Completer
	if kailabBase != "" && kailabToken != "" {
		llm = planner.NewServerCompleter(kailabBase, kailabToken, cfg.Planner.Model)
	}

	return &views.PlannerServices{
		DB:         db,
		LLM:        llm,
		GateConfig: gateCfg,
		// Per-role models resolved above. The chat agent, planner
		// agent, and classifier each read these; the code agents use
		// OrchestratorCfg.AgentModel.
		ClassifierModel:      classifierModel,
		PlannerModel:         plannerModel,
		PlannerFinalizeModel: plannerFinalizeModel,
		ChatModel:            chatModel,
		ReviewModel:          reviewModel,
		HandsOff:             handsOffEnabled(cfg),
		PlannerCfg: planner.Config{
			Model:     cfg.Planner.Model,
			MaxAgents: cfg.Planner.MaxAgents,
		},
		OrchestratorCfg: orchestrator.Config{
			AgentTimeout:     time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
			AgentIdleTimeout: time.Duration(cfg.Agent.IdleTimeoutSeconds) * time.Second,
			GateConfig:       gateCfg,
			// Provider chosen via KAI_PROVIDER (kailab default).
			// All three implementations share the same Provider
			// interface, so the runner is provider-agnostic.
			AgentProvider: prov,
			AgentModel:    agentModel,
			// kai_consult escalation model. Default to Opus 4.7 —
			// strongest available; the whole point of escalation is
			// "get the best diagnosis when stuck." KAI_CONSULT_MODEL
			// overrides (e.g. =claude-sonnet-4-6 to trade quality for
			// ~5× lower cost); =off disables the tool entirely.
			ConsultModel: consultModelFromEnv(),
			// kai_web_search auth — read by the runner and threaded
			// into the tool registry. Both must be set for the tool
			// to register; either missing silently omits.
			KailabBaseURL: kailabBase,
			KailabToken:   kailabToken,
			// Pass the main repo's graph DB so the in-process runner
			// can register kai_callers / kai_dependents / kai_context
			// as native agent tools.
			MainGraph: db,
			// Multi-root projects.Set. When this has >1 project, the
			// orchestrator's spawn path materializes every project
			// into the spawn dir so the agent can read+edit across
			// projects (the "user invoked from kai-server but the
			// bug is in kai-cli" case). nil / single-project sets
			// fall back to the single-root spawnFor path.
			Projects: set,
			// LiveSync, when set, broadcasts every agent file write
			// to kailab so other clients on the same channel see the
			// change in real time. nil if `kai live on` wasn't run
			// or live-sync setup failed (we just skip rather than
			// blocking the TUI).
			LiveSync: orchLiveSync(liveSync),
			// Bash tool: on by default for the in-process runner so
			// agents can run tests, build, lint, etc. Allowlist
			// (optional) comes from .kai/config.yaml's agent.bash_allow.
			AgentBashEnabled: true,
			AgentBashAllow:   cfg.Agent.BashAllow,
			// Auto-test: after a coding agent applies edits to
			// non-test source, the orchestrator runs a follow-up
			// test-coverage pass. Default on; opt out by setting
			// agent.auto_test: false in .kai/config.yaml.
			AutoTest: cfg.Agent.AutoTest,
			// Session persistence: pass the same DB the graph uses
			// so agent conversations land in <kaiDir>/db.sqlite.
			// One backup story, one migration story.
			AgentSessionStore: db,
			// TUI default: clean up successful spawns so /tmp doesn't
			// accumulate. Failed runs are kept regardless so the user
			// can read agent.log post-mortem (the orchestrator skips
			// despawn on ExitErr / IntegrateErr).
			Despawn: true,
			PromptContext: agentprompt.Context{
				RepoRoot:    workDir,
				Roots:       promptRootsFromSet(set),
				Protected:   gateCfg.Protected,
				ModuleRoots: agentprompt.DetectModuleRoots(workDir),
			},
		},
		PromptCtx: agentprompt.Context{
			RepoRoot:    workDir,
			Roots:       promptRootsFromSet(set),
			Protected:   gateCfg.Protected,
			ModuleRoots: agentprompt.DetectModuleRoots(workDir),
		},
		MainRepo: workDir,
		KaiDir:   kaiDir,
		Projects: set,
		// Fixxy worker is started ONLY when the secret
		// --fixxy-upper flag was passed. nil otherwise (and
		// fixxy.Worker.Trigger is nil-safe so REPL hooks
		// don't have to nil-check).
		Fixxy:     buildFixxyWorker(),
		FixxyMode: fixxy.Mode(fixxyUpper),
	}
}

// buildFixxyWorker constructs the secret fixxy-upper worker
// when --fixxy-upper was passed; returns nil otherwise. Repo
// path comes from KAI_REPO env or defaults to ~/projects/kai/kai
// (the user's standard checkout location). Binary path is
// resolved from the running kai's argv[0] so the rebuild
// overwrites whatever binary the user actually has on PATH.
func buildFixxyWorker() *fixxy.Worker {
	if fixxyUpper == 0 {
		return nil
	}
	repo := os.Getenv("KAI_REPO")
	if repo == "" {
		home, _ := os.UserHomeDir()
		repo = filepath.Join(home, "projects", "kai", "kai")
	}
	binPath, err := os.Executable()
	if err != nil || binPath == "" {
		// Without a resolvable binary path the rebuild step
		// has nowhere to write. Falling back to "kai" on PATH
		// works for most users but may surprise people running
		// from a custom build.
		binPath = "kai"
	}
	return fixxy.New(repo, binPath)
}

// promptRootsFromSet flattens a Set into the agentprompt.RootInfo
// slice the prompt template expects. nil/empty Set → nil, which the
// template treats as "single-root, omit the multi-root block."
func promptRootsFromSet(set *projects.Set) []agentprompt.RootInfo {
	if set == nil || len(set.Projects()) <= 1 {
		return nil
	}
	out := make([]agentprompt.RootInfo, 0, len(set.Projects()))
	for _, p := range set.Projects() {
		out = append(out, agentprompt.RootInfo{Name: p.Name, Path: p.Path})
	}
	return out
}

// asGraphDB unwraps openDB's return into a *graph.DB pointer. openDB
// returns an interface; the TUI needs the concrete type for in-process
// engine calls.
func asGraphDB(db interface{}) *graph.DB {
	if g, ok := db.(*graph.DB); ok {
		return g
	}
	return nil
}

// isTerminal reports whether both stdin and stdout are connected to
// a TTY. Both must be true: stdout-only TTY would mean piped input
// (e.g. `echo foo | kai`); stdin-only would mean redirected output.
func isTerminal() bool {
	stdoutFd := os.Stdout.Fd()
	stdinFd := os.Stdin.Fd()
	return (isatty.IsTerminal(stdoutFd) || isatty.IsCygwinTerminal(stdoutFd)) &&
		(isatty.IsTerminal(stdinFd) || isatty.IsCygwinTerminal(stdinFd))
}

// bootstrapProject runs `kai init` then `kai capture -m "first
// snapshot"` against dir. Used by runCodeTUI when the user agrees
// to set up a project from an empty / uninitialized directory.
//
// We shell out (rather than calling the in-process functions) for
// two reasons: (a) `kai init` and `kai capture` already handle all
// the corner cases (existing .kai/, git repo detection, etc.) and
// re-implementing them here would duplicate ~500 lines; (b) it's
// the same path the user would have run by hand, so behavior is
// identical and any future kai-init improvements land here for
// free.
//
// Output is forwarded so the user sees progress and can spot any
// warnings the init/capture flow surfaces.
func bootstrapProject(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		exe = "kai" // fallback to PATH lookup
	}
	// `--yes` on init: the TUI auto-bootstrap must never block on an
	// interactive prompt. The org-selection / repo-exists prompts
	// print to stderr *before* the Bubble Tea alt-screen takes over,
	// so a user launching `kai code` in a fresh dir would see a
	// half-painted terminal hang with no visible question. Non-
	// interactive init picks KAI_ORG or the personal org instead.
	for _, args := range [][]string{
		{"init", "--yes"},
		{"capture", "-m", "kai code: first snapshot"},
	} {
		c := exec.Command(exe, args...)
		c.Dir = dir
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// seedModelCatalog fetches /api/v1/models from kailab and seeds the
// view layer's in-memory catalog so the /model picker reflects the
// server's actual support matrix (not just the hardcoded fallback
// list). Best-effort: any error silently leaves the fallback in
// place. Runs once at TUI startup; the catalog is small (~10 rows)
// and stable enough that we don't refetch.
//
// Called in a goroutine so a slow kailab response can't block the
// alt-screen entering. Worst case: the picker shows the fallback
// list for a turn or two, then the catalog lands. Race: SetCatalog
// is mutex-protected; concurrent reads from the picker are safe.
func seedModelCatalog(kailabBase string) {
	c := remote.NewClient(kailabBase, "", "")
	entries, err := c.ListModels()
	if err != nil {
		// Silent — fallback list keeps working. Log only when the
		// debug env is set so normal users aren't spammed.
		if os.Getenv("KAI_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "kai: model catalog fetch failed (using fallback): %v\n", err)
		}
		return
	}
	// Translate the wire shape into the views package's local
	// mirror struct. We don't import the remote package's struct
	// directly into views/ because that would make views depend
	// on remote, which inverts the dependency direction (views
	// is consumed by cmd/, not the other way around).
	mirror := make([]views.RemoteCatalogEntry, 0, len(entries))
	for _, e := range entries {
		mirror = append(mirror, views.RemoteCatalogEntry{
			ID:       e.ID,
			Provider: e.Provider,
			Tier:     e.Tier,
		})
	}
	views.SetCatalog(mirror)
}

// roundDuration renders a Duration in a human shape ("3m", "42s",
// "1h12m") for the resume-on-boot prompt. time.Duration.String alone
// gives "3m0.123456s" which reads poorly to a human picking Y/n.
func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
