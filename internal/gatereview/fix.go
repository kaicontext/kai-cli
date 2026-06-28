package gatereview

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/dirio"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/safetygate"
	"github.com/kaicontext/kai-engine/snapshot"
	"github.com/kaicontext/kai-engine/util"
	"github.com/kaicontext/kai-engine/workspace"
)

// FixInputs carries everything the fix flow needs from the REPL.
// Provider / Graph / KaiBinary / Hooks come straight off
// PlannerServices.OrchestratorCfg; the snapshot + issues come from
// the prior Review.
//
// AgentOpts is treated as a template — Fix overrides Prompt, MaxTurns,
// TaskName, and Mode so the caller can't accidentally hand the fix
// agent a 50-turn budget meant for a planner run.
type FixInputs struct {
	Snap       *graph.Node
	Issues     []Issue
	AgentOpts  agent.Options // template; Provider/Workspace/Graph/etc. set by caller
	GateConfig safetygate.Config
	MaxTurns   int // capped at 5; defaults to 3
}

// FixResult is what the REPL renders after a fix run. NewSnapshotID
// is empty when the fix attempt failed before producing a new
// snapshot; in that case Error explains why and the original held
// snapshot is unchanged.
type FixResult struct {
	NewSnapshotID []byte
	NewVerdict    safetygate.Verdict
	NewBlast      int
	NewReasons    []string
	AutoPromoted  bool   // verdict == auto AND the integrate path advanced refs
	AdvancedRefs  []string
	AssistantText string // last user-visible model narration; surfaced on error
	Error         error
}

