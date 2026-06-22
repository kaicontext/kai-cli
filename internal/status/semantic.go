package status

import (
	"fmt"
	"os"
	"path/filepath"

	"kai/internal/classify"
	"kai/internal/graph"
	"kai/internal/snapshot"
)

// SemanticResult contains semantic analysis of changes.
type SemanticResult struct {
	ChangeTypes    []*classify.ChangeType
	CategoryCounts map[classify.ChangeCategory]int
	AffectedFiles  int
}

// SemanticOptions configures semantic analysis.
type SemanticOptions struct {
	Dir string // Working directory
}

// AnalyzeSemantic performs semantic analysis on modified files.
func AnalyzeSemantic(db *graph.DB, statusResult *Result, opts SemanticOptions) (*SemanticResult, error) {
	if len(statusResult.Modified) == 0 {
		return &SemanticResult{
			ChangeTypes:    []*classify.ChangeType{},
			CategoryCounts: make(map[classify.ChangeCategory]int),
			AffectedFiles:  0,
		}, nil
	}

	// Get baseline files content from snapshot
	snapshotFiles, err := snapshot.NewCreator(db, nil).GetSnapshotFiles(statusResult.BaselineID)
	if err != nil {
		return nil, fmt.Errorf("getting snapshot files: %w", err)
	}

	// Build map of baseline file content
	baselineContent := make(map[string][]byte)
	for _, f := range snapshotFiles {
		path, _ := f.Payload["path"].(string)
		// Load content from object store
		digest, ok := f.Payload["digest"].(string)
		if ok && digest != "" {
			content, err := db.ReadObject(digest)
			if err == nil && content != nil {
				baselineContent[path] = content
			}
		}
	}

	detector := classify.NewDetector()
	result := &SemanticResult{
		ChangeTypes:    []*classify.ChangeType{},
		CategoryCounts: make(map[classify.ChangeCategory]int),
		AffectedFiles:  0,
	}

	dir := opts.Dir
	if dir == "" {
		dir = "."
	}

	// Analyze each modified file
	for _, path := range statusResult.Modified {
		beforeContent, ok := baselineContent[path]
		if !ok {
			continue
		}

		// Read current content
		fullPath := filepath.Join(dir, path)
		afterContent, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		// Detect changes
		changes, err := detector.DetectChanges(path, beforeContent, afterContent, "")
		if err != nil {
			continue
		}

		if len(changes) > 0 {
			result.AffectedFiles++
			result.ChangeTypes = append(result.ChangeTypes, changes...)
			for _, ct := range changes {
				result.CategoryCounts[ct.Category]++
			}
		}
	}

	return result, nil
}

// FormatSemanticSummary formats a brief summary of semantic changes.
func FormatSemanticSummary(result *SemanticResult) string {
	if len(result.ChangeTypes) == 0 {
		return "No semantic changes detected"
	}

	summary := fmt.Sprintf("Semantic analysis: %d change(s) in %d file(s)\n",
		len(result.ChangeTypes), result.AffectedFiles)

	for cat, count := range result.CategoryCounts {
		summary += fmt.Sprintf("  %s: %d\n", cat, count)
	}

	return summary
}
