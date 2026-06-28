package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/agent"
	"kai/internal/agent/provider"
	"github.com/kaicontext/kai-engine/session"
	"kai/internal/config"
	"kai/internal/gatereview"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/projects"
	"github.com/kaicontext/kai-engine/remote"
	"github.com/kaicontext/kai-engine/safetygate"
	"github.com/kaicontext/kai-engine/util"
	"kai/internal/workspace"
)

// `kai gate` is the human-facing surface for the safety gate's hold
// queue. When `kai integrate` produces a Review or Block verdict, the
// merged snapshot is committed to the DB but the team-visible refs
// (snap.latest, etc.) are not advanced. `kai gate list` shows what's
// held; `kai gate approve` advances the refs after human review.
//
// This is intentionally separate from `kai review`, which is the
// formal PR-style review system (Review nodes, comments, reviewers).
// The gate is a lighter-weight "did this earn auto-publish?" check.

type gateListEntry struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Verdict     string `json:"verdict"`
	BlastRadius int    `json:"blastRadius"`
	From        string `json:"from"`
	Timestamp   string `json:"timestamp"`
}

var gateCmd = &cobra.Command{
	Use:   "gate",
	Short: "Inspect and resolve safety-gate-held integrations",
	Long: `The safety gate decides whether an agent's integration auto-promotes,
needs human review, or is blocked. Held integrations stay in the
database as orphan snapshots until you approve or reject them.

Examples:
  kai gate list                    # snapshots held by the gate
  kai gate show <id>               # verdict reasons + affected files
  kai gate approve <id>            # advance the team-visible refs
  kai gate reject <id>             # mark the held snapshot dismissed`,
}

var gateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List integrations held by the safety gate",
	RunE:  runGateList,
}

var gateShowCmd = &cobra.Command{
	Use:   "show <snapshot-id>",
	Short: "Show the gate verdict for a held integration",
	Args:  cobra.ExactArgs(1),
	RunE:  runGateShow,
}

var gateDiffCmd = &cobra.Command{
	Use:   "diff <snapshot-id>",
	Short: "Show a unified diff for a held integration",
	Long: `Print a unified line-level diff between the held snapshot and the
target it would have advanced over (the pre-integration base). Same
shape as ` + "`git diff`" + `, suitable for review or piping into other tools.`,
	Args: cobra.ExactArgs(1),
	RunE: runGateDiff,
}

