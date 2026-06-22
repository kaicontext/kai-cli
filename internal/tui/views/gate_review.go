package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kai/api/agent"
	"kai/api/graph"
	"kai/api/provider"
	"kai/api/gatereview"
	"kai/api/orchestrator"
	"kai/api/safetygate"
	"kai/api/telemetry"
	"kai/api/util"
	"kai/api/workspace"
)

// gateReviewState drives the in-TUI walkthrough of held integrations.
// Held while the REPL is in "review mode": each item is presented with
// a styled header / summary / audit / recommendation block and the
// user resolves it with a single keystroke ([a]/[r]/[s]/[f]/[d]).
//
// The state machine is intentionally narrow — no nested screens, no
// scrollable diff pager. The diff key writes the patch into the REPL's
// scrollback (where the existing viewport already handles scrolling),
// the action keys mutate the gate via workspace.Manager and advance
// to the next item.
// maxAutoFixRounds caps the automatic review→fix loop. Each fix
// re-reviews its own output; without a cap a fixer that never fully
// satisfies the reviewer would loop forever. After this many rounds
// the loop stops and the change is left for the user's manual
// approve/reject.
const maxAutoFixRounds = 3

// gateVerifyTimeout caps the ground-truth build+test run the gate
// review executes before each audit. The Go verify uses `-short`, so
// a passing suite finishes fast; this budget mainly bounds a
// hang/deadlock (which a timeout correctly reports as a failure).
const gateVerifyTimeout = 4 * time.Minute

type gateReviewState struct {
	snaps   []*graph.Node // held items at /gate entry; copy so DB churn doesn't shift the cursor
	idx     int           // current index into snaps; len(snaps) means "done"
	loading bool          // LLM call in flight for snaps[idx]
	result  *gatereview.Result
	err     error

	// fixRounds counts auto review→fix iterations for the current
	// item; reset on advance. Capped at maxAutoFixRounds.
	fixRounds int

	// autoMode is true when the walkthrough was entered by
	// TriggerAutoGateReview at end-of-turn rather than by the user
	// typing /gate review. In auto mode a FIX_THEN_APPROVE rec with
	// any audit issues auto-presses [f] so the user doesn't have to.
	// Approve/reject stay manual either way — we surface the
	// recommendation, not the decision.
	autoMode bool
}

// gateReviewResultMsg lands when the LLM call for a single held item
// completes. The REPL writes the formatted block into scrollback and
// re-prompts for the user's keystroke.
type gateReviewResultMsg struct {
	idx    int // matched against state.idx so a stale call doesn't render over a newer one
	result *gatereview.Result
	err    error
}

// GateReviewActionMsg lands after an approve / reject completes. The
// REPL writes a confirmation line and advances to the next item.
type GateReviewActionMsg struct {
	idx    int
	kind   string // "approve" | "reject"
	advRefs []string
	err    error
}

// gateReviewDiffMsg lands when [d] finishes loading the patch text.
// Kept off the LLM path so showing a diff doesn't burn a token.
type gateReviewDiffMsg struct {
	idx   int
	patch string
	err   error
}

// GateReviewFixMsg lands after the fix agent finishes running and the
// re-stage / re-integrate has produced a new verdict. Result.Error is
// the first thing the dispatcher checks; on success the dispatcher
// either advances (auto-promoted) or replaces the current item with a
// fresh review for the new (still-held) snapshot.
type GateReviewFixMsg struct {
	idx    int
	result *gatereview.FixResult
}

// TriggerAutoGateReview is the end-of-turn entry point: app.go calls
// this after an agent run finishes and the held-gate count has grown.
// Returns (REPL, nil) when the REPL is busy (planning, executing,
// already in review) so the caller can fall back to the "Type /gate
// review when you're ready" nudge. Returns (REPL, cmd) otherwise,
// where cmd kicks off the same review flow the user gets from typing
// /gate review.
//
// Defined as a value-receiver wrapper because the parent model holds
// REPL by value; mutations inside enterGateReview propagate through
// the returned REPL.
func (r REPL) TriggerAutoGateReview() (REPL, tea.Cmd) {
	if r.gateReviewing || r.executing || r.planning || r.gateReview != nil {
		return r, nil
	}
	cmd := r.enterGateReview()
	if r.gateReview != nil {
		r.gateReview.autoMode = true
	}
	return r, cmd
}

