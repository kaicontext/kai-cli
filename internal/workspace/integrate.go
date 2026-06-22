// Package workspace provides integration operations for workspaces.
package workspace

import (
	"context"
	"fmt"

	"github.com/kaicontext/kai-core/merge"

	"kai/internal/graph"
	"kai/internal/safetygate"
	"kai/internal/util"
)

// IntegrateOptions controls how a workspace is integrated. The zero
// value is equivalent to plain Integrate: no resolutions, gate runs
// with default config.
type IntegrateOptions struct {
	// Resolutions, when non-nil, supplies merged content for paths that
	// would otherwise conflict. Used by `kai resolve`.
	Resolutions map[string][]byte
	// SkipGate bypasses safety classification and forces Verdict=Auto.
	// Used by `kai review approve` (the human already approved) and by
	// tests that intentionally bypass the gate.
	SkipGate bool
	// GateConfig overrides the default gate configuration. nil → defaults.
	// CLI callers should populate this from safetygate.LoadConfig.
	GateConfig *safetygate.Config
}

// IntegrateResult contains the result of integrating a workspace.
type IntegrateResult struct {
	ResultSnapshot    []byte
	AppliedChangeSets [][]byte
	Conflicts         []Conflict
	AutoResolved      int

	// Decision is the safety gate's verdict on this integration. It is
	// populated by integrateInternal once the gate is wired in (task 3).
	// Until then it is nil; PublishToRef / PublishAtTarget treat nil as
	// "no gate ran" and allow the publish, preserving today's behavior.
	Decision *IntegrationDecision
}

// IntegrationDecision is a workspace-package-local view of the safety
// gate's output. We keep a thin local type rather than importing
// safetygate.Decision directly so this package stays free of any
// gate-specific dependencies — the gate populates these fields and
// callers (kai integrate, kai resolve, kai review) read Verdict.
//
// The string values must match safetygate.Verdict ("auto"/"review"/"block").
type IntegrationDecision struct {
	Verdict     string
	BlastRadius int
	Reasons     []string
	Touches     []string
}

// Integrate merges a workspace's changes into a target snapshot.
func (m *Manager) Integrate(nameOrID string, targetSnapshotID []byte) (*IntegrateResult, error) {
	return m.integrateInternal(nameOrID, targetSnapshotID, IntegrateOptions{})
}

// IntegrateWithResolutions merges a workspace's changes into a target snapshot,
// using the provided resolutions for conflicting paths. Paths in the resolutions
// map contain the resolved file content and will be used instead of reporting a conflict.
func (m *Manager) IntegrateWithResolutions(nameOrID string, targetSnapshotID []byte, resolutions map[string][]byte) (*IntegrateResult, error) {
	return m.integrateInternal(nameOrID, targetSnapshotID, IntegrateOptions{Resolutions: resolutions})
}

// IntegrateWithOptions is the full-control entry point. Used by `kai
// review approve` (SkipGate=true) and by callers that need to override
// the gate config.
func (m *Manager) IntegrateWithOptions(nameOrID string, targetSnapshotID []byte, opts IntegrateOptions) (*IntegrateResult, error) {
	return m.integrateInternal(nameOrID, targetSnapshotID, opts)
}

