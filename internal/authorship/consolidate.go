package authorship

import (
	"fmt"
	"time"

	"github.com/kaicontext/kai-engine/graph"

	"github.com/kaicontext/kai-core/cas"
)

// Consolidate reads pending checkpoints and writes authorship ranges to the DB.
// Called as Step 4 of kai capture.
//
// If previousSnapshotID is non-nil, carries authorship history forward from
// the previous snapshot: files untouched in this capture keep their ranges as-is,
// and for files that were edited, old ranges outside the edited region are
// copied forward so the attribution "memory" survives across captures.
func Consolidate(db *graph.DB, snapshotID, previousSnapshotID []byte, kaiDir string) error {
	checkpoints, err := ReadPendingCheckpoints(kaiDir)
	if err != nil {
		return fmt.Errorf("reading checkpoints: %w", err)
	}
	// We still want to forward-port previous authorship even if there are no
	// new checkpoints — otherwise every capture zeroes out history.
	if len(checkpoints) == 0 && previousSnapshotID == nil {
		return nil
	}

	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	now := cas.NowMs()

	// Group checkpoints by file and merge overlapping ranges
	byFile := make(map[string][]CheckpointRecord)
	for _, cp := range checkpoints {
		byFile[cp.File] = append(byFile[cp.File], cp)
	}

	// --- Forward-port from previous snapshot ---
	//
	// For every file that had authorship in the previous snapshot:
	//   (a) if it is NOT being modified in this capture, copy its ranges
	//       verbatim — line numbers still match.
	//   (b) if it IS being modified, copy only the ranges that don't
	//       intersect the lines being (re)attributed by this capture.
	//       The new checkpoint ranges will cover the overlapping lines.
	//
	// This is deliberately simple: we don't try to shift line numbers when
	// insertions/deletions happen. INSERT OR REPLACE lets new ranges win on
	// exact-start-line collisions; callers of `kai blame` should treat the
	// ranges as best-effort attribution, not surveillance-grade truth.
	if previousSnapshotID != nil {
		prevRanges, err := db.GetAllAuthorshipRanges(previousSnapshotID)
		if err != nil {
			return fmt.Errorf("reading previous authorship: %w", err)
		}
		for _, pr := range prevRanges {
			// Skip ranges that overlap the current capture's re-attributed lines.
			if overlapsAny(pr, byFile[pr.FilePath]) {
				continue
			}
			if err := db.InsertAuthorshipRange(tx, snapshotID, pr); err != nil {
				return fmt.Errorf("forward-porting authorship range: %w", err)
			}
		}
	}

	// --- Apply new checkpoints on top ---
	for filePath, cps := range byFile {
		// Merge overlapping/adjacent ranges from the same agent
		merged := mergeRanges(cps)
		for _, r := range merged {
			ar := graph.AuthorshipRange{
				FilePath:   filePath,
				StartLine:  r.StartLine,
				EndLine:    r.EndLine,
				AuthorType: r.AuthorType,
				Agent:      r.Agent,
				Model:      r.Model,
				SessionID:  r.SessionID,
				CreatedAt:  now,
			}
			if err := db.InsertAuthorshipRange(tx, snapshotID, ar); err != nil {
				return fmt.Errorf("inserting authorship range: %w", err)
			}
		}
	}

	// Create an AuthorshipLog node linked to the snapshot
	totalRanges := 0
	for _, cps := range byFile {
		totalRanges += len(mergeRanges(cps))
	}

	agents := collectAgents(checkpoints)
	logPayload := map[string]interface{}{
		"snapshotId":    fmt.Sprintf("%x", snapshotID),
		"checkpoints":   len(checkpoints),
		"files":         len(byFile),
		"ranges":        totalRanges,
		"agents":        agents,
		"consolidatedAt": time.Now().UTC().Format(time.RFC3339),
	}
	logID, err := db.InsertNode(tx, graph.KindAuthorshipLog, logPayload)
	if err != nil {
		return fmt.Errorf("inserting authorship log node: %w", err)
	}

	// Link: Snapshot -> AuthorshipLog
	if err := db.InsertEdge(tx, snapshotID, graph.EdgeAttributedIn, logID, nil); err != nil {
		return fmt.Errorf("inserting ATTRIBUTED_IN edge: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing authorship data: %w", err)
	}

	// Clean up processed checkpoints
	if err := ClearProcessedCheckpoints(kaiDir); err != nil {
		return fmt.Errorf("clearing checkpoints: %w", err)
	}

	return nil
}

// mergeRanges merges overlapping or adjacent checkpoint ranges for the same file.
// Later checkpoints (by timestamp) win when ranges overlap.
func mergeRanges(cps []CheckpointRecord) []CheckpointRecord {
	if len(cps) == 0 {
		return nil
	}
	if len(cps) == 1 {
		return cps
	}

	// Build a line-level map: line -> latest checkpoint info
	lineMap := make(map[int]*CheckpointRecord)
	for i := range cps {
		cp := &cps[i]
		for line := cp.StartLine; line <= cp.EndLine; line++ {
			existing, ok := lineMap[line]
			if !ok || cp.Timestamp >= existing.Timestamp {
				lineMap[line] = cp
			}
		}
	}

	// Collect all lines and sort them
	lines := make([]int, 0, len(lineMap))
	for line := range lineMap {
		lines = append(lines, line)
	}
	sortInts(lines)

	// Merge consecutive lines with the same attribution into ranges
	var merged []CheckpointRecord
	i := 0
	for i < len(lines) {
		start := lines[i]
		cp := lineMap[start]
		end := start

		// Extend range while consecutive lines have same attribution
		for i+1 < len(lines) && lines[i+1] == lines[i]+1 && sameAttribution(lineMap[lines[i+1]], cp) {
			i++
			end = lines[i]
		}

		merged = append(merged, CheckpointRecord{
			File:       cp.File,
			StartLine:  start,
			EndLine:    end,
			AuthorType: cp.AuthorType,
			Agent:      cp.Agent,
			Model:      cp.Model,
			SessionID:  cp.SessionID,
			Timestamp:  cp.Timestamp,
		})
		i++
	}

	return merged
}

func sameAttribution(a, b *CheckpointRecord) bool {
	return a.AuthorType == b.AuthorType && a.Agent == b.Agent && a.Model == b.Model
}

// overlapsAny reports whether `pr` (a previous authorship range) overlaps the
// line span of any checkpoint in `cps` on the same file. Used during
// forward-port to skip old ranges that the current capture is rewriting.
func overlapsAny(pr graph.AuthorshipRange, cps []CheckpointRecord) bool {
	for _, c := range cps {
		if c.File != pr.FilePath {
			continue
		}
		if c.StartLine <= pr.EndLine && pr.StartLine <= c.EndLine {
			return true
		}
	}
	return false
}

func collectAgents(cps []CheckpointRecord) []string {
	seen := make(map[string]bool)
	var agents []string
	for _, cp := range cps {
		if cp.Agent != "" && !seen[cp.Agent] {
			seen[cp.Agent] = true
			agents = append(agents, cp.Agent)
		}
	}
	return agents
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