// enterGateReview starts the review flow. Called from the slash
// dispatcher when the user types bare `/gate`, and from
// TriggerAutoGateReview when an agent turn ends with new holds.
// Snapshots the held list upfront so the cursor isn't disrupted by
// background refreshes mid-walkthrough — fresh holds appear next time.
//
// Returns a tea.Cmd that fires the first review (LLM call) so the user
// sees movement immediately instead of waiting for the next event tick.
func (r *REPL) enterGateReview() tea.Cmd {
	if r.services == nil || r.services.DB == nil {
		r.write(styleError.Render("/gate review is unavailable: TUI is in shell-out-only mode"))
		return nil
	}
	if r.services.OrchestratorCfg.AgentProvider == nil {
		r.write(styleError.Render(
			"/gate review needs an LLM api. Run `kai login` (kailab) or set ANTHROPIC_API_KEY and re-run."))
		return nil
	}

	held, err := safetygate.ListHeld(r.services.DB)
	if err != nil {
		r.write(styleError.Render(fmt.Sprintf("/gate: list held failed: %v", err)))
		return nil
	}
	if len(held) == 0 {
		r.write(styleDim.Render("Nothing held by the gate."))
		return nil
	}

	r.gateReview = &gateReviewState{snaps: held, idx: 0, loading: true}
	// Header during loading: just the count + a "loading…" hint.
	// Keybinds intentionally NOT shown here — they appear in the
	// per-item footer once the audit lands. Listing them up front
	// invited users to press them before the data was ready.
	r.write(styleHeader.Render(fmt.Sprintf(
		"Gate review — %d held integration(s). loading audit…",
		len(held))))
	// Spinner setup: gate-review's audit LLM call takes ~10s; without
	// this the TUI looks frozen between the header and the result.
	// Treat it as a planner-shaped activity — dedicated bool keeps
	// the planning/executing semantics clean elsewhere.
	r.gateReviewing = true
	r.spinnerText = pickSpinnerPhrase()
	r.spinner.Spinner = pickSpinnerStyle()
	r.runStart = time.Now()
	r.lastActivity = r.runStart
	r.providerState = ChatActivityEvent{}
	r.providerStateAt = time.Time{}
	// Clear any leftover in-flight streaming preview from a prior
	// chat/planner turn. Without this, the bottom (live) region
	// keeps showing "+N earlier lines (full reply lands below…)"
	// from the previous response while we're working in /gate
	// mode — visually splitting the screen between gate review
	// scrollback and a phantom chat preview that has no relevance
	// to what the user is doing now.
	r.streamBuf = ""
	r.streamActive = false
	r.streamClosed = true
	r.renderSpinner()
	return tea.Batch(
		runGateReviewItem(r.services, held[0], r.fmtRecentTurns(), 0),
		r.spinner.Tick,
	)
}

// runGateReviewItem fires the LLM review for one held snapshot and
// wraps the result as a tea.Msg. Telemetry is opened/closed inside so
// each item is one event regardless of how the user resolves it.
func runGateReviewItem(svc *PlannerServices, snap *graph.Node, sessionTurns string, idx int) tea.Cmd {
	return func() tea.Msg {
		te := telemetry.NewEvent("gate_review_started")
		prov := svc.OrchestratorCfg.AgentProvider
		if prov == nil {
			err := fmt.Errorf("gate review: no LLM provider configured (run kai login or set ANTHROPIC_API_KEY)")
			if te != nil {
				te.SetResult("error")
				te.SetErrorClass("ai_client")
				te.Finish()
			}
			return gateReviewResultMsg{idx: idx, err: err}
		}
		// Forward provider state events through chatCh so the
		// REPL spinner shows real call lifecycle during the
		// 10s-ish audit LLM round-trip. Without this the TUI looks
		// frozen between the gate-review header and the result.
		var onState func(provider.RequestState)
		if svc.ChatActivityCh != nil {
			onState = func(state provider.RequestState) {
				select {
				case svc.ChatActivityCh <- ChatActivityEvent{
					Kind:    "provider_state",
					Summary: ProviderStateSummary(state),
					When:    state.When,
				}:
				default:
				}
			}
		}
		// Ground-truth verification: run the project's build+tests
		// over the held change BEFORE the LLM reviews it. The reviewer
		// is then given the result and told not to approve red code —
		// and we hard-enforce that below, because the reviewer has
		// been observed to talk itself out of a real failure. A
		// hang/deadlock surfaces here as a timeout = failure.
		vr := orchestrator.VerifyWorkspace(context.Background(), svc.MainRepo, gateVerifyTimeout)

		// Audit-skip fast path. If the deterministic gate already
		// classified the held snapshot as Auto AND emitted zero
		// reasons (blast radius below threshold, plan-coverage 100%,
		// no protected paths touched) AND build verification ran
		// green, the LLM audit is overhead — it's going to say "looks
		// good" 95% of the time on these. Skip it, synthesise an
		// approve result, save the user 5-15s of audit-LLM wait.
		//
		// Review-tier and Block-tier still audit. So does Auto-with-
		// reasons (rare but possible if the verdict was Auto only
		// because reasons couldn't promote it past the threshold).
		// And Auto-with-no-verify still audits — we don't trust an
		// "Auto" gate on a build we couldn't run.
		if r := synthesizeAutoAudit(snap, vr); r != nil {
			if te != nil {
				te.SetStat("blast_radius", int64(r.BlastRadius))
				te.Stats["recommendation_approve"] = 1
				te.Stats["audit_skipped_auto"] = 1
				te.Finish()
			}
			return gateReviewResultMsg{idx: idx, result: r}
		}

		// Review runs on ReviewModel (Opus by default), not the agent
		// model: the reviewer is the quality gate over the cheaper
		// agent's output, so it must not be the same model that
		// produced the change.
		res, err := gatereview.Review(context.Background(), prov, svc.ReviewModel, svc.DB, snap, gatereview.Inputs{
			SessionTurns:  sessionTurns,
			VerifySummary: vr.Summary,
			OnState:       onState,
		})
		// Hard gate: execution beats opinion. If the build/tests are
		// red, the reviewer may not recommend APPROVE no matter how
		// the diff reads. Demote the recommendation and make the
		// failure the first audit issue so the fix loop targets it.
		if err == nil && res != nil && vr.Ran && !vr.OK &&
			res.Recommendation == gatereview.RecApprove {
			res.Recommendation = gatereview.RecFixThenApprove
			res.AuditClean = false
			res.Issues = append([]gatereview.Issue{{
				Fixable:     true,
				Description: "Verification failed — the change does not build/pass tests. " + vr.Summary,
			}}, res.Issues...)
		}
		if te != nil {
			if err != nil {
				te.SetResult("error")
				te.SetErrorClass("llm")
			} else {
				te.SetStat("blast_radius", int64(res.BlastRadius))
				te.SetStat("issues", int64(len(res.Issues)))
				te.Stats["recommendation_"+strings.ToLower(string(res.Recommendation))] = 1
			}
			te.Finish()
		}
		return gateReviewResultMsg{idx: idx, result: res, err: err}
	}
}

