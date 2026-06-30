package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/session"
	"github.com/kaicontext/kai-engine/agentprompt"
	"kai/internal/config"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/orchestrator"
	"github.com/kaicontext/kai-engine/planner"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/remote"
	"github.com/kaicontext/kai-engine/safetygate"
)

// agentServices bundles everything the headless paths need to plan and
// run an agent task against the current project: the opened project set,
// the LLM provider, the role models, and the gate/prompt context. Both
// `kai code -p` (runCodeHeadless) and `kai autofix` build one of these so
// the subtle setup (provider resolution, role-model split, container
// invariant) lives in exactly one place.
type agentServices struct {
	set       *projects.Set
	primary   *projects.Project
	gdb       *graph.DB
	prov      provider.Provider
	cfg       config.Config
	gateCfg   safetygate.Config
	promptCtx agentprompt.Context

	plannerModel string
	agentModel   string

	// executorMaxTurns, when > 0, overrides the orchestrator's per-executor
	// turn cap. Headless callers set it so a slower model isn't cut off
	// mid-fix; 0 keeps the default.
	executorMaxTurns int
}

// buildAgentServices discovers and opens the project at cwd, resolves the
// provider and role models, and returns a ready-to-run services bundle.
// The caller must call Close when done. autoBootstrap mirrors
// runCodeHeadless's one-time project setup; autofix passes false because
// it expects to run inside an already-initialized repo.
//
// singleRoot narrows a discovered multi-root workspace down to just the
// project that owns cwd. Autofix passes true: it operates on ONE git repo
// (it branches cwd's repo, pushes one branch, opens one PR), so the
// planner, executor, and verify step must all root at THAT repo and emit
// repo-relative paths. `kai code -p` passes false because cross-project
// planning over the whole workspace is a wanted feature there.
func buildAgentServices(ctx context.Context, cwd string, autoBootstrap, singleRoot bool) (*agentServices, error) {
	if err := projects.CheckContainerInvariant(cwd); err != nil {
		return nil, fmt.Errorf("container/project invariant violated: %w", err)
	}

	set, outcome := projects.Discover(cwd)
	switch outcome {
	case projects.OutcomeContainer:
		return nil, fmt.Errorf("%s is a container of projects; cd into one and rerun", cwd)
	case projects.OutcomeEmpty, projects.OutcomeUninitProject:
		if !autoBootstrap {
			return nil, fmt.Errorf("%s has no initialized kai project; run `kai init` first", cwd)
		}
		if _, err := os.Stat(filepath.Join(cwd, projects.ProjectsFileName)); err == nil {
			return nil, fmt.Errorf("refusing to auto-init: %s exists at cwd", projects.ProjectsFileName)
		}
		if err := bootstrapProject(cwd); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		set, outcome = projects.Discover(cwd)
		if outcome != projects.OutcomeRootsFound {
			return nil, fmt.Errorf("kai init succeeded but rediscovery still found nothing")
		}
	case projects.OutcomeRootsFound:
	}

	// Single-repo callers (autofix) must run entirely single-root.
	// Discover deliberately walks UP to a parent kai.projects.yaml and
	// returns the full multi-root Set — correct for `kai code`. But in a
	// multi-root Set the planner prompt and graph overview switch to
	// workspace-relative, project-prefixed paths (`cd kai-tui && go test`,
	// `kai-tui/internal/...`; see the len(Projects())>1 branches in
	// planner/agent_planner.go and agent/graph_context.go) while the
	// executor and runVerifyChecks run at mainRepo = the sub-project. The
	// two then disagree by one directory level and every edit/verify
	// fails. Narrowing to the project that owns cwd makes the whole run
	// self-consistent. Done BEFORE Open so only the one DB is opened and
	// promptCtx.Roots reflects the narrowed set.
	if singleRoot {
		narrowed, err := narrowToOwner(set, cwd)
		if err != nil {
			return nil, err
		}
		set = narrowed
	}

	if err := set.Open(); err != nil {
		return nil, fmt.Errorf("opening projects: %w", err)
	}

	primary := set.Primary()
	kaiDir = primary.KaiDir
	gdb := asGraphDB(primary.DB)

	if err := session.EnsureSchema(gdb); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent session schema: %v\n", err)
	}

	cfg, err := config.Load(kaiDir)
	if err != nil {
		set.Close()
		return nil, fmt.Errorf("config: %w", err)
	}
	gateCfg, err := safetygate.LoadConfig(kaiDir)
	if err != nil {
		set.Close()
		return nil, fmt.Errorf("safety gate config: %w", err)
	}

	creds, _ := remote.LoadCredentials()
	var kailabBase, kailabToken string
	if creds != nil {
		kailabBase = creds.ServerURL
		if t, terr := remote.GetValidAccessToken(); terr == nil {
			kailabToken = t
		}
	}
	pcfg := provider.FromEnv(kailabBase, kailabToken, cfg.Planner.Model)
	prov, perr := provider.New(pcfg)
	if perr != nil {
		set.Close()
		return nil, fmt.Errorf("provider: %w", perr)
	}

	plannerModel := pcfg.Model
	agentModel := pcfg.Model
	if pcfg.Kind == provider.KindKailab {
		plannerModel = modelFromEnv("KAI_PLANNER_MODEL", cfg.Planner.Model)
		agentModel = modelFromEnv("KAI_AGENT_MODEL", cfg.Agent.Model)
	}

	return &agentServices{
		set:     set,
		primary: primary,
		gdb:     gdb,
		prov:    prov,
		cfg:     cfg,
		gateCfg: gateCfg,
		promptCtx: agentprompt.Context{
			RepoRoot:    primary.Path,
			Roots:       promptRootsFromSet(set),
			Protected:   gateCfg.Protected,
			ModuleRoots: agentprompt.DetectModuleRoots(primary.Path),
		},
		plannerModel: plannerModel,
		agentModel:   agentModel,
	}, nil
}