// Fix runs a scoped agent on the held snapshot's working tree, then
// re-stages and re-integrates the workspace so the result goes
// through the gate just like a fresh integrate. Returns the new
// verdict; the caller decides what to display.
//
// The agent is intentionally constrained:
//   - MaxTurns is capped at 5 (default 3) — the spec frames this as a
//     small remediation, not a redesign.
//   - Mode is forced to coding (full tools).
//   - The prompt enumerates EVERY audit issue — both [fixable]
//     (mechanical) and [human] (judgment) — because the fix agent's
//     output is itself re-held by the gate for human approve/reject,
//     so attempting a judgment issue proposes a fix for review rather
//     than publishing one autonomously. The prompt tells the agent to
//     decline (no edit) any judgment issue with no concrete code fix.
//   - The file allowlist is conveyed in the prompt rather than via a
//     hard sandbox: the agent.Run path doesn't currently enforce a
//     per-tool path whitelist, and adding one is out of scope for the
//     Phase 2 wow demo. The agent IS confined to opts.Workspace, so
//     it can't escape the repo.
func Fix(ctx context.Context, db *graph.DB, in FixInputs) *FixResult {
	res := &FixResult{}
	if db == nil {
		res.Error = fmt.Errorf("Fix: db is nil")
		return res
	}
	if in.Snap == nil {
		res.Error = fmt.Errorf("Fix: held snapshot is nil")
		return res
	}

	// Two held-snapshot lineages exist:
	//   - workspace path (`kai integrate`): payload has `integratedFrom`
	//     pointing at a workspace node. We re-stage that workspace and
	//     re-integrate.
	//   - orchestrator path (`kai code` agent runs): payload has
	//     `orchestratorAgent` set but `integratedFrom` empty — the
	//     orchestrator integrates straight into mainRepo via `kai
	//     capture` and there's no workspace to re-stage. We must
	//     mirror the orchestrator's recapture+classify dance instead.
	//     See orchestrator.integrateOneAgent for the original sequence;
	//     we replay steps 3–5 of it here after the fix agent edits the
	//     working tree.
	wsHex, _ := in.Snap.Payload["integratedFrom"].(string)
	orchAgent, _ := in.Snap.Payload["orchestratorAgent"].(string)
	targetHex, _ := in.Snap.Payload["targetSnapshot"].(string)
	if targetHex == "" {
		res.Error = fmt.Errorf("Fix: snapshot has no targetSnapshot")
		return res
	}
	if wsHex == "" && orchAgent == "" {
		res.Error = fmt.Errorf("Fix: snapshot has neither integratedFrom workspace nor orchestratorAgent — cannot determine fix path")
		return res
	}
	targetID, err := util.HexToBytes(targetHex)
	if err != nil {
		res.Error = fmt.Errorf("Fix: decoding targetSnapshot: %w", err)
		return res
	}

	if len(in.Issues) == 0 {
		res.Error = fmt.Errorf("no issues in the audit — nothing for the fix agent to do")
		return res
	}

	turns := in.MaxTurns
	if turns <= 0 {
		turns = 3
	}
	if turns > 5 {
		turns = 5
	}

	prompt := buildFixPrompt(in.Snap, in.Issues)

	opts := in.AgentOpts
	opts.Prompt = prompt
	opts.MaxTurns = turns
	opts.TaskName = "gate_fix"
	opts.Mode = agent.ModeCoding
	// Capture the model's last narration so the REPL can surface it
	// on failure (otherwise "fix failed" is opaque). The original
	// hooks pass through unchanged for live UI updates.
	prevText := opts.Hooks.OnAssistantText
	opts.Hooks.OnAssistantText = func(t string) {
		res.AssistantText = t
		if prevText != nil {
			prevText(t)
		}
	}

	if _, err := agent.Run(ctx, opts); err != nil {
		res.Error = fmt.Errorf("fix agent: %w", err)
		return res
	}

	mgr := workspace.NewManager(db)

	if wsHex != "" {
		// Workspace path: re-stage the working tree and re-integrate.
		if opts.Workspace == "" {
			res.Error = fmt.Errorf("fix re-stage: AgentOpts.Workspace is empty")
			return res
		}
		src, err := dirio.OpenDirectory(opts.Workspace)
		if err != nil {
			res.Error = fmt.Errorf("fix re-stage: open dir: %w", err)
			return res
		}
		if _, err := mgr.Stage(wsHex, src, nil, "gate-review fix-then-approve", nil); err != nil {
			res.Error = fmt.Errorf("fix re-stage: %w", err)
			return res
		}

		gateCfg := in.GateConfig
		intRes, err := mgr.IntegrateWithOptions(wsHex, targetID, workspace.IntegrateOptions{
			GateConfig: &gateCfg,
		})
		if err != nil {
			res.Error = fmt.Errorf("fix re-integrate: %w", err)
			return res
		}
		res.NewSnapshotID = intRes.ResultSnapshot
		if intRes.Decision != nil {
			res.NewVerdict = safetygate.Verdict(intRes.Decision.Verdict)
			res.NewBlast = intRes.Decision.BlastRadius
			res.NewReasons = intRes.Decision.Reasons
		}

		// If the new verdict is Auto, publish so the result auto-promotes.
		// This is the "wow" path: held → fix → auto-clear without further
		// human input. PublishToRef is what the integrate CLI calls
		// post-classification.
		if res.NewVerdict == safetygate.Auto {
			ws, err := mgr.Get(wsHex)
			if err == nil && ws != nil {
				report, err := mgr.PublishToRef(ws, intRes, "snap.latest", workspace.PublishOptions{})
				if err == nil && !report.HeldByGate {
					res.AutoPromoted = true
					res.AdvancedRefs = report.AdvancedRefs
				}
			}
		}
	} else {
		// Orchestrator path: no workspace to re-stage. The fix agent
		// already wrote its edits straight into opts.Workspace
		// (== mainRepo). Mirror orchestrator.integrateOneAgent's tail:
		// run `kai capture` in mainRepo, classify the changed paths,
		// decorate the resulting snapshot. Roll snap.latest back if
		// the verdict isn't Auto.
		if err := fixOrchestratorRecapture(ctx, db, in, opts.Workspace, targetID, orchAgent, res); err != nil {
			res.Error = err
			return res
		}
	}

	// Always reject the original held snapshot — it's been superseded
	// either by an auto-promoted fix or by a new held snapshot the
	// user will review next. Idempotent if already dismissed.
	_ = mgr.RejectHeld(in.Snap.ID)

	return res
}

