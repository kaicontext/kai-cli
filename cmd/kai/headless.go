package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kai/internal/agent/provider"
	"kai/internal/agent/session"
	"kai/internal/agentprompt"
	"kai/internal/config"
	"kai/internal/orchestrator"
	"kai/internal/planner"
	"kai/internal/projects"
	"kai/internal/remote"
	"kai/internal/safetygate"
)

// runCodeHeadless drives one planner+execute cycle without launching
// the TUI. Used by `kai code -p "task"` for cost-validation
// benchmarking: pair with `kai run summary` for a per-turn dollar /
// cache-reuse readout. Mirrors runCodeTUI's setup (container
// invariant, project discovery, planner services) but ends with a
// printed summary and exits instead of handing off to Bubble Tea.
//
// Held integrations are left held — gate review is interactive by
// design. The headless path's job is to reproduce the cost surface,
// not to also approve work.
func runCodeHeadless(ctx context.Context, prompt string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, _ := os.Getwd()

	if err := projects.CheckContainerInvariant(cwd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fmt.Errorf("refusing to launch: container/project invariant violated")
	}

	set, outcome := projects.Discover(cwd)
	switch outcome {
	case projects.OutcomeContainer:
		return fmt.Errorf("%s is a container of projects; cd into one and rerun", cwd)
	case projects.OutcomeEmpty, projects.OutcomeUninitProject:
		if _, err := os.Stat(filepath.Join(cwd, projects.ProjectsFileName)); err == nil {
			return fmt.Errorf("refusing to auto-init: %s exists at cwd", projects.ProjectsFileName)
		}
		fmt.Fprintf(os.Stderr, "kai code -p: setting up project (one-time)…\n")
		if err := bootstrapProject(cwd); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		set, outcome = projects.Discover(cwd)
		if outcome != projects.OutcomeRootsFound {
			return fmt.Errorf("kai init succeeded but rediscovery still found nothing")
		}
	case projects.OutcomeRootsFound:
	}

	if err := set.Open(); err != nil {
		return fmt.Errorf("opening projects: %w", err)
	}
	defer set.Close()

	primary := set.Primary()
	kaiDir = primary.KaiDir
	db := primary.DB

	if err := session.EnsureSchema(asGraphDB(db)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent session schema: %v\n", err)
	}

	cfg, err := config.Load(kaiDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	gateCfg, err := safetygate.LoadConfig(kaiDir)
	if err != nil {
		return fmt.Errorf("safety gate config: %w", err)
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
		return fmt.Errorf("provider: %w", perr)
	}

	// Role models — same split the TUI uses (see buildPlannerServices
	// in tui.go). The headless path runs planner + execute only, so
	// it needs the planner model (QWEN) and the code agent model
	// (DeepSeek); chat / classifier roles don't apply here. BYOM providers
	// collapse every role to the single provider-resolved model.
	plannerModel := pcfg.Model
	agentModel := pcfg.Model
	if pcfg.Kind == provider.KindKailab {
		plannerModel = modelFromEnv("KAI_PLANNER_MODEL", cfg.Planner.Model)
		agentModel = modelFromEnv("KAI_AGENT_MODEL", cfg.Agent.Model)
	}

	promptCtx := agentprompt.Context{
		RepoRoot:    primary.Path,
		Roots:       promptRootsFromSet(set),
		Protected:   gateCfg.Protected,
		ModuleRoots: agentprompt.DetectModuleRoots(primary.Path),
	}

	pa := &planner.PlannerAgent{
		Provider:     prov,
		Model:        plannerModel,
		Set:          set,
		GateConfig:   gateCfg,
		Cfg:          planner.Config{Model: cfg.Planner.Model, MaxAgents: cfg.Planner.MaxAgents},
		SessionStore: asGraphDB(db),
		RunLogDir:    kaiDir,
	}

	fmt.Fprintf(os.Stderr, "kai code -p: planning…\n")
	t0 := time.Now()
	pres, err := pa.Run(ctx, prompt, "")
	plannerElapsed := time.Since(t0)
	if err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  planner: %s, in=%d out=%d cache_create=%d cache_read=%d\n",
		plannerElapsed.Round(time.Millisecond),
		pres.TokensIn, pres.TokensOut, pres.TokensCacheCreate, pres.TokensCacheRead)

	if pres.Plan == nil || len(pres.Plan.Agents) == 0 {
		if pres.Reply != "" {
			fmt.Println(pres.Reply)
			return nil
		}
		fmt.Fprintln(os.Stderr, "planner produced no plan (empty agents list)")
		return nil
	}

	fmt.Fprintf(os.Stderr, "kai code -p: executing %d agent(s)…\n", len(pres.Plan.Agents))
	orchCfg := orchestrator.Config{
		AgentTimeout:      time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
		AgentIdleTimeout:  time.Duration(cfg.Agent.IdleTimeoutSeconds) * time.Second,
		GateConfig:        gateCfg,
		AgentProvider:     prov,
		AgentModel:        agentModel,
		MainGraph:         asGraphDB(db),
		KailabBaseURL:     kailabBase,
		KailabToken:       kailabToken,
		AgentBashEnabled:  true,
		AgentBashAllow:    cfg.Agent.BashAllow,
		AgentSessionStore: asGraphDB(db),
		Despawn:           true,
		PromptContext:     promptCtx,
		RunLogDir:         kaiDir,
	}
	t0 = time.Now()
	res, err := orchestrator.Execute(ctx, pres.Plan, orchCfg, asGraphDB(db), primary.Path, kaiDir)
	execElapsed := time.Since(t0)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  execute: %s\n", execElapsed.Round(time.Millisecond))
	fmt.Printf("agents=%d auto_promoted=%d held=%d failed=%d\n",
		len(res.Runs), res.AutoPromoted, res.Held, res.Failed)
	for _, r := range res.Runs {
		status := "ok"
		if r.ExitErr != nil {
			status = "exit_err"
		} else if r.IntegrateErr != nil {
			status = "integrate_err"
		} else if r.Verdict != nil && r.Verdict.Verdict != "auto" {
			status = r.Verdict.Verdict
		}
		fmt.Printf("  %s: %s\n", r.Task.Name, status)
	}
	fmt.Fprintln(os.Stderr, "tip: run `kai run summary` to see the cost row.")
	return nil
}
