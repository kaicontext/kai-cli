// Package diff provides semantic diff types and operations.
package diff

import (
	"kai/internal/graph"
	"kai/internal/util"
)

// FromChangeSet extracts a SemanticDiff from changeset graph data.
// This bridges the graph storage format to our diff types.
func FromChangeSet(db *graph.DB, csNode *graph.Node) (*SemanticDiff, error) {
	if csNode == nil || csNode.Kind != graph.KindChangeSet {
		return nil, nil
	}

	baseHex, _ := csNode.Payload["base"].(string)
	headHex, _ := csNode.Payload["head"].(string)

	sd := &SemanticDiff{
		Base: baseHex,
		Head: headHex,
	}

	// Get all nodes and edges for this changeset
	csData, err := db.GetAllNodesAndEdgesForChangeSet(csNode.ID)
	if err != nil {
		return sd, nil // Return partial result on error
	}

	// Map of file path to FileDiff
	fileMap := make(map[string]*FileDiff)

	nodes, _ := csData["nodes"].([]map[string]interface{})
	for _, node := range nodes {
		kind, _ := node["kind"].(string)
		payload, _ := node["payload"].(map[string]interface{})

		switch kind {
		case "File":
			path, _ := payload["path"].(string)
			digest, _ := payload["digest"].(string)
			action := determineFileAction(payload)

			if path != "" {
				fileMap[path] = &FileDiff{
					Path:      path,
					Action:    action,
					AfterHash: digest,
				}
			}

		case "Symbol":
			fqName, _ := payload["fqName"].(string)
			symKind, _ := payload["kind"].(string)
			sig, _ := payload["signature"].(string)
			beforeSig, _ := payload["beforeSignature"].(string)
			changeType, _ := payload["changeType"].(string)
			filePath, _ := payload["file"].(string)

			// Determine action from changeType or presence of before/after
			action := ActionModified
			if changeType == "ADDED" || beforeSig == "" && sig != "" {
				action = ActionAdded
			} else if changeType == "REMOVED" || beforeSig != "" && sig == "" {
				action = ActionRemoved
			}

			unit := UnitDiff{
				Kind:       UnitKind(symKind),
				Name:       extractName(fqName),
				FQName:     fqName,
				Action:     action,
				BeforeSig:  beforeSig,
				AfterSig:   sig,
				ChangeType: changeType,
			}

			// Add to appropriate file
			if filePath != "" {
				if _, exists := fileMap[filePath]; !exists {
					fileMap[filePath] = &FileDiff{Path: filePath, Action: ActionModified}
				}
				fileMap[filePath].Units = append(fileMap[filePath].Units, unit)
			}
		}
	}

	// Convert map to slice
	for _, f := range fileMap {
		sd.Files = append(sd.Files, *f)
	}

	return sd, nil
}

// FromChangeSetID loads a changeset by ID and extracts its SemanticDiff.
func FromChangeSetID(db *graph.DB, csID []byte) (*SemanticDiff, error) {
	csNode, err := db.GetNode(csID)
	if err != nil {
		return nil, err
	}
	return FromChangeSet(db, csNode)
}

// FromReview extracts a SemanticDiff from a review's target changeset.
func FromReview(db *graph.DB, targetID []byte, targetKind graph.NodeKind) (*SemanticDiff, error) {
	if targetKind != graph.KindChangeSet {
		return nil, nil // Only changesets have diffs
	}
	return FromChangeSetID(db, targetID)
}

func determineFileAction(payload map[string]interface{}) Action {
	// Check for explicit action field
	if actionStr, ok := payload["action"].(string); ok {
		switch actionStr {
		case "added":
			return ActionAdded
		case "removed":
			return ActionRemoved
		}
	}
	// Check for digest presence (if no before digest but has after, it's added)
	beforeDigest, hasBefore := payload["beforeDigest"].(string)
	afterDigest, hasAfter := payload["digest"].(string)

	if (!hasBefore || beforeDigest == "") && hasAfter && afterDigest != "" {
		return ActionAdded
	}
	if hasBefore && beforeDigest != "" && (!hasAfter || afterDigest == "") {
		return ActionRemoved
	}
	return ActionModified
}

func extractName(fqName string) string {
	// Extract the short name from "pkg.Class.method"
	if fqName == "" {
		return ""
	}
	// Find the last dot
	for i := len(fqName) - 1; i >= 0; i-- {
		if fqName[i] == '.' {
			return fqName[i+1:]
		}
	}
	return fqName
}

// ensure util import is used
var _ = util.BytesToHex