// synthesizeAutoAudit returns a synthetic APPROVE result, skipping
// the LLM audit, when the deterministic gate verdict is already
// strong enough to clear the change without prose review. Returns
// nil when the audit should run normally.
//
// Conditions (all required):
//   - gateVerdict == "Auto" (the gate's own deterministic verdict was clean)
//   - len(gateReasons) == 0 (no held-reasons — blast radius below
//     threshold, plan-coverage 100%, no protected paths)
//   - vr.Ran && vr.OK (we ACTUALLY built and tested; "we didn't run
//     a build" is NOT a green light)
//
// Anything else (Review tier, Block tier, Auto-with-reasons, missing
// or red build) falls through to the LLM audit.
//
// Saves ~5-15s of audit-LLM wait on clean small changes — the
// "feels heavy" category from the 2026-05-24 dogfood feedback.
func synthesizeAutoAudit(snap *graph.Node, vr orchestrator.VerifyResult) *gatereview.Result {
	verdict, _ := snap.Payload["gateVerdict"].(string)
	if !strings.EqualFold(verdict, "Auto") {
		return nil
	}
	if rs := stringSliceFromPayload(snap.Payload["gateReasons"]); len(rs) > 0 {
		return nil
	}
	if !vr.Ran || !vr.OK {
		return nil
	}
	blastF, _ := snap.Payload["gateBlastRadius"].(float64)
	blast := int(blastF)
	short := snap.ID
	if len(short) > 6 {
		short = short[:6]
	}
	summary := fmt.Sprintf(
		"auto-approved: deterministic gate said Auto (blast radius %d, plan-coverage 100%%, no protected paths) and build+tests passed. LLM audit skipped.",
		blast)
	return &gatereview.Result{
		SnapshotID:     snap.ID,
		Verdict:        verdict,
		BlastRadius:    blast,
		Reasons:        nil,
		Touches:        stringSliceFromPayload(snap.Payload["gateTouches"]),
		Summary:        summary,
		AuditClean:     true,
		Issues:         nil,
		Recommendation: gatereview.RecApprove,
		RecReason:      "deterministic gate clean + verify green; LLM audit not required",
		Raw:            "",
	}
}

