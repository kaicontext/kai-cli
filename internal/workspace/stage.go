// Package workspace provides staging operations for workspaces.
package workspace

import (
	"fmt"

	"kai/internal/classify"
	"kai/internal/filesource"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/module"
	"kai/internal/signing"
	"kai/internal/snapshot"
	"kai/internal/util"
)

// StageResult contains the result of staging changes.
type StageResult struct {
	ChangeSetID  []byte
	HeadSnapshot []byte
	ChangedFiles int
	ChangeTypes  int
	Conflicts    []Conflict
}

// Conflict represents a merge conflict.
type Conflict struct {
	Path        string
	Description string
	BaseDigest  string
	HeadDigest  string
	NewDigest   string
}

// StageOptions controls staging behavior.
type StageOptions struct {
	SignKeyPath string
}

// Stage stages changes from a file source into a workspace. It snapshots
// the source, analyzes it, then records the diff against the workspace
// head via StageSnapshot.
func (m *Manager) Stage(nameOrID string, source filesource.FileSource, matcher *module.Matcher, message string, opts *StageOptions) (*StageResult, error) {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}
	if ws.Status != StatusActive {
		return nil, fmt.Errorf("workspace is %s, must be active to stage", ws.Status)
	}

	// Create a new snapshot from the source
	creator := snapshot.NewCreator(m.db, matcher)
	newSnapID, err := creator.CreateSnapshot(source)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot from source: %w", err)
	}

	// Auto-analyze for better intent generation
	creator.Analyze(newSnapID, nil) // Non-fatal if fails

	return m.StageSnapshot(nameOrID, newSnapID, matcher, message, opts)
}

