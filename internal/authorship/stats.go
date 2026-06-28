package authorship

import (
	"fmt"

	"github.com/kaicontext/kai-engine/graph"
)

// ProjectStats computes project-wide AI vs human authorship statistics.
func ProjectStats(db *graph.DB, snapshotID []byte) (*ProjectSummary, error) {
	ranges, err := db.GetAllAuthorshipRanges(snapshotID)
	if err != nil {
		return nil, err
	}

	summary := &ProjectSummary{
		ByAgent: make(map[string]int),
	}

	for _, r := range ranges {
		lines := r.EndLine - r.StartLine + 1
		summary.TotalLines += lines
		if r.AuthorType == "ai" {
			summary.AILines += lines
			agent := r.Agent
			if agent == "" {
				agent = "unknown-ai"
			}
			summary.ByAgent[agent] += lines
		} else {
			summary.HumanLines += lines
		}
	}

	if summary.TotalLines > 0 {
		summary.AIPct = float64(summary.AILines) / float64(summary.TotalLines) * 100
	}

	edgeCount, err := db.EdgeCount()
	if err != nil {
		return nil, fmt.Errorf("counting edges: %w", err)
	}
	summary.EdgesCount = edgeCount

	return summary, nil
}