// stringSliceFromPayload pulls a []any out of a payload map and
// returns the string elements. Mirrors gatereview/review.go's
// stringList — re-implemented here to avoid an export.
func stringSliceFromPayload(v any) []string {
	xs, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// runGateReviewApprove / Reject delegate to workspace.Manager so the
// in-process action behaves identically to `kai gate approve|reject`.
func runGateReviewApprove(svc *PlannerServices, snap *graph.Node, idx int) tea.Cmd {
	return func() tea.Msg {
		mgr := workspace.NewManager(svc.DB)
		advanced, err := mgr.ApproveHeld(snap.ID)
		te := telemetry.NewEvent("gate_review_decision")
		if te != nil {
			te.Stats["action_approve"] = 1
			if err != nil {
				te.SetResult("error")
			}
			te.Finish()
		}
		return GateReviewActionMsg{idx: idx, kind: "approve", advRefs: advanced, err: err}
	}
}

func runGateReviewReject(svc *PlannerServices, snap *graph.Node, idx int) tea.Cmd {
	return func() tea.Msg {
		mgr := workspace.NewManager(svc.DB)
		err := mgr.RejectHeld(snap.ID)
		te := telemetry.NewEvent("gate_review_decision")
		if te != nil {
			te.Stats["action_reject"] = 1
			if err != nil {
				te.SetResult("error")
			}
			te.Finish()
		}
		return GateReviewActionMsg{idx: idx, kind: "reject", err: err}
	}
}

// runGateReviewDiff loads the patch in a goroutine. The render path
// (writing to scrollback) is on the REPL side so we keep the
// background work narrow.
func runGateReviewDiff(svc *PlannerServices, snap *graph.Node, idx int) tea.Cmd {
	return func() tea.Msg {
		patch, err := gatereview.HeldSnapshotDiff(svc.DB, snap)
		return gateReviewDiffMsg{idx: idx, patch: patch, err: err}
	}
}

// runGateReviewFix builds an agent.Options template from the live
// PlannerServices, hands it to gatereview.Fix, and wraps the result
// for the REPL dispatcher. The agent runs against the working tree
// because the held snapshot's edits are already there (the original
// agent wrote them); after the fix agent returns, gatereview.Fix
// re-stages and re-integrates so the gate gets another swing.
func runGateReviewFix(svc *PlannerServices, snap *graph.Node, issues []gatereview.Issue, idx int) tea.Cmd {
	return func() tea.Msg {
		te := telemetry.NewEvent("gate_fix_attempted")
		fixable := 0
		for _, i := range issues {
			if i.Fixable {
				fixable++
			}
		}
		if te != nil {
			te.SetStat("fixable_issues", int64(fixable))
		}

		opts := buildFixAgentOptions(svc)
		out := gatereview.Fix(context.Background(), svc.DB, gatereview.FixInputs{
			Snap:       snap,
			Issues:     issues,
			AgentOpts:  opts,
			GateConfig: svc.GateConfig,
			MaxTurns:   3,
		})

		fe := telemetry.NewEvent("gate_fix_outcome")
		if fe != nil {
			if out.Error != nil {
				fe.SetResult("error")
			} else {
				if out.AutoPromoted {
					fe.Stats["auto_promoted"] = 1
				}
				fe.Stats["new_verdict_"+strings.ToLower(string(out.NewVerdict))] = 1
			}
			fe.Finish()
		}
		if te != nil {
			te.Finish()
		}
		return GateReviewFixMsg{idx: idx, result: out}
	}
}

// buildFixAgentOptions assembles the agent.Options template used by
// the fix agent. The orchestrator's existing wiring supplies provider,
// graph, bash allowlist, and session store — same surface the chat
// agent uses, just with a tighter turn cap and a remediation-oriented
// prompt (Fix() overrides Prompt/MaxTurns/TaskName/Mode internally).
//
// Hooks are deliberately minimal here: the live UI events (assistant
// text, file diffs, bash output) flow through the REPL's
// ChatActivityCh just like during a regular chat agent run, so the
// user sees the fix agent narrating + editing in real time.
func buildFixAgentOptions(svc *PlannerServices) agent.Options {
	if svc == nil || svc.OrchestratorCfg.AgentProvider == nil {
		return agent.Options{}
	}
	// The fix agent runs on the REVIEW model (Opus/Claude), not the
	// cheaper code-agent model. The code agent is what produced the
	// held change; asking the same model to repair its own mistake is
	// asking it to re-make the reasoning error. Gate review is the
	// escalation tier — review AND fix both run on the strong model.
	// Falls back to the code-agent model only if no review model is set.
	fixModel := svc.ReviewModel
	if strings.TrimSpace(fixModel) == "" {
		fixModel = svc.OrchestratorCfg.AgentModel
	}
	opts := agent.Options{
		Projects:       svc.Projects,
		Workspace:      svc.MainRepo,
		Provider:       svc.OrchestratorCfg.AgentProvider,
		Model:          fixModel,
		Graph:          svc.OrchestratorCfg.MainGraph,
		EnableBash:     true,
		BashAllow:      svc.OrchestratorCfg.AgentBashAllow,
		MaxTotalTokens: svc.OrchestratorCfg.MaxAgentTokens,
		SessionStore:   svc.OrchestratorCfg.AgentSessionStore,
		GateConfig:     svc.GateConfig,
		RunLogDir:      svc.KaiDir, // per-turn run-log artifacts
		// Same opt-out as the chat / planner agents: trimming
		// older tool-result content rewrites the cache prefix
		// every turn, causing cache_read=0 on every fix-agent
		// turn. With caching on, the prefix should be billed
		// at read rate after the first turn.
		KeepToolResults: true,
	}
	// Stream the same activity events the chat agent does so the
	// user sees what the fix agent is doing in real time. Drop on a
	// full channel — non-blocking is the contract here.
	if svc.ChatActivityCh != nil {
		ch := svc.ChatActivityCh
		opts.Hooks = agent.Hooks{
			OnAssistantDelta: func(delta string) {
				select {
				case ch <- ChatActivityEvent{Kind: "delta", Delta: delta, When: time.Now()}:
				default:
				}
			},
			OnToolCall: func(name, _ string) {
				select {
				case ch <- ChatActivityEvent{Kind: "tool", Summary: name, When: time.Now()}:
				default:
				}
			},
			OnFileDiff: func(relPath, op, patch string, added, removed int) {
				select {
				case ch <- ChatActivityEvent{Kind: "diff", Path: relPath, Op: op, Diff: patch, Added: added, Removed: removed, When: time.Now()}:
				default:
				}
			},
		}
	}
	return opts
}

// renderGateReviewBlock formats a Result as the styled review screen
// the spec describes. Returns a single string ready for writeRaw —
// caller is responsible for appending it to scrollback. ANSI escapes
// from lipgloss survive intact (writeRaw skips word-wrapping).
//
// width is the terminal column count to wrap at (caller passes
// r.wrapWidth()). Long issue descriptions are wrapped to this
// width with a hanging indent so they don't get truncated mid-
// sentence.
func renderGateReviewBlock(snap *graph.Node, res *gatereview.Result, idx, total int, width int) string {
	short := util.BytesToHex(snap.ID)
	if len(short) > 12 {
		short = short[:12]
	}
	headerColor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warn := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	good := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	bad := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(headerColor.Render(fmt.Sprintf("━━━ Gate Review %d/%d: %s ━━━", idx+1, total, short)))
	sb.WriteString("\n")

	// Why-held block — translated into user-facing terms.
	sb.WriteString(dim.Render("Why held:"))
	sb.WriteString("\n  ")
	if len(res.Reasons) > 0 {
		sb.WriteString(strings.Join(res.Reasons, "\n  "))
	} else {
		sb.WriteString(fmt.Sprintf("verdict %s, blast radius %d", strings.ToUpper(res.Verdict), res.BlastRadius))
	}
	sb.WriteString("\n\n")

	if res.Summary != "" {
		sb.WriteString(headerColor.Render("Summary:"))
		sb.WriteString("\n  ")
		sb.WriteString(strings.ReplaceAll(res.Summary, "\n", "\n  "))
		sb.WriteString("\n\n")
	}

	sb.WriteString(headerColor.Render("Audit:"))
	sb.WriteString("\n  ")
	if res.AuditClean && len(res.Issues) == 0 {
		sb.WriteString(good.Render("CLEAN — no issues found."))
	} else {
		sb.WriteString(warn.Render(fmt.Sprintf("%d issue(s) found:", len(res.Issues))))
		// Numbered, hanging-indent issues with a blank line between
		// each. Without numbering + visual gap, multi-line wrapped
		// issues bleed into each other and the reader can't tell
		// where one ends and the next begins.
		const issueIndent = "    " // 4 spaces (visual gutter)
		// "1. [fixable] " is the longest plausible tag prefix at one
		// digit; continuation lines align under the description so
		// the wrapped text reads as a single paragraph per issue.
		// Compute prefix width per-issue to keep things tidy as the
		// list grows past 9 (extra digit shifts the indent).
		for i, iss := range res.Issues {
			tag := "human"
			tagStyle := bad
			if iss.Fixable {
				tag = "fixable"
				tagStyle = warn
			}
			numStr := fmt.Sprintf("%d. ", i+1)
			tagStr := "[" + tag + "] "
			contIndent := issueIndent + strings.Repeat(" ", len(numStr)+len(tagStr))
			issueWrap := width - len(contIndent)
			if issueWrap < 30 {
				issueWrap = 30
			}
			wrapped := wrapToWidth(iss.Description, issueWrap)
			lines := strings.Split(wrapped, "\n")
			// Blank line between items (and before the first one is
			// fine — separates from the "N issue(s) found" header).
			sb.WriteString("\n\n")
			sb.WriteString(issueIndent)
			sb.WriteString(numStr)
			sb.WriteString(tagStyle.Render("[" + tag + "]"))
			sb.WriteString(" ")
			sb.WriteString(lines[0])
			for _, cont := range lines[1:] {
				sb.WriteString("\n")
				sb.WriteString(contIndent)
				sb.WriteString(cont)
			}
		}
		// Tiny legend so the tag column reads as more than jargon.
		// First-time users hit "[human]" / "[fixable]" with no
		// referent; one dim line below the issues turns it into
		// signal.
		sb.WriteString("\n\n  ")
		sb.WriteString(dim.Render("[human] = needs your judgment · [fixable] = kai can fix it"))
	}
	sb.WriteString("\n\n")

	sb.WriteString(headerColor.Render("Recommendation:"))
	sb.WriteString(" ")
	sb.WriteString(renderRecommendation(res.Recommendation))
	if res.RecReason != "" {
		sb.WriteString("\n  ")
		sb.WriteString(dim.Render(res.RecReason))
	}
	sb.WriteString("\n")
	sb.WriteString(dim.Render("  [a]pprove  [r]eject  [s]kip  [d]iff  [f]ix  [q]uit"))
	return sb.String()
}

func renderRecommendation(r gatereview.Recommendation) string {
	good := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warn := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bad := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	switch r {
	case gatereview.RecApprove:
		return good.Render("✓ approve")
	case gatereview.RecFixThenApprove:
		return warn.Render("⚠ fix then approve")
	case gatereview.RecReject:
		return bad.Render("✗ reject")
	case gatereview.RecAsk:
		return warn.Render("? your call")
	default:
		return dim.Render("(no recommendation)")
	}
}

// handleGateReviewKey is consulted before the textarea sees the
// keystroke. Returns ok=true if the key was a review-mode shortcut and
// the caller should swallow it (don't insert into the input). The
// returned tea.Cmd may be nil for purely-local state changes.
//
// Active only when the input is empty — typing "a" mid-prompt during a
// review must keep working as a normal character. The empty-input
// guard is the same idiom the plan-confirmation menu uses.
func (r *REPL) handleGateReviewKey(key string) (bool, tea.Cmd) {
	if r.gateReview == nil {
		return false, nil
	}
	if r.gateReview.loading {
		// While the LLM call is in flight, only allow [q] to bail.
		if key == "q" || key == "esc" {
			r.exitGateReview("review canceled")
			return true, nil
		}
		return false, nil
	}
	if strings.TrimSpace(r.input.Value()) != "" {
		return false, nil
	}
	st := r.gateReview
	if st.idx >= len(st.snaps) {
		return false, nil
	}
	snap := st.snaps[st.idx]

	switch key {
	case "a":
		st.loading = true
		return true, runGateReviewApprove(r.services, snap, st.idx)
	case "r":
		st.loading = true
		return true, runGateReviewReject(r.services, snap, st.idx)
	case "s":
		r.write(styleDim.Render(fmt.Sprintf("skipped %s", shortHex(snap.ID))))
		return true, r.advanceGateReview()
	case "d":
		return true, runGateReviewDiff(r.services, snap, st.idx)
	case "f":
		// Refuse only when the audit is completely empty. Both
		// [fixable] and [human] issues go to the fix agent — its
		// output is re-held for human approve/reject, so a judgment
		// fix is a proposal, not an autonomous publish.
		if st.result == nil || len(st.result.Issues) == 0 {
			r.write(styleDim.Render(
				"no issues in this audit — use [a]pprove if the change looks fine, or [r]eject."))
			return true, nil
		}
		// Fix needs the same agent provider the chat path uses.
		// Surface the misconfiguration cleanly rather than failing
		// inside agent.Run with a stack-trace-shaped error.
		if r.services.OrchestratorCfg.AgentProvider == nil {
			r.write(styleError.Render(
				"fix needs an agent provider — run `kai auth login` (or set KAI_PROVIDER) and retry."))
			return true, nil
		}
		st.loading = true
		r.armGateReviewSpinner()
		r.write(styleDim.Render(fmt.Sprintf("→ fix agent: addressing %d issue(s)…", len(st.result.Issues))))
		return true, tea.Batch(runGateReviewFix(r.services, snap, st.result.Issues, st.idx), r.spinner.Tick)
	case "q", "esc":
		r.exitGateReview("review exited")
		return true, nil
	}
	return false, nil
}

// armGateReviewSpinner puts the REPL into the gate-review busy state
// and arms the spinner. Every path that kicks off an async gate-review
// LLM call or fix-agent run must call this — without it IsBusy() reads
// false and the TUI looks idle (Agents: 0, no spinner, live input
// prompt) while the fix agent is actually streaming edits. The caller
// must also batch r.spinner.Tick into its returned cmd so the
// animation starts.
func (r *REPL) armGateReviewSpinner() {
	r.gateReviewing = true
	r.spinnerText = pickSpinnerPhrase()
	r.spinner.Spinner = pickSpinnerStyle()
	r.renderSpinner()
}

// advanceGateReview moves to the next held item; if exhausted, exits
// review mode. Returns the cmd that fires the next LLM call (or nil
// when there's nothing left).
func (r *REPL) advanceGateReview() tea.Cmd {
	st := r.gateReview
	if st == nil {
		return nil
	}
	st.idx++
	st.result = nil
	st.err = nil
	st.fixRounds = 0 // each held item gets its own auto-fix budget
	if st.idx >= len(st.snaps) {
		r.exitGateReview(fmt.Sprintf("review complete — %d item(s) processed", len(st.snaps)))
		return nil
	}
	st.loading = true
	// Re-arm the spinner for the next item's audit LLM call.
	r.gateReviewing = true
	r.spinnerText = pickSpinnerPhrase()
	r.spinner.Spinner = pickSpinnerStyle()
	r.runStart = time.Now()
	r.lastActivity = r.runStart
	r.providerState = ChatActivityEvent{}
	r.providerStateAt = time.Time{}
	r.streamBuf = ""
	r.streamActive = false
	r.streamClosed = true
	r.renderSpinner()
	return tea.Batch(
		runGateReviewItem(r.services, st.snaps[st.idx], r.fmtRecentTurns(), st.idx),
		r.spinner.Tick,
	)
}

func (r *REPL) exitGateReview(msg string) {
	r.gateReview = nil
	r.gateReviewing = false
	r.spinnerText = ""
	r.streamBuf = ""
	r.streamActive = false
	r.streamClosed = true
	r.clearTransient()
	if msg != "" {
		r.write(styleDim.Render(msg))
	}
}

// applyGateReviewMsg routes async review-flow messages (LLM result,
// approve/reject outcome, diff text). Returns a tea.Cmd that fires the
// next step, or nil. Called from REPL.Update.
func (r *REPL) applyGateReviewMsg(msg tea.Msg) tea.Cmd {
	st := r.gateReview
	if st == nil {
		return nil
	}
	switch m := msg.(type) {
	case gateReviewResultMsg:
		if m.idx != st.idx {
			return nil // stale
		}
		st.loading = false
		// LLM call done — drop the spinner. clearTransient zeros
		// the rendered transient so the result block lands clean.
		r.gateReviewing = false
		r.spinnerText = ""
		r.clearTransient()
		if m.err != nil {
			st.err = m.err
			r.write(styleError.Render(fmt.Sprintf(
				"review %s failed: %v — [s]kip or [q]uit",
				shortHex(st.snaps[st.idx].ID), m.err)))
			return nil
		}
		st.result = m.result
		r.writeRaw(renderGateReviewBlock(st.snaps[st.idx], m.result, st.idx, len(st.snaps), r.wrapWidth()))
		// Auto-fix dispatch (Slice B): when we got here via
		// TriggerAutoGateReview AND the reviewer wants
		// FIX_THEN_APPROVE AND there's something the constrained fix
		// agent can actually do, fire the same cmd the [f] keystroke
		// would. Approve and reject deliberately remain manual.
		if st.autoMode &&
			m.result != nil &&
			m.result.Recommendation == gatereview.RecFixThenApprove &&
			len(m.result.Issues) > 0 &&
			r.services != nil &&
			r.services.OrchestratorCfg.AgentProvider != nil {
			// Cap the auto review→fix loop. Each fix re-reviews its
			// own output, which can loop indefinitely if the fixer
			// never fully satisfies the reviewer. After
			// maxAutoFixRounds, stop and leave the audit on screen
			// for the user's manual [a]pprove / [r]eject — the
			// "cap then surface for the final approve" behavior.
			if st.fixRounds >= maxAutoFixRounds {
				r.write(styleDim.Render(fmt.Sprintf(
					"auto-fix stopped after %d round(s) — your call: [a]pprove / [r]eject / [d]iff",
					st.fixRounds)))
				return nil
			}
			st.fixRounds++
			st.loading = true
			r.armGateReviewSpinner()
			r.write(styleDim.Render(fmt.Sprintf(
				"→ auto-fix (round %d/%d): addressing %d issue(s)…",
				st.fixRounds, maxAutoFixRounds, len(m.result.Issues))))
			return tea.Batch(runGateReviewFix(r.services, st.snaps[st.idx], m.result.Issues, st.idx), r.spinner.Tick)
		}
		return nil
	case GateReviewActionMsg:
		if m.idx != st.idx {
			return nil
		}
		st.loading = false
		if m.err != nil {
			r.write(styleError.Render(fmt.Sprintf("%s failed: %v", m.kind, m.err)))
			return nil
		}
		switch m.kind {
		case "approve":
			line := fmt.Sprintf("✓ approved %s", shortHex(st.snaps[st.idx].ID))
			if len(m.advRefs) > 0 {
				line += " — advanced " + strings.Join(m.advRefs, ", ")
			}
			r.write(styleDim.Render(line))
		case "reject":
			r.write(styleDim.Render(fmt.Sprintf("✗ rejected %s", shortHex(st.snaps[st.idx].ID))))
		}
		return r.advanceGateReview()
	case gateReviewDiffMsg:
		if m.idx != st.idx {
			return nil
		}
		if m.err != nil {
			r.write(styleError.Render(fmt.Sprintf("diff failed: %v", m.err)))
			return nil
		}
		if m.patch == "" {
			r.write(styleDim.Render("(empty diff)"))
			return nil
		}
		r.writeRaw(colorizeDiff(m.patch))
		// Re-anchor the action keys after the diff dump. The original
		// renderGateReviewBlock printed them once at the top of the
		// review; a large diff pushes that hint off-screen and the
		// user is left staring at hunks with no obvious next move and
		// only [q]/[esc] (which exits review) as a visible escape
		// route. Repeat the hint at the bottom of scrollback so the
		// current options are always one glance away. Also remind the
		// user that pressing d again rebuilds the diff if they
		// scrolled away.
		dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		r.write("")
		r.write(dim.Render("  (still reviewing — diff above does not change state)"))
		r.write(dim.Render("  [a]pprove  [r]eject  [s]kip  [d]iff  [f]ix  [q]uit"))
		return nil
	case GateReviewFixMsg:
		if m.idx != st.idx {
			return nil
		}
		st.loading = false
		out := m.result
		if out == nil || out.Error != nil {
			// Fix run ended without a usable result — drop the
			// gate-review busy state so the spinner stops and the
			// item's audit menu reads as idle, awaiting the user.
			r.gateReviewing = false
			r.spinnerText = ""
			r.clearTransient()
			detail := "unknown error"
			if out != nil && out.Error != nil {
				detail = out.Error.Error()
			}
			r.write(styleError.Render(fmt.Sprintf("✗ fix failed: %s", detail)))
			if out != nil && out.AssistantText != "" {
				r.write(styleDim.Render("agent said: " + truncateForLog(out.AssistantText, 240)))
			}
			r.write(styleDim.Render(
				"recommendation downgraded to ? — your call. [a]pprove / [r]eject / [s]kip / [q]uit"))
			// Mutate the on-screen recommendation so the user sees
			// the audit-failed state plainly. Issues stay rendered
			// from the previous block; the demoted rec is the
			// signal the spec asks for.
			if st.result != nil {
				st.result.Recommendation = gatereview.RecAsk
			}
			return nil
		}
		if out.AutoPromoted {
			r.write(styleDim.Render(fmt.Sprintf(
				"✓ fix succeeded — new snapshot %s auto-promoted (advanced %s). Original %s rejected.",
				shortHex(out.NewSnapshotID), strings.Join(out.AdvancedRefs, ", "), shortHex(st.snaps[m.idx].ID))))
			return r.advanceGateReview()
		}
		// New snapshot was held again. Replace the current item with
		// the new one and re-fire the LLM review so the user sees a
		// fresh audit on the post-fix change. The original snap is
		// already rejected by Fix(); we don't enqueue it again.
		newNode, err := r.services.DB.GetNode(out.NewSnapshotID)
		if err != nil || newNode == nil {
			r.write(styleError.Render(fmt.Sprintf(
				"fix produced snapshot %s but it can't be loaded for re-review: %v",
				shortHex(out.NewSnapshotID), err)))
			return r.advanceGateReview()
		}
		r.write(styleDim.Render(fmt.Sprintf(
			"⚠ fix landed but new snapshot %s is %s (blast %d). Re-reviewing…",
			shortHex(out.NewSnapshotID), strings.ToUpper(string(out.NewVerdict)), out.NewBlast)))
		st.snaps[st.idx] = newNode
		st.result = nil
		st.loading = true
		return runGateReviewItem(r.services, newNode, r.fmtRecentTurns(), st.idx)
	}
	return nil
}

// colorizeDiff adds ANSI red/green to +/- lines so the inlined patch
// is readable in scrollback. The patch from gatereview.HeldSnapshotDiff
// is intentionally uncolored (LLM-friendly); this is the render-time
// transformation.
func colorizeDiff(patch string) string {
	const red = "\x1b[31m"
	const green = "\x1b[32m"
	const cyan = "\x1b[36m"
	const reset = "\x1b[0m"
	var sb strings.Builder
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --kai") ||
			strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ "):
			sb.WriteString(cyan)
			sb.WriteString(line)
			sb.WriteString(reset)
		case strings.HasPrefix(line, "+"):
			sb.WriteString(green)
			sb.WriteString(line)
			sb.WriteString(reset)
		case strings.HasPrefix(line, "-"):
			sb.WriteString(red)
			sb.WriteString(line)
			sb.WriteString(reset)
		default:
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func shortHex(id []byte) string {
	h := util.BytesToHex(id)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
