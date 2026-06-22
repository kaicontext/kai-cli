package authorship

import (
	"kai/internal/graph"
)

// Blame returns authorship ranges for a file in the given snapshot.
func Blame(db *graph.DB, snapshotID []byte, filePath string) ([]graph.AuthorshipRange, error) {
	return db.GetAuthorshipRanges(snapshotID, filePath)
}

// BlameFileSummary returns a summary of AI vs human attribution for a file.
func BlameFileSummary(db *graph.DB, snapshotID []byte, filePath string) (*FileSummary, error) {
	ranges, err := db.GetAuthorshipRanges(snapshotID, filePath)
	if err != nil {
		return nil, err
	}

	summary := &FileSummary{File: filePath}
	agentSet := make(map[string]bool)

	for _, r := range ranges {
		lines := r.EndLine - r.StartLine + 1
		summary.TotalLines += lines
		if r.AuthorType == "ai" {
			summary.AILines += lines
			if r.Agent != "" {
				agentSet[r.Agent] = true
			}
		} else {
			summary.HumanLines += lines
		}
	}

	if summary.TotalLines > 0 {
		summary.AIPct = float64(summary.AILines) / float64(summary.TotalLines) * 100
	}

	for agent := range agentSet {
		summary.Agents = append(summary.Agents, agent)
	}

	return summary, nil
}