// fixOrchestratorRecapture replays the orchestrator's post-absorb
// sequence: `kai capture` in mainRepo to record the post-fix state,
// diff against the held snapshot's `targetSnapshot` to recover changed
// paths, classify, decorate the new snapshot with the gate verdict,
// and roll snap.latest back if the verdict isn't Auto.
//
// This intentionally duplicates the structure of
// orchestrator.integrateOneAgent rather than importing it because that
// package has agent-spawning concerns we don't want pulled into
// gatereview's dependency surface.
func fixOrchestratorRecapture(
	ctx context.Context,
	db *graph.DB,
	in FixInputs,
	mainRepo string,
	prevTarget []byte,
	agentName string,
	res *FixResult,
) error {
	if mainRepo == "" {
		return fmt.Errorf("fix orchestrator-recapture: AgentOpts.Workspace is empty")
	}
	kaiBin := resolveKaiBinary(in.AgentOpts.KaiBinary)

	// Snapshot snap.latest before capture so we can roll it back if
	// the gate doesn't approve. For orchestrator-held flows the
	// pre-fix snap.latest IS the prev-target (it was rolled back to
	// that when the original integrate held).
	prevLatest, _ := readLatestSnap(db)

	captureCmd := exec.CommandContext(ctx, kaiBin,
		"capture", "-m", "gate-review fix-then-approve")
	captureCmd.Dir = mainRepo
	if out, err := captureCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fix recapture (kai capture in %s): %w: %s",
			mainRepo, err, strings.TrimSpace(string(out)))
	}
	newLatest, err := readLatestSnap(db)
	if err != nil {
		return fmt.Errorf("fix recapture: resolving snap.latest: %w", err)
	}
	if len(prevLatest) > 0 && string(newLatest) == string(prevLatest) {
		return fmt.Errorf("fix agent produced no observable changes (snap.latest unchanged)")
	}

	changed, err := changedPathsBetween(db, prevTarget, newLatest)
	if err != nil {
		return fmt.Errorf("fix recapture: diff paths: %w", err)
	}

	gateCfg := in.GateConfig
	verdict, err := safetygate.Classify(ctx, changed, db, gateCfg)
	if err != nil {
		return fmt.Errorf("fix recapture: classify: %w", err)
	}

	// Decorate the new snapshot with gate metadata so `kai gate list`
	// surfaces it (or skips it if Auto). Same payload keys the
	// orchestrator writes — see orchestrator.go:594-615.
	if newSnap, err := db.GetNode(newLatest); err == nil && newSnap != nil && newSnap.Payload != nil {
		newSnap.Payload["targetSnapshot"] = hex.EncodeToString(prevTarget)
		newSnap.Payload["gateVerdict"] = string(verdict.Verdict)
		newSnap.Payload["gateBlastRadius"] = verdict.BlastRadius
		if len(verdict.Reasons) > 0 {
			newSnap.Payload["gateReasons"] = verdict.Reasons
		}
		// Match orchestrator.integrateOneAgent: always populate
		// gateTouches. Falls back to the recomputed changed-paths
		// set so consumers never have to diff snapshots themselves.
		touches := verdict.Touches
		if len(touches) == 0 {
			touches = changed
		}
		newSnap.Payload["gateTouches"] = touches
		if agentName != "" {
			newSnap.Payload["orchestratorAgent"] = agentName
		}
		// Clear any stale rename-residuals payload from earlier snapshots
		// — the deterministic rename gate was removed; the audit model
		// now covers incomplete-rename detection semantically.
		delete(newSnap.Payload, "gateRenameResiduals")
		_ = db.UpdateNodePayload(newLatest, newSnap.Payload)
	}

	res.NewSnapshotID = newLatest
	res.NewVerdict = verdict.Verdict
	res.NewBlast = verdict.BlastRadius
	res.NewReasons = verdict.Reasons

	if verdict.Verdict == safetygate.Auto {
		// `kai capture` already advanced snap.latest, which is
		// exactly the publish behavior we want.
		res.AutoPromoted = true
		res.AdvancedRefs = []string{"snap.latest"}
		return nil
	}

	// Non-Auto: roll snap.latest back so the held snap stays out of
	// team-visible refs. The new snap remains in the DB tagged for
	// review.
	if len(prevLatest) > 0 {
		_ = ref.NewRefManager(db).Set("snap.latest", prevLatest, ref.KindSnapshot)
	}
	return nil
}

