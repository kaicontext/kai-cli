// Package graph provides the SQLite-backed node/edge graph storage.
package graph

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GCPlan describes what would be deleted by garbage collection.
type GCPlan struct {
	// Nodes that will be deleted
	NodesToDelete []*Node

	// Object digests that will be deleted from the objects directory
	ObjectsToDelete []string

	// Edges that will be deleted (count only, for summary)
	EdgesDeleted int

	// Counts by kind for summary
	SnapshotCount  int
	ChangeSetCount int
	SymbolCount    int
	ModuleCount    int
	FileCount      int

	// Total bytes that will be reclaimed
	BytesReclaimed int64
}

// GCOptions configures the garbage collector.
type GCOptions struct {
	// SinceDays only sweeps nodes older than N days (0 = no limit)
	SinceDays int

	// Aggressive also sweeps Symbols and Modules with no incoming edges
	Aggressive bool

	// DryRun computes plan without executing
	DryRun bool

	// Keep is a list of glob patterns for paths to preserve
	// Nodes referencing files matching these patterns will not be deleted
	Keep []string
}

// BuildGCPlan computes what would be deleted by garbage collection.
// It uses a mark-and-sweep algorithm:
// 1. Collect all roots (refs targets, workspace nodes)
// 2. Mark all reachable nodes from roots
// 3. Anything not marked is eligible for deletion
func (db *DB) BuildGCPlan(opts GCOptions) (*GCPlan, error) {
	plan := &GCPlan{}

	// Compute cutoff time
	var cutoffMs int64
	if opts.SinceDays > 0 {
		cutoff := time.Now().Add(-time.Duration(opts.SinceDays) * 24 * time.Hour)
		cutoffMs = cutoff.UnixMilli()
	}

	// 1. Collect roots
	roots, err := db.collectRoots()
	if err != nil {
		return nil, fmt.Errorf("collecting roots: %w", err)
	}

	// 2. Mark reachable nodes (BFS)
	marked := make(map[string]bool)
	markedDigests := make(map[string]bool)

	queue := make([][]byte, 0, len(roots))
	for id := range roots {
		queue = append(queue, []byte(id))
		marked[id] = true
	}

	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]

		// Get the node
		node, err := db.GetNode(nodeID)
		if err != nil || node == nil {
			continue
		}

		// Mark any file digests
		if node.Kind == KindFile {
			if digest, ok := node.Payload["digest"].(string); ok && digest != "" {
				markedDigests[digest] = true
			}
		}

		// Follow outgoing edges to mark reachable nodes
		edges, err := db.getAllEdgesFrom(nodeID)
		if err != nil {
			continue
		}

		for _, edge := range edges {
			dstKey := string(edge.Dst)
			if !marked[dstKey] {
				marked[dstKey] = true
				queue = append(queue, edge.Dst)
			}
		}
	}

	// 3. Find all nodes not marked
	allNodes, err := db.getAllNodes()
	if err != nil {
		return nil, fmt.Errorf("getting all nodes: %w", err)
	}

	for _, node := range allNodes {
		nodeKey := string(node.ID)
		if marked[nodeKey] {
			continue
		}

		// Check cutoff time
		if cutoffMs > 0 && node.CreatedAt > cutoffMs {
			continue // Too recent, skip
		}

		// In non-aggressive mode, skip Symbols and Modules
		if !opts.Aggressive && (node.Kind == KindSymbol || node.Kind == KindModule) {
			continue
		}

		// Check keep filters - if path matches any keep pattern, preserve node
		if len(opts.Keep) > 0 {
			if path, ok := node.Payload["path"].(string); ok && path != "" {
				shouldKeep := false
				for _, pattern := range opts.Keep {
					if matched, _ := filepath.Match(pattern, path); matched {
						shouldKeep = true
						break
					}
					// Also try glob-style matching with ** support
					if matchGlob(pattern, path) {
						shouldKeep = true
						break
					}
				}
				if shouldKeep {
					continue
				}
			}
		}

		plan.NodesToDelete = append(plan.NodesToDelete, node)

		switch node.Kind {
		case KindSnapshot:
			plan.SnapshotCount++
		case KindChangeSet:
			plan.ChangeSetCount++
		case KindSymbol:
			plan.SymbolCount++
		case KindModule:
			plan.ModuleCount++
		case KindFile:
			plan.FileCount++
			if digest, ok := node.Payload["digest"].(string); ok && digest != "" {
				if !markedDigests[digest] {
					plan.ObjectsToDelete = append(plan.ObjectsToDelete, digest)
					// Get file size
					objPath := filepath.Join(db.objectsDir, digest)
					if info, err := os.Stat(objPath); err == nil {
						plan.BytesReclaimed += info.Size()
					}
				}
			}
		}
	}

	return plan, nil
}

