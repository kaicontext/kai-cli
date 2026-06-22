// Package authorship tracks AI vs human code attribution.
// It records checkpoints as AI agents edit files, consolidates them
// on capture, and provides blame/stats queries.
package authorship

// CheckpointRecord represents a single edit event recorded by the MCP server.
type CheckpointRecord struct {
	Version    int    `json:"v"`
	File       string `json:"file"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Action     string `json:"action"`      // insert, modify, delete, conflict
	AuthorType string `json:"author_type"` // ai, human
	Agent      string `json:"agent"`       // e.g. "claude-code", "cursor"
	Model      string `json:"model"`       // e.g. "claude-opus-4-6"
	SessionID  string `json:"session_id"`
	Timestamp  int64  `json:"ts"`
	// PeerOrigin is true when this checkpoint was written from a live-sync
	// receive, i.e. the author is a remote peer and we only observed the edit
	// as incoming file content. Distinguishes peer-attributed lines from
	// locally-observed tool-call edits.
	PeerOrigin bool `json:"peer_origin,omitempty"`
}

// FileSummary holds attribution stats for a single file.
type FileSummary struct {
	File       string   `json:"file"`
	TotalLines int      `json:"total_lines"`
	AILines    int      `json:"ai_lines"`
	HumanLines int      `json:"human_lines"`
	AIPct      float64  `json:"ai_pct"`
	Agents     []string `json:"agents,omitempty"`
}

// ProjectSummary holds attribution stats for the entire project.
type ProjectSummary struct {
	TotalLines int            `json:"total_lines"`
	AILines    int            `json:"ai_lines"`
	HumanLines int            `json:"human_lines"`
	AIPct      float64        `json:"ai_pct"`
	EdgesCount int            `json:"edge_count"`
	ByAgent    map[string]int `json:"by_agent"` // agent name -> line count
}