// resolveKaiBinary returns an absolute path to the kai executable.
// Prefers the explicit override (set by callers that know which build
// to use), falls back to os.Executable() (the running binary), then to
// "kai" on PATH as a last resort.
func resolveKaiBinary(override string) string {
	if override != "" {
		return override
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "kai"
}

// readLatestSnap reads snap.latest from the refs table directly. We
// don't import internal/ref's Get path because we only need this one
// query and the import edge isn't worth the test-graph noise.
func readLatestSnap(db *graph.DB) ([]byte, error) {
	row := db.QueryRow(`SELECT target_id FROM refs WHERE name = 'snap.latest'`)
	var id []byte
	if err := row.Scan(&id); err != nil {
		return nil, fmt.Errorf("snap.latest: %w", err)
	}
	if len(id) == 0 {
		return nil, fmt.Errorf("snap.latest is empty")
	}
	return id, nil
}

// changedPathsBetween returns the set of paths that differ between two
// snapshots — files added, modified, or deleted from base→head. Used
// by the orchestrator-fix path to feed safetygate.Classify, which
// expects the changed-paths list to compute blast radius.
func changedPathsBetween(db *graph.DB, baseID, headID []byte) ([]string, error) {
	creator := snapshot.NewCreator(db, nil)
	collect := func(id []byte) (map[string]string, error) {
		files, err := creator.GetSnapshotFiles(id)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(files))
		for _, f := range files {
			path, _ := f.Payload["path"].(string)
			digest, _ := f.Payload["digest"].(string)
			out[path] = digest
		}
		return out, nil
	}
	base, err := collect(baseID)
	if err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	head, err := collect(headID)
	if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}
	seen := map[string]struct{}{}
	for p, hd := range head {
		if bd, ok := base[p]; !ok || bd != hd {
			seen[p] = struct{}{}
		}
	}
	for p := range base {
		if _, ok := head[p]; !ok {
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out, nil
}

func buildFixPrompt(snap *graph.Node, issues []Issue) string {
	// gateTouches is guaranteed populated by the producers
	// (workspace.applyDecisionToPayload, orchestrator.integrateOneAgent,
	// gatereview.fixOrchestratorRecapture). No defensive fallback here.
	touches := stringList(snap.Payload["gateTouches"])

	// Split the audit into mechanical ([fixable]) and judgment
	// ([human]) issues. Both go to the agent, but framed differently:
	// mechanical issues get the obvious fix, judgment issues get a
	// careful best-effort attempt (or an explicit decline).
	var mechanical, judgment []Issue
	for _, iss := range issues {
		if iss.Fixable {
			mechanical = append(mechanical, iss)
		} else {
			judgment = append(judgment, iss)
		}
	}

	var sb strings.Builder
	sb.WriteString("System: You are a remediation agent. A code change was just held by Kai's safety gate. ")
	sb.WriteString("Address the audit issues below with the smallest edits that resolve each — ")
	sb.WriteString("do not refactor, restructure, or add unrelated features, then stop.\n\n")
	sb.WriteString("Your result is NOT published automatically: it returns to the gate as a held change for a human to ")
	sb.WriteString("approve or reject. So attempt every issue listed — the human reviews your work before it lands.\n\n")
	sb.WriteString("Constraints:\n")
	sb.WriteString("- Confine your edits to the files the held change already touched (listed below).\n")
	sb.WriteString("- Do not introduce new dependencies or rewrite unrelated code.\n")
	sb.WriteString("- Stop as soon as the listed issues are addressed; you have a tight tool-call budget.\n")
	sb.WriteString("- Use Read/Edit on the listed file(s) directly. Do NOT rely on kai_grep — the workspace may not be indexed for arbitrary text.\n")
	sb.WriteString("- A rename residual inside test fixture data (a string literal in a _test.go table, an expected-value constant) is usually deliberate test input, NOT a stale reference. Do not blindly rewrite it — that breaks the test. Either leave it and annotate the line with a \"kai-rename-keep\" comment, or update the fixture AND its expected result together so the test still passes.\n")
	sb.WriteString("- NEVER delete an existing \"kai-rename-keep\" comment. It is a load-bearing annotation: it is the only thing keeping the rename gate from re-blocking that line. If an issue describes a \"kai-rename-keep\" comment as leftover cruft or a stray marker, that issue is mistaken — ignore it and make NO edit. The annotation stays.\n")
	sb.WriteString("- \"kai-rename-keep\" is ONLY for a reference that is genuinely correct to retain. Do not append it to a comment whose text is now factually wrong just to silence the gate — fix the wording instead. Silencing a real staleness gets caught downstream and wastes a round.\n\n")

	if len(touches) > 0 {
		sb.WriteString("Files in the held change:\n")
		for _, t := range touches {
			fmt.Fprintf(&sb, "  - %s\n", t)
		}
		sb.WriteString("\n")
	}

	if len(mechanical) > 0 {
		sb.WriteString("Mechanical issues — apply the obvious fix:\n")
		for i, iss := range mechanical {
			fmt.Fprintf(&sb, "  %d. %s\n", i+1, iss.Description)
		}
		sb.WriteString("\n")
	}
	if len(judgment) > 0 {
		sb.WriteString("Judgment issues — these need a careful decision. Apply the most reasonable fix you can ")
		sb.WriteString("justify. If a judgment issue has no concrete code fix (it is a design question or an ")
		sb.WriteString("intentional choice), make NO edit for it and state plainly why — do not invent a change ")
		sb.WriteString("just to look productive:\n")
		for i, iss := range judgment {
			fmt.Fprintf(&sb, "  %d. %s\n", i+1, iss.Description)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Apply the fixes now, then end your turn.")
	return sb.String()
}
