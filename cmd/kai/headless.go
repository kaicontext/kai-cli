package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// runCodeHeadless drives one planner+execute cycle without launching
// the TUI. Used by `kai code -p "task"` for cost-validation
// benchmarking: pair with `kai run summary` for a per-turn dollar /
// cache-reuse readout. The provider/planner/orchestrator setup lives in
// buildAgentServices (shared with `kai autofix`); this wrapper keeps the
// cost-surface stderr trailer that the benchmark workflow reads.
//
// Held integrations are left held — gate review is interactive by
// design. The headless path's job is to reproduce the cost surface,
// not to also approve work.
func runCodeHeadless(ctx context.Context, prompt string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, _ := os.Getwd()

	svc, err := buildAgentServices(ctx, cwd, true)
	if err != nil {
		return err
	}
	defer svc.Close()

	fmt.Fprintf(os.Stderr, "kai code -p: planning + executing…\n")
	t0 := time.Now()
	res, pres, err := svc.runAgentTask(ctx, prompt)
	elapsed := time.Since(t0)
	if err != nil {
		return err
	}

	if pres != nil {
		fmt.Fprintf(os.Stderr, "  planner: in=%d out=%d cache_create=%d cache_read=%d\n",
			pres.TokensIn, pres.TokensOut, pres.TokensCacheCreate, pres.TokensCacheRead)
	}
	if res == nil {
		if pres != nil && pres.Reply != "" {
			fmt.Println(pres.Reply)
			return nil
		}
		fmt.Fprintln(os.Stderr, "planner produced no plan (empty agents list)")
		return nil
	}

	fmt.Fprintf(os.Stderr, "  execute: %s\n", elapsed.Round(time.Millisecond))
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
