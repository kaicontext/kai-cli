package orchestrator

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/projects"
	"kai/internal/safetygate"
)

// integrate_per_project.go: per-project gate fanout for multi-root
// orchestrator runs.
//
// Background. integrateOneAgent's absorb step is multi-root aware —
// it fans file writes out to every project the agent touched. v0.31.3
// extended that to capture (every touched project gets a snapshot).
// This file extends it to classify, snapshot decoration, and verdict
// aggregation: each project's snapshot now carries its OWN
// gate-verdict metadata based on its OWN diff, not the primary's.
//
// Subsequently made per-project too:
//
//   - snap.latest rollback on non-Auto verdict — every touched project
//     rolls back, all-or-nothing (orchestrator.go, "Held:" block).
//   - Build check + build-fix loop — each touched project is checked
//     against its OWN pre-absorb baseline (projectState.baseline) and
//     fixed/rolled-back independently (orchestrator.go build-check loop,
//     Phase B 2026-05-29).
//
// What's STILL primary-only:
//
//   - Plan-coverage check (computed once against aggregate ChangedPaths).
//     Lower-impact than the above; not yet fanned out per project.

// projectState bundles everything we need to per-project-gate one
// project the agent touched. Constructed from an absorbTarget; the
// primary's state reuses the existing db / cfg.GateConfig passed
// into integrateOneAgent, secondaries pull their DB and gate config
// from the projects.Set (populated by Set.Open at TUI startup).
type projectState struct {
	target  absorbTarget
	db      *graph.DB
	gateCfg safetygate.Config
	// changed is the subset of integrate-level changed paths that
	// belong to this project, with the project-name prefix stripped
	// so the path is project-relative (matches what each project's
	// DB indexes). Empty when the agent touched the project's mainDir
	// during absorb but the absorb step then reported no net change.
	changed    []string
	prevLatest []byte
	newLatest  []byte
	verdict    safetygate.Decision
	// baseline is this project's build-check state captured BEFORE the
	// agent's edits were absorbed (its real dir was still pristine).
	// The per-project build gate blocks only on packages this project
	// NEWLY broke vs this baseline — see the build-check loop in
	// integrateOneAgent and newFailures().
	baseline buildCheckResult
	// captureFailed flags states whose post-absorb `kai capture`
	// errored. We still keep the state in the slice so reporting can
	// surface the partial outcome, but we don't try to classify or
	// decorate.
	captureFailed bool
	// gateSkipped flags non-primary projects with no graph DB
	// (uninitialized). The agent's writes are on disk, but we can't
	// open a snapshot graph to record a verdict against, so per-
	// project gating doesn't apply.
	gateSkipped bool
}

// buildProjectStates pairs each absorbTarget with the right
// graph.DB + safety-gate config and the per-project subset of
// `changed`. The primary's state reuses the orchestrator's existing
// db / cfg.GateConfig (passed in); secondaries use the per-project
// values populated by Set.Open.
//
// Single-root callers (cfg.Projects nil or <= 1 project) get one
// state with the primary's plumbing — same shape as the legacy code
// path so the downstream loop is uniform.
func buildProjectStates(cfg Config, primaryDB *graph.DB, mainRepo string, targets []absorbTarget, changed []string) []projectState {
	out := make([]projectState, 0, len(targets))
	multiRoot := cfg.Projects != nil && len(cfg.Projects.Projects()) > 1

	for _, t := range targets {
		s := projectState{target: t}

		if !multiRoot {
			// Single-root: one target, one project, paths are bare
			// (no project-name prefix in `changed`). Use the
			// orchestrator's primary plumbing.
			s.db = primaryDB
			s.gateCfg = cfg.GateConfig
			s.changed = append([]string(nil), changed...)
			out = append(out, s)
			continue
		}

		// Multi-root. The primary target's mainDir == mainRepo; reuse
		// the passed-in db + gateCfg for it (avoids a redundant
		// per-project lookup and stays consistent with how the rest
		// of the integrate path knows the primary's plumbing).
		if t.mainDir == mainRepo {
			s.db = primaryDB
			s.gateCfg = cfg.GateConfig
		} else {
			// Secondary project. Look up via name to get its DB +
			// gate config from the set.
			p := lookupProjectByName(cfg.Projects, t.name)
			if p == nil {
				// Shouldn't happen — targets come from the set in
				// the first place. Skip cleanly.
				s.gateSkipped = true
			} else if p.DB == nil {
				// Pinned-but-uninitialized: no snapshot graph
				// exists for this project. Captures already wrote
				// the file changes to disk; we just can't classify
				// or decorate.
				s.gateSkipped = true
			} else {
				s.db = p.DB
				s.gateCfg = p.GateConfig
			}
		}
		s.changed = filterChangedForProject(changed, t.name)
		out = append(out, s)
	}
	return out
}

// lookupProjectByName finds the projects.Project in the set whose
// directory basename matches name. Returns nil on no match.
//
// Directory basename is the canonical multi-root identifier (matches
// projectDirBasename used by buildProjectStates' caller and by the
// absorb step that produced the target list).
func lookupProjectByName(set *projects.Set, name string) *projects.Project {
	if set == nil || name == "" {
		return nil
	}
	for _, p := range set.Projects() {
		if projectDirBasename(p) == name {
			return p
		}
	}
	return nil
}