// StageSnapshot stages an already-created (and ideally already-analyzed)
// snapshot into a workspace: it diffs the snapshot against the workspace
// head, records a changeset, and advances the workspace head. Callers
// that have already built and analyzed a snapshot — notably `kai capture`
// when a workspace is checked out — use this to avoid snapshotting the
// working tree a second time.
func (m *Manager) StageSnapshot(nameOrID string, newSnapID []byte, matcher *module.Matcher, message string, opts *StageOptions) (*StageResult, error) {
	ws, err := m.Get(nameOrID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace not found: %s", nameOrID)
	}
	if ws.Status != StatusActive {
		return nil, fmt.Errorf("workspace is %s, must be active to stage", ws.Status)
	}

	creator := snapshot.NewCreator(m.db, matcher)

	// Add description to snapshot if message provided
	if message != "" {
		node, err := m.db.GetNode(newSnapID)
		if err == nil && node != nil {
			node.Payload["description"] = message
			m.db.UpdateNodePayload(newSnapID, node.Payload)
		}
	}

	// Get files from head snapshot and new snapshot
	headFiles, err := creator.GetSnapshotFiles(ws.HeadSnapshot)
	if err != nil {
		return nil, fmt.Errorf("getting head files: %w", err)
	}

	newFiles, err := creator.GetSnapshotFiles(newSnapID)
	if err != nil {
		return nil, fmt.Errorf("getting new files: %w", err)
	}

	// Build file maps
	headFileMap := make(map[string]*graph.Node)
	newFileMap := make(map[string]*graph.Node)

	for _, f := range headFiles {
		if path, ok := f.Payload["path"].(string); ok {
			headFileMap[path] = f
		}
	}

	for _, f := range newFiles {
		if path, ok := f.Payload["path"].(string); ok {
			newFileMap[path] = f
		}
	}

	// Find changed files
	var changedPaths []string
	var changedFileIDs [][]byte

	for path, newFile := range newFileMap {
		headFile, exists := headFileMap[path]
		if !exists {
			// Added file
			changedPaths = append(changedPaths, path)
			changedFileIDs = append(changedFileIDs, newFile.ID)
		} else {
			// Check if digest differs
			headDigest, _ := headFile.Payload["digest"].(string)
			newDigest, _ := newFile.Payload["digest"].(string)
			if headDigest != newDigest {
				changedPaths = append(changedPaths, path)
				changedFileIDs = append(changedFileIDs, newFile.ID)
			}
		}
	}

	// Check for deleted files
	for path := range headFileMap {
		if _, exists := newFileMap[path]; !exists {
			changedPaths = append(changedPaths, path)
		}
	}

	if len(changedPaths) == 0 {
		return &StageResult{
			ChangeSetID:  nil,
			HeadSnapshot: ws.HeadSnapshot,
			ChangedFiles: 0,
			ChangeTypes:  0,
		}, nil
	}

	// Start transaction
	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Create changeset node
	changeSetPayload := map[string]interface{}{
		"base":        util.BytesToHex(ws.HeadSnapshot),
		"head":        util.BytesToHex(newSnapID),
		"title":       "",
		"description": message,
		"intent":      "",
		"workspaceId": util.BytesToHex(ws.ID),
		"createdAt":   util.NowMs(),
	}
	if opts != nil && opts.SignKeyPath != "" {
		sigFields, err := signing.SignChangeSetPayloadSSH(changeSetPayload, opts.SignKeyPath)
		if err != nil {
			return nil, fmt.Errorf("signing changeset: %w", err)
		}
		for k, v := range sigFields {
			changeSetPayload[k] = v
		}
	}
	changeSetID, err := m.db.InsertNode(tx, graph.KindChangeSet, changeSetPayload)
	if err != nil {
		return nil, fmt.Errorf("inserting changeset: %w", err)
	}

	// Detect change types
	detector := classify.NewDetector()
	var allChangeTypes []*classify.ChangeType
	var affectedModules []string
	affectedModulesSet := make(map[string]bool)

	// Load symbols for each file to enable symbol mapping in change detection
	for _, f := range newFiles {
		fileIDHex := util.BytesToHex(f.ID)
		symbols, err := creator.GetSymbolsInFile(f.ID, newSnapID)
		if err == nil && len(symbols) > 0 {
			detector.SetSymbols(fileIDHex, symbols)
		}
	}

	for i, path := range changedPaths {
		newFile := newFileMap[path]
		headFile := headFileMap[path]

		var beforeContent, afterContent []byte

		// Read after content
		if newFile != nil {
			if digest, ok := newFile.Payload["digest"].(string); ok {
				afterContent, _ = m.db.ReadObject(digest)
			}
		}

		// Read before content
		if headFile != nil {
			if digest, ok := headFile.Payload["digest"].(string); ok {
				beforeContent, _ = m.db.ReadObject(digest)
			}
		}

		// Get the file's language
		var lang string
		if newFile != nil {
			lang, _ = newFile.Payload["lang"].(string)
		}

		if len(beforeContent) > 0 && len(afterContent) > 0 && i < len(changedFileIDs) {
			var changes []*classify.ChangeType
			var err error

			switch lang {
			case "json":
				// Use JSON-specific detection
				changes, err = classify.DetectJSONChanges(path, beforeContent, afterContent)
			case "yaml":
				// Use YAML-specific detection
				changes, err = classify.DetectYAMLChanges(path, beforeContent, afterContent)
			case "ts", "js":
				// Use tree-sitter based detection for JavaScript/TypeScript
				changes, err = detector.DetectChanges(path, beforeContent, afterContent, util.BytesToHex(changedFileIDs[i]), lang)
			case "py":
				// Use tree-sitter based detection for Python
				changes, err = detector.DetectChanges(path, beforeContent, afterContent, util.BytesToHex(changedFileIDs[i]), "py")
			default:
				// Non-parseable files get FILE_CONTENT_CHANGED
				changes = []*classify.ChangeType{classify.NewFileChange(classify.FileContentChanged, path)}
			}

			if err == nil && len(changes) > 0 {
				allChangeTypes = append(allChangeTypes, changes...)
			}
		} else if headFile == nil && len(afterContent) > 0 {
			// New file added
			allChangeTypes = append(allChangeTypes, classify.NewFileChange(classify.FileAdded, path))
		}

		// Create MODIFIES edge to file
		if i < len(changedFileIDs) {
			if err := m.db.InsertEdge(tx, changeSetID, graph.EdgeModifies, changedFileIDs[i], nil); err != nil {
				return nil, fmt.Errorf("inserting MODIFIES edge: %w", err)
			}
		}

		// Map to modules
		if matcher != nil {
			modules := matcher.MatchPath(path)
			for _, mod := range modules {
				if !affectedModulesSet[mod] {
					affectedModulesSet[mod] = true
					affectedModules = append(affectedModules, mod)
				}
			}
		}
	}

	// Create ChangeType nodes and HAS edges
	for _, ct := range allChangeTypes {
		payload := classify.GetCategoryPayload(ct)
		ctID, err := m.db.InsertNode(tx, graph.KindChangeType, payload)
		if err != nil {
			return nil, fmt.Errorf("inserting change type: %w", err)
		}
		if err := m.db.InsertEdge(tx, changeSetID, graph.EdgeHas, ctID, nil); err != nil {
			return nil, fmt.Errorf("inserting HAS edge: %w", err)
		}
	}

	// Create AFFECTS edges to modules
	if matcher != nil {
		for _, modName := range affectedModules {
			payload := matcher.GetModulePayload(modName)
			if payload != nil {
				modID, err := m.db.InsertNode(tx, graph.KindModule, payload)
				if err != nil {
					return nil, fmt.Errorf("inserting module: %w", err)
				}
				if err := m.db.InsertEdge(tx, changeSetID, graph.EdgeAffects, modID, nil); err != nil {
					return nil, fmt.Errorf("inserting AFFECTS edge: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	// Update workspace head and add changeset
	if err := m.UpdateHead(ws.ID, newSnapID); err != nil {
		return nil, fmt.Errorf("updating workspace head: %w", err)
	}
	if err := m.AddChangeSet(ws.ID, changeSetID); err != nil {
		return nil, fmt.Errorf("adding changeset to workspace: %w", err)
	}

	return &StageResult{
		ChangeSetID:  changeSetID,
		HeadSnapshot: newSnapID,
		ChangedFiles: len(changedPaths),
		ChangeTypes:  len(allChangeTypes),
	}, nil
}
