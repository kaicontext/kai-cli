package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/contract"

	"github.com/spf13/cobra"
)

// Verification-layer contract commands (Horizon 1, Phase 1 — manual only).
// `kai intent` opens a contract; `kai contract ...` inspects and manages them.
// The daemon, classifier, and semantic layer arrive in later phases; for now
// these surface and edit the persisted contract store.

var contractCmd = &cobra.Command{
	Use:   "contract",
	Short: "Inspect and manage verification contracts",
	Long: "A contract is a living, folded statement of desired end-state with " +
		"classified prompt provenance. Verification holds the working tree " +
		"accountable to it.",
}

var contractListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all contracts in flight",
	RunE:  runContractList,
}

var contractShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show a contract: status, layers, and provenance",
	Args:  cobra.ExactArgs(1),
	RunE:  runContractShow,
}

var contractDropCmd = &cobra.Command{
	Use:   "drop <id>",
	Short: "Abandon a contract",
	Args:  cobra.ExactArgs(1),
	RunE:  runContractDrop,
}

var contractCloseCmd = &cobra.Command{
	Use:   "close <id>",
	Short: "Mark a contract intentionally done",
	Args:  cobra.ExactArgs(1),
	RunE:  runContractClose,
}

func init() {
	contractCmd.AddCommand(contractListCmd, contractShowCmd, contractDropCmd, contractCloseCmd)
	rootCmd.AddCommand(contractCmd)
}

func openContractStore() (*contract.Store, error) {
	return contract.Open(kaiDir)
}

// runIntentOpen backs `kai intent "<statement>"` — open a contract for work
// outside kit. (The `intent render` subcommand is unaffected.)
func runIntentOpen(cmd *cobra.Command, args []string) error {
	statement := strings.TrimSpace(strings.Join(args, " "))
	if statement == "" {
		return fmt.Errorf("usage: kai intent \"<statement>\"")
	}
	store, err := openContractStore()
	if err != nil {
		return err
	}
	defer store.Close()

	// CLI-opened contracts are best-effort (traced) — they're declared outside
	// the kit planner, so they carry the wider-residue source even though the
	// statement itself is explicit.
	c, err := store.Create(statement, contract.SourceTraced)
	if err != nil {
		return err
	}
	fmt.Printf("opened contract %s\n", c.ID)
	fmt.Printf("  %s\n", c.Statement)
	fmt.Printf("  status: %s · source: %s\n", c.Status.Display(), c.Source)
	fmt.Printf("\nnext: kai status   ·   kai contract show %s\n", c.ID)
	return nil
}

func runContractList(cmd *cobra.Command, args []string) error {
	store, err := openContractStore()
	if err != nil {
		return err
	}
	defer store.Close()

	cs, err := store.List()
	if err != nil {
		return err
	}
	open := cs[:0]
	for _, c := range cs {
		if !c.Closed {
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		fmt.Println("no contracts in flight")
		return nil
	}
	fmt.Printf("%d contract(s) in flight\n\n", len(open))
	for _, c := range open {
		fmt.Printf("%s %-18s %-44s %s\n", c.Status.Glyph(), c.ID, truncate(c.Statement, 44), c.Status.Display())
	}
	return nil
}

func runContractShow(cmd *cobra.Command, args []string) error {
	store, err := openContractStore()
	if err != nil {
		return err
	}
	defer store.Close()

	c, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("contract %q not found", args[0])
	}

	fmt.Printf("intent    %s\n", c.Statement)
	if len(c.Plan.Steps) > 0 {
		fmt.Printf("plan      %d steps · folded from %d prompts\n", len(c.Plan.Steps), c.Plan.FoldedFrom)
	} else {
		fmt.Printf("plan      (not folded yet)\n")
	}
	closed := ""
	if c.Closed {
		closed = " · closed"
	}
	fmt.Printf("status    %s · source: %s%s\n", c.Status.Display(), c.Source, closed)

	// Continuous layer (Phase 2 populates this; Phase 1 shows the boundary).
	fmt.Printf("\ncontinuous (deterministic, live)\n")
	if c.Continuous.RanAt == 0 {
		fmt.Printf("  (daemon not run — Phase 2)\n")
	} else {
		fmt.Printf("  typecheck: %s · tests: %s (%d)\n", boolMark(c.Continuous.Typecheck), boolMark(c.Continuous.TestsPass), c.Continuous.TestsTotal)
		for _, f := range c.Continuous.Failures {
			fmt.Printf("  ✗ %s\n", f)
		}
	}

	// Semantic layer (Phase 3).
	fmt.Printf("\nsemantic\n")
	if c.Semantic.RanAt == 0 {
		fmt.Printf("  (not run — kai verify, Phase 3)\n")
	} else {
		fmt.Printf("  last run %s\n", relTime(c.Semantic.RanAt))
		if c.Semantic.Note != "" {
			fmt.Printf("  %s\n", c.Semantic.Note)
		}
	}

	if len(c.Residue) > 0 {
		fmt.Printf("\nresidue (%d)\n", len(c.Residue))
		for _, r := range c.Residue {
			fmt.Printf("  ? %s\n", r.Prompt)
		}
	}

	fmt.Printf("\nprovenance\n")
	for _, e := range c.Provenance {
		tag := string(e.Kind)
		if !e.Applied {
			tag += " · skipped"
		}
		fmt.Printf("  %-4s %-44s %s\n", e.Ref, truncate(quote(e.Text), 44), tag)
	}
	return nil
}

func runContractDrop(cmd *cobra.Command, args []string) error {
	store, err := openContractStore()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Drop(args[0]); err != nil {
		return err
	}
	fmt.Printf("dropped contract %s\n", args[0])
	return nil
}

func runContractClose(cmd *cobra.Command, args []string) error {
	store, err := openContractStore()
	if err != nil {
		return err
	}
	defer store.Close()
	c, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("contract %q not found", args[0])
	}
	c.Closed = true
	if err := store.Save(c); err != nil {
		return err
	}
	fmt.Printf("closed contract %s (intentionally done)\n", c.ID)
	return nil
}

// --- small display helpers ---

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func quote(s string) string { return "\"" + s + "\"" }

func boolMark(b *bool) string {
	if b == nil {
		return "?"
	}
	if *b {
		return "✓"
	}
	return "✗"
}

func relTime(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
