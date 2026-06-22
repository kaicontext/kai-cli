package authorship

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"kai/internal/graph"
)

// GitNotePayload is the JSON structure written to git notes.
// Designed for interoperability with the Git AI standard.
type GitNotePayload struct {
	Version   int                       `json:"version"`
	Tool      string                    `json:"tool"`
	Snapshot  string                    `json:"snapshot_id"`
	Files     map[string][]NoteRange    `json:"files"`
	Summary   NoteSummary               `json:"summary"`
}

// NoteRange is a line range with attribution in the git note.
type NoteRange struct {
	Start      int    `json:"start"`
	End        int    `json:"end"`
	AuthorType string `json:"author_type"`
	Agent      string `json:"agent,omitempty"`
	Model      string `json:"model,omitempty"`
}

// NoteSummary holds aggregate counts for the git note.
type NoteSummary struct {
	TotalLines int `json:"total_lines"`
	AILines    int `json:"ai_lines"`
	HumanLines int `json:"human_lines"`
}

const gitNotesRef = "refs/notes/kai-authorship"

// WriteGitNote writes authorship data as a git note on the given commit.
func WriteGitNote(repoPath string, commitHash string, snapshotHex string, ranges []graph.AuthorshipRange) error {
	// Group ranges by file
	files := make(map[string][]NoteRange)
	var totalLines, aiLines, humanLines int

	for _, r := range ranges {
		lines := r.EndLine - r.StartLine + 1
		totalLines += lines
		if r.AuthorType == "ai" {
			aiLines += lines
		} else {
			humanLines += lines
		}
		files[r.FilePath] = append(files[r.FilePath], NoteRange{
			Start:      r.StartLine,
			End:        r.EndLine,
			AuthorType: r.AuthorType,
			Agent:      r.Agent,
			Model:      r.Model,
		})
	}

	payload := GitNotePayload{
		Version:  1,
		Tool:     "kai",
		Snapshot: snapshotHex,
		Files:    files,
		Summary: NoteSummary{
			TotalLines: totalLines,
			AILines:    aiLines,
			HumanLines: humanLines,
		},
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling git note: %w", err)
	}

	cmd := exec.Command("git", "notes", "--ref="+gitNotesRef, "add", "-f", "-m", string(data), commitHash)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("writing git note: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// ReadGitNote reads a kai authorship note from a commit, if one exists.
func ReadGitNote(repoPath, commitHash string) (*GitNotePayload, error) {
	cmd := exec.Command("git", "notes", "--ref="+gitNotesRef, "show", commitHash)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reading git note: %w", err)
	}

	var payload GitNotePayload
	if err := json.Unmarshal(output, &payload); err != nil {
		return nil, fmt.Errorf("parsing git note: %w", err)
	}

	return &payload, nil
}