var gateReviewCmd = &cobra.Command{
	Use:   "review [snapshot-id]",
	Short: "AI-driven review of held integrations (summary, audit, recommendation)",
	Long: `Walk each held snapshot (or the given one) and produce a plain-English
summary, an automated audit, and a recommended action. Requires
ANTHROPIC_API_KEY. Output is human-readable and can be piped through
the TUI's /gate flow.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runGateReview,
}

var gateApproveCmd = &cobra.Command{
	Use:   "approve <snapshot-id>",
	Short: "Approve a held integration; advance the team-visible refs",
	Args:  cobra.ExactArgs(1),
	RunE:  runGateApprove,
}

var (
	gateFixAutoApprove bool
	gateFixMaxTurns    int
	gateListJSON       bool
)

var gateFixCmd = &cobra.Command{
	Use:   "fix <snapshot-id>",
	Short: "Run the fix-then-approve agent on a held integration",
	Long: `Re-runs the gate review to collect [fixable] issues, then dispatches
the in-process kai code agent to apply minimal remediations. After the
agent finishes, the workspace is re-staged and re-integrated so the
gate gets another swing. Refuses if the audit has any [human] issues
or no fixable issues at all.`,
	Args: cobra.ExactArgs(1),
	RunE: runGateFix,
}

var gateRejectCmd = &cobra.Command{
	Use:   "reject <snapshot-id>",
	Short: "Mark a held integration as dismissed (snapshot is kept for audit)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGateReject,
}

// Note: the --json flag for `gate list` is registered in main.go's
// init() alongside the other --json flags (gateListCmd.Flags().BoolVar
// on gateListJSON). Registering it here too panics at startup with
// "list flag redefined: json".

func runGateList(cmd *cobra.Command, args []string) error {
	// Multi-root aware: list held snapshots from EVERY project in
	// the workspace, not just the cwd's DB. Pre-v0.31.9, this only
	// queried the cwd's project — so a cross-project agent run that
	// held a snapshot in kai-server didn't show up when you ran
	// `kai gate list` from the kai workspace. Now we discover the
	// workspace (same as the TUI does), open every initialized
	// project's DB, and aggregate.
	//
	// Each row carries a project tag so the user can tell which
	// project the held snapshot lives in. Single-root workspaces
	// render exactly as before (the project tag is suppressed when
	// there's only one).
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	set, outcome := projects.Discover(cwd)
	if outcome != projects.OutcomeRootsFound {
		// Fall back to the legacy single-DB path. Common when run from
		// an uninitialized dir or a single-project layout with no
		// kai.projects.yaml — the existing UX should be unchanged.
		return runGateListSingle()
	}
	if err := set.Open(); err != nil {
		return fmt.Errorf("opening project DBs: %w", err)
	}
	defer set.Close()

	type heldRow struct {
		project string
		node    *graph.Node
	}
	var rows []heldRow
	for _, p := range set.Projects() {
		if p.DB == nil {
			continue // uninitialized project
		}
		held, lerr := safetygate.ListHeld(p.DB)
		if lerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: listing held snapshots: %v\n", p.Name, lerr)
			continue
		}
		for _, n := range held {
			rows = append(rows, heldRow{project: p.Name, node: n})
		}
	}

	if gateListJSON {
		entries := make([]gateListEntry, 0, len(rows))
		for _, r := range rows {
			v, _ := r.node.Payload["gateVerdict"].(string)
			blast, _ := r.node.Payload["gateBlastRadius"].(float64)
			from, _ := r.node.Payload["integratedFrom"].(string)
			createdMs, _ := r.node.Payload["createdAt"].(float64)
			ts := ""
			if createdMs > 0 {
				ts = time.UnixMilli(int64(createdMs)).Format("2006-01-02 15:04:05")
			}
			hexStr := util.BytesToHex(r.node.ID)
			entries = append(entries, gateListEntry{
				ID:          hexStr,
				Project:     r.project,
				Verdict:     strings.ToUpper(v),
				BlastRadius: int(blast),
				From:        from,
				Timestamp:   ts,
			})
		}
		b, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("No integrations are held by the safety gate.")
		return nil
	}
	if len(set.Projects()) > 1 {
		fmt.Printf("%d integration(s) held across %d project(s):\n\n", len(rows), len(set.Projects()))
	} else {
		fmt.Printf("%d integration(s) held:\n\n", len(rows))
	}
	showProj := len(set.Projects()) > 1
	for _, r := range rows {
		n := r.node
		v, _ := n.Payload["gateVerdict"].(string)
		blast, _ := n.Payload["gateBlastRadius"].(float64)
		from, _ := n.Payload["integratedFrom"].(string)
		createdMs, _ := n.Payload["createdAt"].(float64)
		fromShort := from
		if len(fromShort) > 12 {
			fromShort = fromShort[:12]
		}
		ts := ""
		if createdMs > 0 {
			ts = time.UnixMilli(int64(createdMs)).Format("2006-01-02 15:04:05")
		}
		if showProj {
			fmt.Printf("  [%s]  %s  %-6s  blast=%-4d  ws=%s  %s\n",
				r.project, util.BytesToHex(n.ID)[:12], strings.ToUpper(v), int(blast), fromShort, ts)
		} else {
			fmt.Printf("  %s  %-6s  blast=%-4d  ws=%s  %s\n",
				util.BytesToHex(n.ID)[:12], strings.ToUpper(v), int(blast), fromShort, ts)
		}
	}
	fmt.Println("\nRun `kai gate show <id>` to inspect, `kai gate approve <id>` to publish.")
	return nil
}

// runGateListSingle preserves the legacy single-DB path for when
// projects.Discover returns no multi-root set (uninitialized cwd,
// single-project layout, etc). Splitting it out keeps the multi-
// root branch readable.
func runGateListSingle() error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	held, err := safetygate.ListHeld(db)
	if err != nil {
		return fmt.Errorf("listing held snapshots: %w", err)
	}

	if gateListJSON {
		entries := make([]gateListEntry, 0, len(held))
		for _, n := range held {
			v, _ := n.Payload["gateVerdict"].(string)
			blast, _ := n.Payload["gateBlastRadius"].(float64)
			from, _ := n.Payload["integratedFrom"].(string)
			createdMs, _ := n.Payload["createdAt"].(float64)
			ts := ""
			if createdMs > 0 {
				ts = time.UnixMilli(int64(createdMs)).Format("2006-01-02 15:04:05")
			}
			hexStr := util.BytesToHex(n.ID)
			entries = append(entries, gateListEntry{
				ID:          hexStr,
				Project:     "",
				Verdict:     strings.ToUpper(v),
				BlastRadius: int(blast),
				From:        from,
				Timestamp:   ts,
			})
		}
		b, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(b))
		return nil
	}

	if len(held) == 0 {
		fmt.Println("No integrations are held by the safety gate.")
		return nil
	}
	fmt.Printf("%d integration(s) held:\n\n", len(held))
	for _, n := range held {
		v, _ := n.Payload["gateVerdict"].(string)
		blast, _ := n.Payload["gateBlastRadius"].(float64)
		from, _ := n.Payload["integratedFrom"].(string)
		createdMs, _ := n.Payload["createdAt"].(float64)
		fromShort := from
		if len(fromShort) > 12 {
			fromShort = fromShort[:12]
		}
		ts := ""
		if createdMs > 0 {
			ts = time.UnixMilli(int64(createdMs)).Format("2006-01-02 15:04:05")
		}
		fmt.Printf("  %s  %-6s  blast=%-4d  ws=%s  %s\n",
			util.BytesToHex(n.ID)[:12], strings.ToUpper(v), int(blast), fromShort, ts)
	}
	fmt.Println("\nRun `kai gate show <id>` to inspect, `kai gate approve <id>` to publish.")
	return nil
}

// resolveSnapshotByPrefix accepts a hex prefix and returns the unique
// matching snapshot node, or an error if zero or multiple match. This
// lets the user paste the truncated id printed by `kai gate list`.
func resolveSnapshotByPrefix(db *graph.DB, prefix string) (*graph.Node, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if _, err := hex.DecodeString(strings.TrimRight(prefix, " ")); err != nil {
		// Allow odd-length prefixes (e.g. 12 chars from the listing).
		if _, err := hex.DecodeString(prefix + "0"); err != nil {
			return nil, fmt.Errorf("invalid hex id %q: %w", prefix, err)
		}
	}
	all, err := db.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		return nil, err
	}
	var matches []*graph.Node
	for _, n := range all {
		if strings.HasPrefix(util.BytesToHex(n.ID), prefix) {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no snapshot matches prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous prefix %q matches %d snapshots", prefix, len(matches))
	}
}

func runGateShow(cmd *cobra.Command, args []string) error {
	db, snap, err := resolveSnapshotByPrefixAcrossProjects(args[0])
	if err != nil {
		return err
	}
	defer db.Close()
	v, _ := snap.Payload["gateVerdict"].(string)
	if v == "" {
		return fmt.Errorf("snapshot %s has no gate verdict (was it produced by `kai integrate`?)",
			util.BytesToHex(snap.ID)[:12])
	}
	blast, _ := snap.Payload["gateBlastRadius"].(float64)
	from, _ := snap.Payload["integratedFrom"].(string)
	target, _ := snap.Payload["targetSnapshot"].(string)
	createdMs, _ := snap.Payload["createdAt"].(float64)

	fmt.Printf("Snapshot %s\n", util.BytesToHex(snap.ID))
	fmt.Printf("  Verdict:      %s\n", strings.ToUpper(v))
	fmt.Printf("  Blast radius: %d (depth-1 callers + importers)\n", int(blast))
	if from != "" {
		fmt.Printf("  From workspace: %s\n", from)
	}
	if target != "" {
		fmt.Printf("  Original target: %s\n", target)
	}
	if createdMs > 0 {
		fmt.Printf("  Created:      %s\n", time.UnixMilli(int64(createdMs)).Format("2006-01-02 15:04:05"))
	}
	if dismissed, _ := snap.Payload["dismissed"].(bool); dismissed {
		fmt.Println("  Status:       DISMISSED")
	}

	// Provenance diagnostics. Held snapshots produced by `kai integrate`
	// always carry sourceType=merged|merged-ff plus integratedFrom +
	// targetSnapshot. Anything else (or missing fields) means this
	// snapshot came from a non-integrate path (capture, push, manual
	// insertion, pre-`integratedFrom`-field era), and the fix /
	// approve flows will refuse to operate on it.
	if st, _ := snap.Payload["sourceType"].(string); st != "" {
		fmt.Printf("  Source type:  %s\n", st)
	} else {
		fmt.Println("  Source type:  (missing)")
	}
	if sr, _ := snap.Payload["sourceRef"].(string); sr != "" {
		fmt.Printf("  Source ref:   %s\n", sr)
	}
	// fix/approve (ApproveHeld) accepts EITHER integratedFrom OR
	// orchestratorAgent — orchestrator-produced held snaps carry the
	// latter, not the former. Mirror that OR-logic here; warning on a
	// missing integratedFrom alone was a false alarm that made
	// orchestrator snaps look un-approvable when they weren't
	// (2026-05-29: baaf2a72 had orchestratorAgent and was approvable).
	fromStr, _ := snap.Payload["integratedFrom"].(string)
	orchStr, _ := snap.Payload["orchestratorAgent"].(string)
	if fromStr == "" && orchStr == "" {
		fmt.Println("  ⚠ no integratedFrom or orchestratorAgent — fix/approve will refuse this snapshot")
	}
	if _, ok := snap.Payload["targetSnapshot"].(string); !ok {
		fmt.Println("  ⚠ no targetSnapshot — fix/approve will refuse this snapshot")
	}

	if reasons := stringList(snap.Payload["gateReasons"]); len(reasons) > 0 {
		fmt.Println("\nReasons:")
		for _, r := range reasons {
			fmt.Printf("  · %s\n", r)
		}
	}
	if touches := stringList(snap.Payload["gateTouches"]); len(touches) > 0 {
		fmt.Printf("\nAffected files (%d):\n", len(touches))
		for _, t := range touches {
			fmt.Printf("  %s\n", t)
		}
	}
	return nil
}

// runGateApprove delegates to workspace.Manager.ApproveHeld and prints
// the result. The "is the target ref still here?" check and the actual
// ref advance live in the workspace package so the TUI can reuse them.
func runGateApprove(cmd *cobra.Command, args []string) error {
	db, snap, err := resolveSnapshotByPrefixAcrossProjects(args[0])
	if err != nil {
		return err
	}
	defer db.Close()
	mgr := workspace.NewManager(db)
	advanced, err := mgr.ApproveHeld(snap.ID)
	if err != nil {
		return err
	}
	if len(advanced) == 0 {
		fmt.Println("No refs were advanced.")
		return nil
	}
	fmt.Printf("Approved snapshot %s. Advanced:\n", util.BytesToHex(snap.ID)[:12])
	for _, n := range advanced {
		fmt.Printf("  %s -> %s\n", n, util.BytesToHex(snap.ID)[:12])
	}
	return nil
}

// runGateReject delegates to workspace.Manager.RejectHeld.
func runGateReject(cmd *cobra.Command, args []string) error {
	db, snap, err := resolveSnapshotByPrefixAcrossProjects(args[0])
	if err != nil {
		return err
	}
	defer db.Close()
	mgr := workspace.NewManager(db)
	if err := mgr.RejectHeld(snap.ID); err != nil {
		return err
	}
	fmt.Printf("Dismissed snapshot %s.\n", util.BytesToHex(snap.ID)[:12])
	return nil
}

func runGateDiff(cmd *cobra.Command, args []string) error {
	db, snap, err := resolveSnapshotByPrefixAcrossProjects(args[0])
	if err != nil {
		return err
	}
	defer db.Close()
	patch, err := gatereview.HeldSnapshotDiff(db, snap)
	if err != nil {
		return err
	}
	if patch == "" {
		fmt.Println("No differences between held snapshot and its target.")
		return nil
	}
	fmt.Print(patch)
	return nil
}

// runGateReview runs the gate's AI review flow. With no argument it
// reviews every held snapshot; with an id it reviews just that one.
// Output for each item is the SUMMARY / AUDIT / RECOMMENDATION block
// the spec calls for. The LLM call is a single-shot completion through
// the existing ai.Client (ANTHROPIC_API_KEY required).
func runGateReview(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	cfg, err := config.Load(kaiDir)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	prov, reviewModel, _, err := buildGateProvider(cfg)
	if err != nil {
		return err
	}

	var snaps []*graph.Node
	if len(args) == 1 {
		// Multi-root: the held snapshot may live in a sibling
		// project's DB. Re-resolve across all projects and swap
		// the local db to the one that owns the snapshot, so
		// reviewOneHeld below queries the right DB.
		resolvedDB, snap, err := resolveSnapshotByPrefixAcrossProjects(args[0])
		if err != nil {
			return err
		}
		if !safetygate.IsHeld(snap) {
			resolvedDB.Close()
			return fmt.Errorf("snapshot %s is not held by the gate", util.BytesToHex(snap.ID)[:12])
		}
		db.Close()
		db = resolvedDB
		snaps = []*graph.Node{snap}
	} else {
		held, err := safetygate.ListHeld(db)
		if err != nil {
			return fmt.Errorf("listing held snapshots: %w", err)
		}
		if len(held) == 0 {
			fmt.Println("No integrations are held by the safety gate.")
			return nil
		}
		snaps = held
	}

	for i, snap := range snaps {
		if i > 0 {
			fmt.Println()
		}
		if err := reviewOneHeld(ctx, prov, reviewModel, db, snap); err != nil {
			fmt.Fprintf(os.Stderr, "review %s failed: %v\n", util.BytesToHex(snap.ID)[:12], err)
		}
	}
	return nil
}

// reviewOneHeld runs the LLM review for a single held snapshot and
// prints the formatted block. Engine work lives in the gatereview
// package so the CLI and TUI render the same Result.
func reviewOneHeld(ctx context.Context, prov provider.Provider, model string, db *graph.DB, snap *graph.Node) error {
	short := util.BytesToHex(snap.ID)
	if len(short) > 12 {
		short = short[:12]
	}
	res, err := gatereview.Review(ctx, prov, model, db, snap, gatereview.Inputs{})
	if err != nil {
		return err
	}
	fmt.Printf("━━━ Gate Review: %s ━━━\n", short)
	fmt.Printf("Verdict: %s   Blast radius: %d\n", strings.ToUpper(res.Verdict), res.BlastRadius)
	if len(res.Reasons) > 0 {
		fmt.Printf("Held because: %s\n", strings.Join(res.Reasons, "; "))
	}
	if len(res.Touches) > 0 {
		fmt.Printf("Affected files (%d): %s\n", len(res.Touches), strings.Join(res.Touches, ", "))
	}
	fmt.Println()
	if res.Summary != "" {
		fmt.Println("SUMMARY")
		fmt.Println(res.Summary)
		fmt.Println()
	}
	fmt.Println("AUDIT")
	if res.AuditClean && len(res.Issues) == 0 {
		fmt.Println("CLEAN")
	} else {
		fmt.Println("ISSUES")
		for i, iss := range res.Issues {
			tag := "human"
			if iss.Fixable {
				tag = "fixable"
			}
			fmt.Printf("  %d. [%s] %s\n", i+1, tag, iss.Description)
		}
	}
	fmt.Println()
	fmt.Println("RECOMMENDATION")
	if res.Recommendation != "" {
		fmt.Println(string(res.Recommendation))
		if res.RecReason != "" {
			fmt.Println(res.RecReason)
		}
	} else {
		fmt.Println(strings.TrimSpace(res.Raw))
	}
	return nil
}

// runGateFix is the headless counterpart to the TUI's [f] keybind.
// Sets up the same provider+config the TUI uses, re-runs Review to get
// fresh issues (the audit isn't persisted on the snapshot), then calls
// gatereview.Fix. Refuses cleanly when there's nothing fixable rather
// than burning a planner turn proving it.
func runGateFix(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, _ := os.Getwd()

	if err := projects.CheckContainerInvariant(cwd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fmt.Errorf("refusing to launch: container/project invariant violated")
	}
	set, outcome := projects.Discover(cwd)
	if outcome != projects.OutcomeRootsFound {
		return fmt.Errorf("no kai project at %s — run `kai init`", cwd)
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

	snap, err := resolveSnapshotByPrefix(asGraphDB(db), args[0])
	if err != nil {
		return err
	}
	if !safetygate.IsHeld(snap) {
		return fmt.Errorf("snapshot %s is not held by the gate", util.BytesToHex(snap.ID)[:12])
	}

	prov, reviewModel, fixModel, err := buildGateProvider(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "kai gate fix: auditing held snapshot…\n")
	res, err := gatereview.Review(ctx, prov, reviewModel, asGraphDB(db), snap, gatereview.Inputs{})
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}

	// Both [fixable] and [human] issues go to the fix agent: its
	// output is re-held by the gate for human approve/reject, so a
	// judgment-issue fix is a proposal for review, not an autonomous
	// publish. Only a completely empty audit means there's nothing
	// to do.
	if len(res.Issues) == 0 {
		return fmt.Errorf("no issues in audit — use `kai gate approve` if the change looks fine, or `kai gate reject`")
	}
	fixable, human := 0, 0
	for _, iss := range res.Issues {
		if iss.Fixable {
			fixable++
		} else {
			human++
		}
	}

	turns := gateFixMaxTurns
	if turns <= 0 {
		turns = 3
	}

	opts := agent.Options{
		Projects:          set,
		Workspace:         primary.Path,
		Provider:     prov,
		Model:        fixModel,
		Graph:             asGraphDB(db),
		EnableBash:   true,
		BashAllow:    cfg.Agent.BashAllow,
		SessionStore: asGraphDB(db),
		GateConfig:   gateCfg,
		RunLogDir:    kaiDir,
		Hooks: agent.Hooks{
			OnAssistantText: func(t string) {
				if s := strings.TrimSpace(t); s != "" {
					fmt.Fprintln(os.Stderr, "  agent: "+s)
				}
			},
		},
	}

	fmt.Fprintf(os.Stderr, "kai gate fix: dispatching agent (%d mechanical + %d judgment issue(s), max %d turn(s))…\n",
		fixable, human, turns)
	out := gatereview.Fix(ctx, asGraphDB(db), gatereview.FixInputs{
		Snap:       snap,
		Issues:     res.Issues,
		AgentOpts:  opts,
		GateConfig: gateCfg,
		MaxTurns:   turns,
	})
	if out.Error != nil {
		if out.AssistantText != "" {
			fmt.Fprintln(os.Stderr, "agent last said: "+out.AssistantText)
		}
		return out.Error
	}

	short := ""
	if len(out.NewSnapshotID) > 0 {
		short = util.BytesToHex(out.NewSnapshotID)
		if len(short) > 12 {
			short = short[:12]
		}
	}
	fmt.Printf("New snapshot:  %s\n", short)
	fmt.Printf("New verdict:   %s   blast=%d\n", strings.ToUpper(string(out.NewVerdict)), out.NewBlast)
	if len(out.NewReasons) > 0 {
		fmt.Printf("Reasons:       %s\n", strings.Join(out.NewReasons, "; "))
	}
	if out.AutoPromoted {
		fmt.Printf("Auto-promoted: %s\n", strings.Join(out.AdvancedRefs, ", "))
		return nil
	}
	if !gateFixAutoApprove {
		fmt.Println("Fix produced a new held snapshot. Inspect with `kai gate show " + short + "` " +
			"and approve/reject as usual.")
		return nil
	}
	if out.NewVerdict != safetygate.Auto {
		return fmt.Errorf("--auto-approve requested but new verdict is %s; not advancing refs",
			strings.ToUpper(string(out.NewVerdict)))
	}
	// Auto-promote already happens inside Fix when NewVerdict == Auto;
	// reaching this branch with --auto-approve and Auto means publish
	// failed. Surface that.
	return fmt.Errorf("verdict was Auto but auto-promotion did not advance any refs (publish may have failed)")
}

// buildGateProvider constructs the LLM provider used by gate review /
// gate fix, plus the two role models that path needs: the review
// model (a strong model audits the held change) and the fix model
// (the cheaper agent applies the fixes) — cheap model writes, strong
// model reviews. Same plumbing the planner uses (provider.FromEnv +
// provider.New): kailab credentials when present, ANTHROPIC_API_KEY
// fallback otherwise. For BYOM providers both roles collapse to the
// single provider-resolved model (kailab model ids aren't valid
// there), mirroring buildPlannerServices in tui.go.
func buildGateProvider(cfg config.Config) (prov provider.Provider, reviewModel, fixModel string, err error) {
	creds, _ := remote.LoadCredentials()
	var kailabBase, kailabToken string
	if creds != nil {
		kailabBase = creds.ServerURL
		if t, terr := remote.GetValidAccessToken(); terr == nil {
			kailabToken = t
		}
	}
	pcfg := provider.FromEnv(kailabBase, kailabToken, cfg.Planner.Model)
	prov, err = provider.New(pcfg)
	if err != nil {
		return nil, "", "", fmt.Errorf("provider: %w (set ANTHROPIC_API_KEY or run `kai login` for kailab)", err)
	}
	reviewModel, fixModel = pcfg.Model, pcfg.Model
	if pcfg.Kind == provider.KindKailab {
		reviewModel = modelFromEnv("KAI_REVIEW_MODEL", cfg.Review.Model)
		fixModel = modelFromEnv("KAI_AGENT_MODEL", cfg.Agent.Model)
	}
	return prov, reviewModel, fixModel, nil
}

// stringList coerces a payload value to []string. JSON-decoded payloads
// produce []interface{} rather than []string, so we normalize here.
func stringList(v interface{}) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []interface{}:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// resolveSnapshotByPrefixAcrossProjects walks every initialized
// project in the workspace and tries to resolve `prefix` in each
// DB. Returns the matching snapshot AND the DB it lives in so
// downstream operations (approve, reject, show, review, diff) can
// run against the right project. Falls back to single-DB resolution
// when the workspace isn't multi-root.
//
// The 2026-05-27 dogfood pinned the gap: `kai gate list` is
// multi-root aware and surfaced a held entry from the kai monorepo.
// `kai gate approve <id>` from the kai-desktop cwd then failed
// with "no snapshot matches prefix" because it was looking only
// in kai-desktop's DB. List shows what approve can't touch — a
// dead-end UX. After this fix, any project in the workspace can
// resolve any held snapshot's prefix.
//
// Returns the DB *not closed* — caller owns lifecycle via defer.
// On no-match across all projects: returns the original single-DB
// error wording so scripts that grep "no snapshot matches prefix"
// still work.
func resolveSnapshotByPrefixAcrossProjects(prefix string) (*graph.DB, *graph.Node, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fmt.Errorf("cwd: %w", err)
	}
	set, outcome := projects.Discover(cwd)
	if outcome != projects.OutcomeRootsFound {
		// Single-root fallback. openDB() opens the cwd's project
		// DB the way it always has; resolveSnapshotByPrefix runs
		// against it. Caller-as-before.
		db, err := openDB()
		if err != nil {
			return nil, nil, err
		}
		snap, err := resolveSnapshotByPrefix(db, prefix)
		if err != nil {
			db.Close()
			return nil, nil, err
		}
		return db, snap, nil
	}
	if err := set.Open(); err != nil {
		return nil, nil, fmt.Errorf("opening project DBs: %w", err)
	}
	for _, p := range set.Projects() {
		if p == nil || p.DB == nil {
			continue
		}
		snap, err := resolveSnapshotByPrefix(p.DB, prefix)
		if err != nil {
			continue // try next project
		}
		// Close sibling project DBs — caller owns closing p.DB
		for _, other := range set.Projects() {
			if other != p && other.DB != nil {
				other.DB.Close()
				other.DB = nil
			}
		}
		return p.DB, snap, nil
	}
	set.Close()
	return nil, nil, fmt.Errorf("no snapshot matches prefix %q", strings.ToLower(strings.TrimSpace(prefix)))
}
