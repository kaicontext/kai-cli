package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"kai/internal/config"

	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/reviewanalyze"
	"github.com/kaicontext/kai-engine/util"
)

var (
	analyzeFormat   string
	analyzeContract string
)

var reviewAnalyzeCmd = &cobra.Command{
	Use:   "analyze <review-id>",
	Short: "Emit the structured Finding document for a review (headless)",
	Long: `Assemble the review Finding — verdict, intent-vs-code, blast radius, and diff —
as a single JSON document. This is the headless surface the TUI and the server
web UI both render; it serializes data the engine already computed.

The intent-vs-code panel is enriched from a verification contract when one can be
resolved (a single open contract, or --contract <id>). Grounded claims and
should-touch are emitted empty until the graph-grounded-evidence layer lands.

Examples:
  kai review analyze kai-204 --format json
  kai review analyze kai-204 --format json --contract rotate-session-keys`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewAnalyze,
}

// runReviewAnalyze is a thin CLI wrapper over reviewanalyze.Analyze: it opens the
// local store, builds the should-touch LLM judge from client config, runs the
// shared analyzer, and prints the Finding as JSON. The analysis itself lives in
// github.com/kaicontext/kai-engine/reviewanalyze so the server-side (shard) path
// can call it identically.
func runReviewAnalyze(cmd *cobra.Command, args []string) error {
	if analyzeFormat != "json" {
		return fmt.Errorf("unsupported --format %q (only \"json\" is supported)", analyzeFormat)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	judge, model := judgeProvider()

	f, err := reviewanalyze.Analyze(cmd.Context(), db, args[0], reviewanalyze.Options{
		Contract:    analyzeContract,
		ContractDir: kaiDir,
		Judge:       judge,
		JudgeModel:  model,
	})
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling finding: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

var reviewAnalyzeSnapshotsCmd = &cobra.Command{
	Use:   "analyze-snapshots <base> <head>",
	Short: "Emit the Finding for a base→head snapshot pair (no review needed)",
	Long: `Assemble the review Finding directly from two snapshots — the same headless
JSON as 'review analyze', but for an arbitrary base→head pair instead of a
persisted review. This is the entry the server-side review path uses after
resolving a PR's base/head to snapshots; exposed on the CLI for parity/testing.

Examples:
  kai review analyze-snapshots @snap:prev @snap:last
  kai review analyze-snapshots git.<baseSHA> git.<headSHA>`,
	Args: cobra.ExactArgs(2),
	RunE: runReviewAnalyzeSnapshots,
}

func runReviewAnalyzeSnapshots(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	baseID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving base %q: %w", args[0], err)
	}
	headID, err := resolveSnapshotID(db, args[1])
	if err != nil {
		return fmt.Errorf("resolving head %q: %w", args[1], err)
	}

	judge, model := judgeProvider()

	f, err := reviewanalyze.AnalyzeSnapshots(cmd.Context(), db, util.BytesToHex(baseID), util.BytesToHex(headID),
		reviewanalyze.Meta{ID: "snap", Title: fmt.Sprintf("%s..%s", args[0], args[1])},
		reviewanalyze.Options{
			ContractDir: kaiDir,
			Judge:       judge,
			JudgeModel:  model,
		})
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling finding: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

// judgeProvider builds the LLM provider for the should-touch judge, reusing the
// gate/planner plumbing (kailab credentials → OpenRouter when logged in, else an
// ANTHROPIC_API_KEY fallback). Returns (nil, "") when no provider is available,
// so the judge degrades to emitting no should-touch findings.
func judgeProvider() (provider.Provider, string) {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		return nil, ""
	}
	prov, reviewModel, _, err := buildGateProvider(cfg)
	if err != nil {
		return nil, ""
	}
	return prov, reviewModel
}