// filterChangedForProject returns the subset of paths in `changed`
// that belong to the named project, with the project-name prefix
// stripped so the remaining path is project-relative.
//
// The integrate-level `changed` slice is built by the absorb step
// in two shapes:
//
//	multi-root: "<project-name>/<rel/path>"
//	single-root: "<rel/path>"           (this filter is bypassed for single-root)
//
// We only call this in the multi-root branch, so we expect the
// prefixed shape.
func filterChangedForProject(changed []string, name string) []string {
	if name == "" {
		return append([]string(nil), changed...)
	}
	prefix := name + "/"
	var out []string
	for _, p := range changed {
		if strings.HasPrefix(p, prefix) {
			out = append(out, strings.TrimPrefix(p, prefix))
		}
	}
	return out
}

// classifyPerProject runs safetygate.Classify against each state's
// own DB and gate config, in parallel-safe sequence. Skips states
// flagged captureFailed / gateSkipped / empty-changed (no project-
// level diff to classify).
//
// Mutates each state's `verdict` field in place. Errors during one
// project's classify don't abort the others — we record the error on
// the state and continue.
func classifyPerProject(ctx context.Context, states []projectState) {
	for i := range states {
		s := &states[i]
		if s.captureFailed || s.gateSkipped || len(s.changed) == 0 || len(s.newLatest) == 0 {
			continue
		}
		// Scope blast radius to THIS snapshot so stale cross-snapshot
		// edges don't inflate it (the edge-accumulation bug that held a
		// 0-blast leaf change at blast 2). s.newLatest is the snapshot
		// we just captured + are gating.
		gateCfg := s.gateCfg
		gateCfg.SnapshotID = s.newLatest
		v, err := safetygate.Classify(ctx, s.changed, s.db, gateCfg)
		if err != nil {
			// Record a synthetic Review verdict naming the error so
			// the user sees SOMETHING in this project's gate slot
			// rather than a silent gap.
			s.verdict = safetygate.Decision{
				Verdict: safetygate.Review,
				Reasons: []string{fmt.Sprintf("classify error: %v", err)},
			}
			continue
		}
		s.verdict = v
	}
}

// aggregateVerdicts folds N per-project verdicts into one
// run-level decision. Worst-of-N for the verdict tier (any Block →
// Block; any Review → Review; all Auto → Auto). Blast radius sums.
// Reasons get prefixed with the project name so the user can tell
// which project flagged what.
//
// Skipped/failed states contribute nothing to the aggregate, but
// captureFailed states force at least Review — the agent's edits
// landed on disk but never got captured, which is uncertain work.
func aggregateVerdicts(states []projectState) safetygate.Decision {
	agg := safetygate.Decision{Verdict: safetygate.Auto}
	for _, s := range states {
		if s.captureFailed {
			if agg.Verdict == safetygate.Auto {
				agg.Verdict = safetygate.Review
			}
			agg.Reasons = append(agg.Reasons, fmt.Sprintf(
				"[%s] capture failed; edits on disk but not in snapshot graph", s.target.name))
			continue
		}
		if s.gateSkipped || len(s.changed) == 0 {
			continue
		}
		if escalates(s.verdict.Verdict, agg.Verdict) {
			agg.Verdict = s.verdict.Verdict
		}
		agg.BlastRadius += s.verdict.BlastRadius
		for _, r := range s.verdict.Reasons {
			agg.Reasons = append(agg.Reasons, fmt.Sprintf("[%s] %s", s.target.name, r))
		}
		// Touches stay path-prefixed across the aggregate so each
		// touch is unambiguous about which project it belongs to.
		for _, t := range s.verdict.Touches {
			if s.target.name != "" {
				agg.Touches = append(agg.Touches, s.target.name+"/"+t)
			} else {
				agg.Touches = append(agg.Touches, t)
			}
		}
	}
	return agg
}

// escalates returns true when newV is a more-conservative verdict
// tier than currentV. Tier order: Auto < Review < Block.
func escalates(newV, currentV safetygate.Verdict) bool {
	tier := func(v safetygate.Verdict) int {
		switch v {
		case safetygate.Auto:
			return 0
		case safetygate.Review:
			return 1
		case safetygate.Block:
			return 2
		}
		return 0
	}
	return tier(newV) > tier(currentV)
}

// decorateProjectSnap writes per-project gate metadata onto each
// state's new snapshot. Mirrors the legacy single-project decoration
// at the bottom of integrateOneAgent but loops per project so each
// kai-server snapshot, kai snapshot, etc. gets ITS OWN verdict
// metadata that `kai gate list` can render.
//
// taskName + acceptance criteria + agent-error context come from the
// run and are the same across projects — they get duplicated onto
// each snapshot intentionally; consumers shouldn't have to cross-
// reference projects to know what plan produced the held change.
func decorateProjectSnap(s *projectState, run *AgentRun) {
	if s.captureFailed || s.gateSkipped || len(s.newLatest) == 0 {
		return
	}
	snap, err := s.db.GetNode(s.newLatest)
	if err != nil || snap == nil || snap.Payload == nil {
		return
	}
	if len(s.prevLatest) > 0 {
		snap.Payload["targetSnapshot"] = hex.EncodeToString(s.prevLatest)
	}
	snap.Payload["gateVerdict"] = string(s.verdict.Verdict)
	snap.Payload["gateBlastRadius"] = s.verdict.BlastRadius
	if len(s.verdict.Reasons) > 0 {
		snap.Payload["gateReasons"] = s.verdict.Reasons
	}
	touches := s.verdict.Touches
	if len(touches) == 0 {
		touches = s.changed
	}
	snap.Payload["gateTouches"] = touches
	snap.Payload["integratedFrom"] = hex.EncodeToString(s.prevLatest)
	snap.Payload["orchestratorAgent"] = run.Task.Name
	if len(run.Task.AcceptanceCriteria) > 0 {
		snap.Payload["acceptanceCriteria"] = run.Task.AcceptanceCriteria
	}
	_ = s.db.UpdateNodePayload(s.newLatest, snap.Payload)
}
