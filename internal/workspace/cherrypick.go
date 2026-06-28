package workspace

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-core/merge"

	"kai/internal/dirio"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/util"
)

// CherryPickResult contains the result of applying a changeset onto a base.
type CherryPickResult struct {
	ResultSnapshot  []byte
	ResultChangeSet []byte
	Conflicts       []Conflict
	AppliedFiles    int
	AutoResolved    int // files auto-merged via semantic 3-way merge
}

// CherryPick applies a changeset onto a target snapshot.
func (m *Manager) CherryPick(changeSetID, targetSnapshotID []byte) (*CherryPickResult, error) {
	csNode, err := m.db.GetNode(changeSetID)
	if err != nil {
		return nil, fmt.Errorf("getting changeset: %w", err)
	}
	if csNode == nil || csNode.Kind != graph.KindChangeSet {
		return nil, fmt.Errorf("changeset not found")
	}

	baseHex, _ := csNode.Payload["base"].(string)
	headHex, _ := csNode.Payload["head"].(string)
	if baseHex == "" || headHex == "" {
		return nil, fmt.Errorf("changeset missing base/head")
	}

	baseID, err := util.HexToBytes(baseHex)
	if err != nil {
		return nil, fmt.Errorf("invalid base: %w", err)
	}
	headID, err := util.HexToBytes(headHex)
	if err != nil {
		return nil, fmt.Errorf("invalid head: %w", err)
	}

	targetSnap, err := m.db.GetNode(targetSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting target snapshot: %w", err)
	}
	if targetSnap == nil || targetSnap.Kind != graph.KindSnapshot {
		return nil, fmt.Errorf("target snapshot not found")
	}

	baseFiles, err := m.getSnapshotFileMap(baseID)
	if err != nil {
		return nil, fmt.Errorf("getting base files: %w", err)
	}
	headFiles, err := m.getSnapshotFileMap(headID)
	if err != nil {
		return nil, fmt.Errorf("getting head files: %w", err)
	}
	targetFiles, err := m.getSnapshotFileMap(targetSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting target files: %w", err)
	}

	csModified := make(map[string]bool)
	for path, headDigest := range headFiles {
		baseDigest, exists := baseFiles[path]
		if !exists || baseDigest != headDigest {
			csModified[path] = true
		}
	}
	for path := range baseFiles {
		if _, exists := headFiles[path]; !exists {
			csModified[path] = true
		}
	}

	targetModified := make(map[string]bool)
	for path, targetDigest := range targetFiles {
		baseDigest, exists := baseFiles[path]
		if !exists || baseDigest != targetDigest {
			targetModified[path] = true
		}
	}
	for path := range baseFiles {
		if _, exists := targetFiles[path]; !exists {
			targetModified[path] = true
		}
	}

	// Attempt semantic merge for files modified on both sides
	var conflicts []Conflict
	merger := merge.NewMerger()
	semanticMerged := make(map[string][]byte) // path -> merged content (from semantic merge)

	for path := range csModified {
		if !targetModified[path] {
			continue
		}
		// Both sides modified this file — try semantic merge
		baseDigest := baseFiles[path]
		headDigest := headFiles[path]
		targetDigest := targetFiles[path]

		if baseDigest == "" || headDigest == "" || targetDigest == "" {
			// File created/deleted on one side — can't semantic merge
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both changeset and target",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		// Read file content from object store
		baseContent, err := m.db.ReadObject(baseDigest)
		headContent, err2 := m.db.ReadObject(headDigest)
		targetContent, err3 := m.db.ReadObject(targetDigest)
		if err != nil || err2 != nil || err3 != nil {
			// Can't read content — fall back to file-level conflict
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both changeset and target (content not available for semantic merge)",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		// Detect language for semantic merge
		lang := normalizeMergeLang(path)
		if lang == "" {
			conflicts = append(conflicts, Conflict{
				Path:        path,
				Description: "File modified in both changeset and target (unsupported language for semantic merge)",
				BaseDigest:  baseDigest,
				HeadDigest:  headDigest,
				NewDigest:   targetDigest,
			})
			continue
		}

		// Attempt semantic 3-way merge
		mergeResult, mergeErr := merger.MergeFiles(
			map[string][]byte{path: baseContent},
			map[string][]byte{path: headContent},   // changeset = "left"
			map[string][]byte{path: targetContent},  // target = "right"
			lang,
		)
		if mergeErr != nil {
			// Merge engine error — fall back to file-level conflict
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
			// Semantic conflicts — report them with detail
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

		// Semantic merge succeeded — use merged content
		if content, ok := mergeResult.Files[path]; ok {
			semanticMerged[path] = content
		}
	}

	if len(conflicts) > 0 {
		return &CherryPickResult{Conflicts: conflicts}, nil
	}

	mergedFiles := make(map[string]string)
	for path, digest := range targetFiles {
		mergedFiles[path] = digest
	}

	for path, digest := range headFiles {
		if csModified[path] {
			mergedFiles[path] = digest
		}
	}
	for path := range baseFiles {
		if _, existsInHead := headFiles[path]; !existsInHead {
			delete(mergedFiles, path)
		}
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Write semantically merged file content and create new file nodes
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
		// Update the digest in mergedFiles so the snapshot is correct
		mergedFiles[path] = digest
	}

	autoResolved := len(semanticMerged)

	targetHex := util.BytesToHex(targetSnapshotID)
	mergedSnapPayload := map[string]interface{}{
		"sourceType":       "cherry-pick",
		"sourceRef":        fmt.Sprintf("cherry-pick:%s", util.BytesToHex(changeSetID)[:12]),
		"fileCount":        len(mergedFiles),
		"createdAt":        util.NowMs(),
		"targetSnapshot":   targetHex,
		"appliedChangeSet": util.BytesToHex(changeSetID),
	}
	if autoResolved > 0 {
		mergedSnapPayload["autoResolved"] = autoResolved
	}

	mergedSnapID, err := m.db.InsertNode(tx, graph.KindSnapshot, mergedSnapPayload)
	if err != nil {
		return nil, fmt.Errorf("inserting merged snapshot: %w", err)
	}

	headFileNodes, err := m.getSnapshotFileNodes(headID)
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
			// Use the new merged file node
			fileNode = semanticMergedNodes[path]
		} else if csModified[path] {
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

	changeSetPayload := map[string]interface{}{
		"base":            targetHex,
		"head":            util.BytesToHex(mergedSnapID),
		"title":           "",
		"description":     fmt.Sprintf("cherry-pick %s", util.BytesToHex(changeSetID)[:12]),
		"intent":          "",
		"createdAt":       util.NowMs(),
		"sourceChangeSet": util.BytesToHex(changeSetID),
	}
	newChangeSetID, err := m.db.InsertNode(tx, graph.KindChangeSet, changeSetPayload)
	if err != nil {
		return nil, fmt.Errorf("inserting changeset: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &CherryPickResult{
		ResultSnapshot:  mergedSnapID,
		ResultChangeSet: newChangeSetID,
		AppliedFiles:    len(csModified),
		AutoResolved:    autoResolved,
	}, nil
}

// normalizeMergeLang maps file extension to a language name the merge engine supports.
// Returns "" for unsupported languages.
func normalizeMergeLang(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	lang := dirio.DetectLang(path)
	switch lang {
	case "js", "ts":
		return lang
	case "python", "py":
		return "python"
	case "ruby", "rb":
		return "ruby"
	case "rust", "rs":
		return "rust"
	case "go":
		return "go"
	}
	// Fallback by extension for languages DetectLang normalizes differently
	switch ext {
	case ".js", ".jsx", ".mjs", ".cjs":
		return "js"
	case ".ts", ".tsx":
		return "ts"
	case ".py":
		return "python"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".go":
		return "go"
	}
	return ""
}