// ExecuteGC performs garbage collection according to the plan.
func (db *DB) ExecuteGC(plan *GCPlan) error {
	if len(plan.NodesToDelete) == 0 && len(plan.ObjectsToDelete) == 0 {
		return nil
	}

	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete edges touching deleted nodes
	for _, node := range plan.NodesToDelete {
		// Delete edges where this node is src
		_, err := tx.Exec(`DELETE FROM edges WHERE src = ?`, node.ID)
		if err != nil {
			return fmt.Errorf("deleting outgoing edges: %w", err)
		}

		// Delete edges where this node is dst
		_, err = tx.Exec(`DELETE FROM edges WHERE dst = ?`, node.ID)
		if err != nil {
			return fmt.Errorf("deleting incoming edges: %w", err)
		}

		// Delete the node
		_, err = tx.Exec(`DELETE FROM nodes WHERE id = ?`, node.ID)
		if err != nil {
			return fmt.Errorf("deleting node: %w", err)
		}

		// Delete from logs if present
		_, _ = tx.Exec(`DELETE FROM logs WHERE id = ?`, node.ID)

		// Delete from slugs if present
		_, _ = tx.Exec(`DELETE FROM slugs WHERE target_id = ?`, node.ID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	// Delete object files (outside transaction since it's filesystem)
	for _, digest := range plan.ObjectsToDelete {
		objPath := filepath.Join(db.objectsDir, digest)
		os.Remove(objPath) // Ignore errors (file might not exist)
	}

	return nil
}

// collectRoots gathers all root node IDs that should not be garbage collected.
// Roots are:
// - Named refs (snap.*, cs.*, ws.*, review.*, remote/*)
// - All workspace nodes (and their base/head snapshots)
//
// NOT roots (ephemeral movable pointers):
// - snap.working (working directory snapshot, overwritten by each capture)
// - snap.latest (convenience pointer to last committed snapshot)
func (db *DB) collectRoots() (map[string]bool, error) {
	roots := make(map[string]bool)

	// 1. All ref targets EXCEPT ephemeral movable pointers
	// snap.working and snap.latest are not roots because:
	// - snap.working is overwritten by each kai capture
	// - snap.latest is a convenience pointer, not a named root
	// Snapshots become roots when referenced by workspaces, reviews, or named refs (snap.main, etc.)
	rows, err := db.Query(`SELECT target_id FROM refs WHERE name NOT IN ('snap.working', 'snap.latest')`)
	if err != nil {
		return nil, fmt.Errorf("querying refs: %w", err)
	}
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		roots[string(id)] = true
	}
	rows.Close()

	// 2. All workspace nodes and their referenced snapshots/changesets
	workspaces, err := db.GetNodesByKind(KindWorkspace)
	if err != nil {
		return nil, fmt.Errorf("getting workspaces: %w", err)
	}

	for _, ws := range workspaces {
		roots[string(ws.ID)] = true

		// Add base and head snapshots
		if baseHex, ok := ws.Payload["baseSnapshot"].(string); ok {
			if baseID, err := hexToBytes(baseHex); err == nil {
				roots[string(baseID)] = true
			}
		}
		if headHex, ok := ws.Payload["headSnapshot"].(string); ok {
			if headID, err := hexToBytes(headHex); err == nil {
				roots[string(headID)] = true
			}
		}

		// Add open changesets
		if csArr, ok := ws.Payload["openChangeSets"].([]interface{}); ok {
			for _, csHex := range csArr {
				if hexStr, ok := csHex.(string); ok {
					if csID, err := hexToBytes(hexStr); err == nil {
						roots[string(csID)] = true
					}
				}
			}
		}
	}

	// 3. All review nodes (reviews keep their targets alive)
	reviews, err := db.GetNodesByKind(KindReview)
	if err != nil {
		return nil, fmt.Errorf("getting reviews: %w", err)
	}
	for _, review := range reviews {
		roots[string(review.ID)] = true
	}

	return roots, nil
}

// getAllNodes returns all nodes in the database.
func (db *DB) getAllNodes() ([]*Node, error) {
	rows, err := db.Query(`SELECT id, kind, payload, created_at FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*Node
	for rows.Next() {
		var id []byte
		var kind, payloadJSON string
		var createdAt int64
		if err := rows.Scan(&id, &kind, &payloadJSON, &createdAt); err != nil {
			return nil, err
		}

		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return nil, err
		}

		nodes = append(nodes, &Node{
			ID:        id,
			Kind:      NodeKind(kind),
			Payload:   payload,
			CreatedAt: createdAt,
		})
	}

	return nodes, rows.Err()
}

// getAllEdgesFrom returns all edges from a source node.
func (db *DB) getAllEdgesFrom(src []byte) ([]*Edge, error) {
	rows, err := db.Query(`SELECT type, dst, at, created_at FROM edges WHERE src = ?`, src)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []*Edge
	for rows.Next() {
		var edgeType string
		var dst, at []byte
		var createdAt int64
		if err := rows.Scan(&edgeType, &dst, &at, &createdAt); err != nil {
			return nil, err
		}

		edges = append(edges, &Edge{
			Src:       src,
			Type:      EdgeType(edgeType),
			Dst:       dst,
			At:        at,
			CreatedAt: createdAt,
		})
	}

	return edges, rows.Err()
}

// hexToBytes converts a hex string to bytes.
func hexToBytes(s string) ([]byte, error) {
	if len(s) == 0 {
		return nil, fmt.Errorf("empty hex string")
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			switch {
			case c >= '0' && c <= '9':
				v = v*16 + (c - '0')
			case c >= 'a' && c <= 'f':
				v = v*16 + (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				v = v*16 + (c - 'A' + 10)
			default:
				return nil, fmt.Errorf("invalid hex char: %c", c)
			}
		}
		b[i] = v
	}
	return b, nil
}

// matchGlob provides simple glob matching with ** support.
// ** matches any number of path segments.
func matchGlob(pattern, path string) bool {
	// Handle ** patterns by converting to prefix/suffix matching
	if pattern == "**" {
		return true
	}

	// Handle prefix patterns like "src/**"
	if len(pattern) > 3 && pattern[len(pattern)-3:] == "/**" {
		prefix := pattern[:len(pattern)-3]
		return len(path) >= len(prefix) && path[:len(prefix)] == prefix
	}

	// Handle suffix patterns like "**/*.js"
	if len(pattern) > 3 && pattern[:3] == "**/" {
		suffix := pattern[3:]
		// Match if path ends with suffix or basename matches
		if len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			return true
		}
		// Check if basename matches
		base := filepath.Base(path)
		if matched, _ := filepath.Match(suffix, base); matched {
			return true
		}
	}

	// Handle middle ** patterns like "src/**/test.js"
	for i := 0; i < len(pattern)-2; i++ {
		if pattern[i:i+3] == "/**" && i+3 < len(pattern) && pattern[i+3] == '/' {
			prefix := pattern[:i]
			suffix := pattern[i+4:]
			if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
				// Check if any suffix of the remaining path matches the suffix pattern
				remaining := path[len(prefix):]
				for j := 0; j < len(remaining); j++ {
					if remaining[j] == '/' {
						subpath := remaining[j+1:]
						if matched, _ := filepath.Match(suffix, subpath); matched {
							return true
						}
						if matchGlob(suffix, subpath) {
							return true
						}
					}
				}
			}
		}
	}

	return false
}

// PurgePlan describes what would be removed by a file-level purge.
type PurgePlan struct {
	// File nodes to delete
	FileNodes []*Node

	// Paths matched (for display)
	Paths []string

	// Symbol nodes to delete (defined in purged files)
	SymbolNodes []*Node

	// Object digests to delete from disk
	ObjectsToDelete []string

	// Snapshots that need payload updates (ID -> updated payload)
	SnapshotUpdates map[string]map[string]interface{}

	// Counts for summary
	FileCount     int
	SymbolCount   int
	SnapshotCount int
	BytesReclaimed int64
}

// BuildPurgePlan computes what would be removed by purging files matching the given patterns.
func (db *DB) BuildPurgePlan(patterns []string) (*PurgePlan, error) {
	plan := &PurgePlan{
		SnapshotUpdates: make(map[string]map[string]interface{}),
	}

	// Collect all matching File nodes
	seenDigests := make(map[string]bool)
	seenFiles := make(map[string]bool)

	allFileNodes, err := db.GetNodesByKind(KindFile)
	if err != nil {
		return nil, fmt.Errorf("getting file nodes: %w", err)
	}

	for _, node := range allFileNodes {
		path, _ := node.Payload["path"].(string)
		if path == "" {
			continue
		}

		if !matchesAnyPattern(patterns, path) {
			continue
		}

		nodeKey := string(node.ID)
		if seenFiles[nodeKey] {
			continue
		}
		seenFiles[nodeKey] = true

		plan.FileNodes = append(plan.FileNodes, node)
		plan.Paths = append(plan.Paths, path)
		plan.FileCount++

		// Collect object digest for deletion
		if digest, ok := node.Payload["digest"].(string); ok && digest != "" {
			if !seenDigests[digest] {
				seenDigests[digest] = true
				plan.ObjectsToDelete = append(plan.ObjectsToDelete, digest)
				objPath := filepath.Join(db.objectsDir, digest)
				if info, err := os.Stat(objPath); err == nil {
					plan.BytesReclaimed += info.Size()
				}
			}
		}
		// Also check contentDigest
		if digest, ok := node.Payload["contentDigest"].(string); ok && digest != "" {
			if !seenDigests[digest] {
				seenDigests[digest] = true
				plan.ObjectsToDelete = append(plan.ObjectsToDelete, digest)
				objPath := filepath.Join(db.objectsDir, digest)
				if info, err := os.Stat(objPath); err == nil {
					plan.BytesReclaimed += info.Size()
				}
			}
		}
	}

	if plan.FileCount == 0 {
		return plan, nil
	}

	// Find symbols defined in purged files (DEFINES_IN edges: symbol -> file)
	for _, fileNode := range plan.FileNodes {
		edges, err := db.GetEdgesByDst(EdgeDefinesIn, fileNode.ID)
		if err != nil {
			continue
		}
		for _, edge := range edges {
			symNode, err := db.GetNode(edge.Src)
			if err != nil || symNode == nil {
				continue
			}
			plan.SymbolNodes = append(plan.SymbolNodes, symNode)
			plan.SymbolCount++
		}
	}

	// Find snapshots referencing purged files and build updated payloads
	fileIDSet := make(map[string]bool)
	for _, fn := range plan.FileNodes {
		fileIDSet[hex.EncodeToString(fn.ID)] = true
	}

	pathSet := make(map[string]bool)
	for _, p := range plan.Paths {
		pathSet[p] = true
	}

	allSnapshots, err := db.GetNodesByKind(KindSnapshot)
	if err != nil {
		return nil, fmt.Errorf("getting snapshots: %w", err)
	}

	for _, snap := range allSnapshots {
		snapHex := hex.EncodeToString(snap.ID)
		needsUpdate := false

		// Check fileDigests array
		if digests, ok := snap.Payload["fileDigests"].([]interface{}); ok {
			var filtered []interface{}
			for _, d := range digests {
				dStr, _ := d.(string)
				if fileIDSet[dStr] {
					needsUpdate = true
					continue
				}
				filtered = append(filtered, d)
			}
			if needsUpdate {
				newPayload := copyPayload(snap.Payload)
				newPayload["fileDigests"] = filtered

				// Also filter the files metadata array
				if files, ok := newPayload["files"].([]interface{}); ok {
					var filteredFiles []interface{}
					for _, f := range files {
						fm, ok := f.(map[string]interface{})
						if !ok {
							continue
						}
						if p, ok := fm["path"].(string); ok && pathSet[p] {
							continue
						}
						filteredFiles = append(filteredFiles, f)
					}
					newPayload["files"] = filteredFiles
				}

				// Update fileCount
				if fc, ok := newPayload["fileCount"].(float64); ok {
					remaining := int(fc) - (len(digests) - len(filtered))
					if remaining < 0 {
						remaining = 0
					}
					newPayload["fileCount"] = remaining
				}

				plan.SnapshotUpdates[snapHex] = newPayload
				plan.SnapshotCount++
			}
		}
	}

	return plan, nil
}

// ExecutePurge performs the file-level purge according to the plan.
func (db *DB) ExecutePurge(plan *PurgePlan) error {
	if plan.FileCount == 0 {
		return nil
	}

	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// 1. Delete symbol nodes and their edges
	for _, sym := range plan.SymbolNodes {
		tx.Exec(`DELETE FROM edges WHERE src = ?`, sym.ID)
		tx.Exec(`DELETE FROM edges WHERE dst = ?`, sym.ID)
		tx.Exec(`DELETE FROM nodes WHERE id = ?`, sym.ID)
		tx.Exec(`DELETE FROM slugs WHERE target_id = ?`, sym.ID)
	}

	// 2. Delete file nodes and all their edges
	for _, fileNode := range plan.FileNodes {
		path, _ := fileNode.Payload["path"].(string)

		tx.Exec(`DELETE FROM edges WHERE src = ?`, fileNode.ID)
		tx.Exec(`DELETE FROM edges WHERE dst = ?`, fileNode.ID)
		tx.Exec(`DELETE FROM nodes WHERE id = ?`, fileNode.ID)
		tx.Exec(`DELETE FROM slugs WHERE target_id = ?`, fileNode.ID)
		tx.Exec(`DELETE FROM logs WHERE id = ?`, fileNode.ID)

		// Delete authorship ranges for this file across all snapshots
		if path != "" {
			tx.Exec(`DELETE FROM authorship_ranges WHERE file_path = ?`, path)
		}
	}

	// 3. Snapshot manifests are content-addressed (id = blake3(kind+payload)),
	// so they MUST NOT be rewritten in place to drop the purged file entries.
	// Doing so was the root cause of the snapshot-corruption incident: it
	// broke id == blake3(payload), producing headless snapshots that skip on
	// push and 404 CI checkout. We delete the file's blob content below
	// (step 4) — which achieves the space-reclaim / secret-scrub goal — but
	// leave historical manifests intact so their digests stay valid. Actually
	// removing a file's path/digest from old manifests requires re-minting
	// those snapshots (new ids + repointing every ref/changeset/edge); that
	// is intentionally NOT attempted here. plan.SnapshotUpdates is retained
	// for callers/inspection but no longer applied.
	_ = plan.SnapshotUpdates

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing purge: %w", err)
	}

	// 4. Delete object files from disk (outside transaction)
	for _, digest := range plan.ObjectsToDelete {
		objPath := filepath.Join(db.objectsDir, digest)
		os.Remove(objPath)
	}

	return nil
}

// matchesAnyPattern checks if a path matches any of the given patterns.
func matchesAnyPattern(patterns []string, path string) bool {
	for _, pattern := range patterns {
		// Exact match
		if pattern == path {
			return true
		}
		// Standard glob
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
		// Extended glob with ** support
		if matchGlob(pattern, path) {
			return true
		}
	}
	return false
}

// copyPayload makes a shallow copy of a payload map.
func copyPayload(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