// narrowToOwner collapses a (possibly multi-root) discovered Set down to
// a single-root Set containing only the project that owns cwd. It returns
// an unopened Set rooted at that project's path, so a subsequent Open
// touches just the one DB. Used by single-repo callers (autofix) to keep
// the planner, executor, and verify step rooted at the same repo.
//
// Owner selection reuses Set.Primary, which routes by the InvokedFrom
// that Discover records (longest-prefix match on cwd). Errors when cwd
// sits in no project — the container case, where a single-repo run makes
// no sense.
func narrowToOwner(set *projects.Set, cwd string) (*projects.Set, error) {
	if set == nil {
		return nil, fmt.Errorf("no projects discovered at %s", cwd)
	}
	owner := set.Primary()
	if owner == nil {
		return nil, fmt.Errorf("%s is not inside any initialized kai project", cwd)
	}
	if len(set.Projects()) == 1 {
		return set, nil // already single-root; nothing to narrow
	}
	return projects.New(owner.Path, []*projects.Project{owner}), nil
}

// Close releases the project DB handles.
func (s *agentServices) Close() {
	if s != nil && s.set != nil {
		s.set.Close()
	}
}

// kailabCreds re-derives the kailab base/token the orchestrator config
// wants. Cheap and side-effect-free; keeps the struct lean.
func kailabCreds() (base, token string) {
	creds, _ := remote.LoadCredentials()
	if creds != nil {
		base = creds.ServerURL
		if t, terr := remote.GetValidAccessToken(); terr == nil {
			token = t
		}
	}
	return base, token
}

// runAgentTask plans the prompt into a WorkPlan and executes it, returning
// the orchestrator result. It is the shared core of the headless paths.
// Progress lines go to stderr (callers may add their own framing).
func (s *agentServices) runAgentTask(ctx context.Context, prompt string) (*orchestrator.Result, *planner.PlannerResult, error) {
	pa := &planner.PlannerAgent{
		Provider:     s.prov,
		Model:        s.plannerModel,
		Set:          s.set,
		GateConfig:   s.gateCfg,
		Cfg:          planner.Config{Model: s.cfg.Planner.Model, MaxAgents: s.cfg.Planner.MaxAgents},
		SessionStore: s.gdb,
		RunLogDir:    kaiDir,
	}
	pres, err := pa.Run(ctx, prompt, "")
	if err != nil {
		return nil, nil, fmt.Errorf("planner: %w", err)
	}
	if pres.Plan == nil || len(pres.Plan.Agents) == 0 {
		return nil, pres, nil // nothing to execute (planner replied instead)
	}

	kailabBase, kailabToken := kailabCreds()
	orchCfg := orchestrator.Config{
		AgentTimeout:      time.Duration(s.cfg.Agent.TimeoutSeconds) * time.Second,
		AgentIdleTimeout:  time.Duration(s.cfg.Agent.IdleTimeoutSeconds) * time.Second,
		GateConfig:        s.gateCfg,
		AgentProvider:     s.prov,
		AgentModel:        s.agentModel,
		MainGraph:         s.gdb,
		KailabBaseURL:     kailabBase,
		KailabToken:       kailabToken,
		AgentBashEnabled:  true,
		AgentBashAllow:    s.cfg.Agent.BashAllow,
		AgentSessionStore: s.gdb,
		Despawn:           true,
		PromptContext:     s.promptCtx,
		RunLogDir:         kaiDir,
		ExecutorMaxTurns:  s.executorMaxTurns,
	}
	res, err := orchestrator.Execute(ctx, pres.Plan, orchCfg, s.gdb, s.primary.Path, kaiDir)
	if err != nil {
		return nil, pres, fmt.Errorf("orchestrator: %w", err)
	}
	return res, pres, nil
}

// judge runs a single non-agentic LLM turn (no tools) and returns the
// model's text. Used by autofix for the semantic judge and the reviewer —
// both are one-shot "read this, answer in this format" calls, not loops.
func (s *agentServices) judge(ctx context.Context, system, user string) (string, int, int, error) {
	resp, err := s.prov.Send(ctx, provider.Request{
		Model:  s.agentModel,
		System: system,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: user}},
		}},
		MaxTokens: 1024,
	})
	if err != nil {
		return "", 0, 0, err
	}
	text := message.Message{Parts: resp.Parts}.Text()
	return text, resp.InputTokens, resp.OutputTokens, nil
}