// integrateInternal is the shared implementation. Behavior:
//
//   1. Validate workspace and target snapshot.
//   2. Compute wsModified (paths where ws head ≠ ws base).
//   3. Run the safety gate on wsModified, producing a Decision.
//   4. Fast-forward case (base == target, no resolutions): create a
//      new snapshot node carrying the gate metadata, with HAS_FILE
//      edges copied from ws head. (We don't reuse ws.HeadSnapshot
//      directly so every integration produces a tagged snapshot —
//      uniform model for `kai review`.)
//   5. Non-FF case: detect conflicts, semantic merge, build merged
//      snapshot with gate metadata, commit.
//
// The gate's verdict is attached to the result snapshot's payload AND
// returned in IntegrateResult.Decision. Publish consults Decision to
// decide whether to advance team-visible refs.
func (m *Manager) integrateInternal(nameOrID string, targetSnapshotID []byte, opts IntegrateOptions) (*IntegrateResult, error) {
	resolutions := opts.Resolutions
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}
	if ws.Status == StatusClosed {
		return nil, fmt.Errorf("workspace is closed")
	}
	if len(ws.OpenChangeSets) == 0 {
		return nil, fmt.Errorf("workspace has no changes to integrate")
	}

	// Verify target snapshot exists
	targetSnap, err := m.db.GetNode(targetSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting target snapshot: %w", err)
	}
	if targetSnap == nil {
		return nil, fmt.Errorf("target snapshot not found")
	}
	if targetSnap.Kind != graph.KindSnapshot {
		return nil, fmt.Errorf("target must be a snapshot, got %s", targetSnap.Kind)
	}

	baseHex := util.BytesToHex(ws.BaseSnapshot)
	targetHex := util.BytesToHex(targetSnapshotID)

	// Load base and head files up-front. Both the FF and merge paths
	// need them: FF needs head to copy HAS_FILE edges and the wsModified
	// set for the gate; merge needs all three. Loading target lazily
	// below to avoid an extra read on FF.
	baseFiles, err := m.getSnapshotFileMap(ws.BaseSnapshot)
	if err != nil {
		return nil, fmt.Errorf("getting base files: %w", err)
	}
	headFiles, err := m.getSnapshotFileMap(ws.HeadSnapshot)
	if err != nil {
		return nil, fmt.Errorf("getting head files: %w", err)
	}

	// Find files modified in workspace (base -> head). Computed once
	// and shared by the gate, the FF path, and the merge path.
	wsModified := make(map[string]bool)
	for path, headDigest := range headFiles {
		baseDigest, exists := baseFiles[path]
		if !exists || baseDigest != headDigest {
			wsModified[path] = true
		}
	}
	// Files deleted in workspace
	for path := range baseFiles {
		if _, exists := headFiles[path]; !exists {
			wsModified[path] = true
		}
	}

	// Run the safety gate on the workspace's contribution. The verdict
	// applies to both FF and merge paths — the agent's intent is the
	// same regardless of how it lands.
	decision, err := classifyForGate(m.db, sortedKeys(wsModified), opts)
	if err != nil {
		return nil, fmt.Errorf("classifying integration: %w", err)
	}

	// Fast-forward: target hasn't changed since we branched. Build a
	// new snapshot identical to ws.HeadSnapshot but tagged with gate
	// metadata. We don't reuse ws.HeadSnapshot directly because the
	// integration snapshot must carry the verdict and have its own id
	// for `kai review` to reference later.
	if baseHex == targetHex && resolutions == nil {
		snapID, err := m.buildFFSnapshot(ws, targetHex, decision, sortedKeys(wsModified))
		if err != nil {
			return nil, fmt.Errorf("building fast-forward snapshot: %w", err)
		}
		return &IntegrateResult{
			ResultSnapshot:    snapID,
			AppliedChangeSets: ws.OpenChangeSets,
			AutoResolved:      0,
			Decision:          decision,
		}, nil
	}

	// Non-FF: load target files now (deferred above to skip an extra
	// read on the FF path).
	targetFiles, err := m.getSnapshotFileMap(targetSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting target files: %w", err)
	}

	// Find files modified in target (base -> target)
	targetModified := make(map[string]bool)
	for path, targetDigest := range targetFiles {
		baseDigest, exists := baseFiles[path]
		if !exists || baseDigest != targetDigest {
			targetModified[path] = true
		}
	}
	// Files deleted in target
	for path := range baseFiles {
		if _, exists := targetFiles[path]; !exists {
			targetModified[path] = true
		}
	}

	// Attempt semantic merge for files modified on both sides
	var conflicts []Conflict
	merger := merge.NewMerger()
	semanticMerged := make(map[string][]byte)

	for path := range wsModified {
		if !targetModified[path] {
			continue
		}

		// Check if the user provided a resolution for this path
		if resolutions != nil {
			if content, ok := resolutions[path]; ok {
				semanticMerged[path] = content
				continue
			}
		}

		baseDigest := baseFiles[path]
		headDigest := headFiles[path]
		targetDigest := targetFiles[path]

		if baseDigest == "" || headDigest == "" || targetDigest == "" {
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both workspace and target",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		baseContent, err := m.db.ReadObject(baseDigest)
		headContent, err2 := m.db.ReadObject(headDigest)
		targetContent, err3 := m.db.ReadObject(targetDigest)
		if err != nil || err2 != nil || err3 != nil {
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both workspace and target (content not available for semantic merge)",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		lang := normalizeMergeLang(path)
		if lang == "" {
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both workspace and target (unsupported language for semantic merge)",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		mergeResult, mergeErr := merger.MergeFiles(
			map[string][]byte{path: baseContent},
			map[string][]byte{path: headContent},
			map[string][]byte{path: targetContent},
			lang,
		)
		if mergeErr != nil {
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: fmt.Sprintf("Semantic merge failed: %v", mergeErr),
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		if !mergeResult.Success {
			for _, mc := range mergeResult.Conflicts {
				conflicts = append(conflicts, Conflict{
					Path:        path,
					Description: fmt.Sprintf("[%s] %s", mc.Kind, mc.Message),
					BaseDigest:  baseDigest,
					HeadDigest:  headDigest,
					NewDigest:   targetDigest,
				})
			}
			continue
		}

		if content, ok := mergeResult.Files[path]; ok {
			semanticMerged[path] = content
		}
	}

	if len(conflicts) > 0 {
		// Save conflict state so `kai resolve` can pick it up
		m.SaveConflictState(ws.ID, targetSnapshotID, conflicts)

		return &IntegrateResult{
			Conflicts: conflicts,
		}, nil
	}

	// Build merged file set: start with target, apply workspace changes
	mergedFiles := make(map[string]string)
	for path, digest := range targetFiles {
		mergedFiles[path] = digest
	}
	for path, digest := range headFiles {
		if wsModified[path] {
			mergedFiles[path] = digest
		}
	}
	for path := range baseFiles {
		if _, existsInHead := headFiles[path]; !existsInHead {
			delete(mergedFiles, path)
		}
	}

	// Create the merged snapshot
	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Write semantically merged files
	semanticMergedNodes := make(map[string]*graph.Node)
	for path, content := range semanticMerged {
		digest, err := m.db.WriteObject(content)
		if err != nil {
			return nil, fmt.Errorf("writing merged content for %s: %w", path, err)
		}
		lang := normalizeMergeLang(path)
		filePayload := map[string]interface{}{
			"path":   path,
			"lang":   lang,
			"digest": digest,
		}
		fileID, err := m.db.InsertNode(tx, graph.KindFile, filePayload)
		if err != nil {
			return nil, fmt.Errorf("inserting merged file node for %s: %w", path, err)
		}
		semanticMergedNodes[path] = &graph.Node{ID: fileID, Kind: graph.KindFile, Payload: filePayload}
		mergedFiles[path] = digest
	}

	autoResolved := len(semanticMerged)

	mergedSnapPayload := map[string]interface{}{
		"sourceType":     "merged",
		"sourceRef":      fmt.Sprintf("integrate:%s->%s", util.BytesToHex(ws.ID)[:12], targetHex[:12]),
		"fileCount":      len(mergedFiles),
		"createdAt":      util.NowMs(),
		"integratedFrom": util.BytesToHex(ws.ID),
		"targetSnapshot": targetHex,
	}
	if autoResolved > 0 {
		mergedSnapPayload["autoResolved"] = autoResolved
	}
	applyDecisionToPayload(mergedSnapPayload, decision, sortedKeys(wsModified))

	mergedSnapID, err := m.db.InsertNode(tx, graph.KindSnapshot, mergedSnapPayload)
	if err != nil {
		return nil, fmt.Errorf("inserting merged snapshot: %w", err)
	}

	headFileNodes, err := m.getSnapshotFileNodes(ws.HeadSnapshot)
	if err != nil {
		return nil, err
	}
	targetFileNodes, err := m.getSnapshotFileNodes(targetSnapshotID)
	if err != nil {
		return nil, err
	}

	for path := range mergedFiles {
		var fileNode *graph.Node
		if semanticMergedNodes[path] != nil {
			fileNode = semanticMergedNodes[path]
		} else if wsModified[path] {
			fileNode = headFileNodes[path]
		} else {
			fileNode = targetFileNodes[path]
		}
		if fileNode != nil {
			if err := m.db.InsertEdge(tx, mergedSnapID, graph.EdgeHasFile, fileNode.ID, nil); err != nil {
				return nil, fmt.Errorf("inserting HAS_FILE edge: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &IntegrateResult{
		ResultSnapshot:    mergedSnapID,
		AppliedChangeSets: ws.OpenChangeSets,
		AutoResolved:      autoResolved,
		Decision:          decision,
	}, nil
}

// getSnapshotFileMap returns a map of path -> digest for a snapshot.
func (m *Manager) getSnapshotFileMap(snapshotID []byte) (map[string]string, error) {
	edges, err := m.db.GetEdges(snapshotID, graph.EdgeHasFile)
	if err != nil {
		return nil, err
	}

	fileMap := make(map[string]string)
	for _, edge := range edges {
		node, err := m.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if node != nil {
			path, _ := node.Payload["path"].(string)
			digest, _ := node.Payload["digest"].(string)
			fileMap[path] = digest
		}
	}

	return fileMap, nil
}

// getSnapshotFileNodes returns a map of path -> Node for a snapshot.
func (m *Manager) getSnapshotFileNodes(snapshotID []byte) (map[string]*graph.Node, error) {
	edges, err := m.db.GetEdges(snapshotID, graph.EdgeHasFile)
	if err != nil {
		return nil, err
	}

	nodeMap := make(map[string]*graph.Node)
	for _, edge := range edges {
		node, err := m.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if node != nil {
			path, _ := node.Payload["path"].(string)
			nodeMap[path] = node
		}
	}

	return nodeMap, nil
}

// classifyForGate runs the safety gate on the workspace's modified
// paths and returns a workspace-local IntegrationDecision. SkipGate
// short-circuits to an Auto verdict so review-approval re-runs and
// tests don't pay the classification cost.
func classifyForGate(g safetygate.Grapher, wsModified []string, opts IntegrateOptions) (*IntegrationDecision, error) {
	if opts.SkipGate {
		return &IntegrationDecision{
			Verdict: string(safetygate.Auto),
			Reasons: []string{"gate skipped (explicit caller opt-in)"},
		}, nil
	}
	cfg := safetygate.DefaultConfig()
	if opts.GateConfig != nil {
		cfg = *opts.GateConfig
	}
	d, err := safetygate.Classify(context.Background(), wsModified, g, cfg)
	if err != nil {
		return nil, err
	}
	return &IntegrationDecision{
		Verdict:     string(d.Verdict),
		BlastRadius: d.BlastRadius,
		Reasons:     d.Reasons,
		Touches:     d.Touches,
	}, nil
}

// applyDecisionToPayload writes the gate verdict onto a snapshot
// payload map in place. Persisting on the snapshot lets `kai review`
// list and inspect held integrations later without re-running the
// gate.
//
// changedPaths is the actual set of files this snapshot changed
// (typically sortedKeys(wsModified)). It's used as the fallback for
// gateTouches when the classifier's downstream-affected set is empty
// — e.g. 0-blast-radius edits like comment-only changes. Every
// snapshot that goes through the gate must carry gateTouches; no
// downstream consumer should ever have to recompute changed paths
// from a snapshot diff.
func applyDecisionToPayload(payload map[string]interface{}, d *IntegrationDecision, changedPaths []string) {
	if payload == nil || d == nil {
		return
	}
	payload["gateVerdict"] = d.Verdict
	payload["gateBlastRadius"] = d.BlastRadius
	if len(d.Reasons) > 0 {
		payload["gateReasons"] = d.Reasons
	}
	touches := d.Touches
	if len(touches) == 0 {
		touches = changedPaths
	}
	payload["gateTouches"] = touches
}

// buildFFSnapshot creates a new Snapshot node identical in file content
// to ws.HeadSnapshot but tagged with gate metadata. The HAS_FILE edges
// are copied from ws.HeadSnapshot. Used on the fast-forward path so
// every integration produces a tagged snapshot — uniform model.
func (m *Manager) buildFFSnapshot(ws *Workspace, targetHex string, decision *IntegrationDecision, changedPaths []string) ([]byte, error) {
	headFileNodes, err := m.getSnapshotFileNodes(ws.HeadSnapshot)
	if err != nil {
		return nil, err
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	payload := map[string]interface{}{
		"sourceType":     "merged-ff",
		"sourceRef":      fmt.Sprintf("integrate-ff:%s->%s", util.BytesToHex(ws.ID)[:12], targetHex[:12]),
		"fileCount":      len(headFileNodes),
		"createdAt":      util.NowMs(),
		"integratedFrom": util.BytesToHex(ws.ID),
		"targetSnapshot": targetHex,
	}
	applyDecisionToPayload(payload, decision, changedPaths)

	snapID, err := m.db.InsertNode(tx, graph.KindSnapshot, payload)
	if err != nil {
		return nil, fmt.Errorf("inserting fast-forward snapshot: %w", err)
	}

	for _, fileNode := range headFileNodes {
		if fileNode == nil {
			continue
		}
		if err := m.db.InsertEdge(tx, snapID, graph.EdgeHasFile, fileNode.ID, nil); err != nil {
			return nil, fmt.Errorf("inserting HAS_FILE edge: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing fast-forward snapshot: %w", err)
	}
	return snapID, nil
}

// sortedKeys returns the keys of a string-keyed set in deterministic order.
// Used to make gate input order-independent across map iterations.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// stdlib sort.Strings would do, but avoiding the import for a
	// trivial in-place sort.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
