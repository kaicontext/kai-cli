package status

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"kai/internal/util"
)

// OutputFormat specifies how to format status output.
type OutputFormat int

const (
	// FormatDefault shows grouped status with counts
	FormatDefault OutputFormat = iota
	// FormatNameOnly shows just paths with status prefixes
	FormatNameOnly
	// FormatJSON outputs structured JSON
	FormatJSON
)

// JSONOutput is the JSON structure for status results.
type JSONOutput struct {
	Baseline    string          `json:"baseline"`
	BaselineRef string          `json:"baselineRef,omitempty"`
	NoBaseline  bool            `json:"noBaseline,omitempty"`
	Added       []string        `json:"added"`
	Modified    []string        `json:"modified"`
	Deleted     []string        `json:"deleted"`
	Summary     Summary         `json:"summary"`
	Semantic    *SemanticJSON   `json:"semantic,omitempty"`
}

// Summary contains counts for the JSON output.
type Summary struct {
	TotalChanges  int `json:"totalChanges"`
	AddedCount    int `json:"addedCount"`
	ModifiedCount int `json:"modifiedCount"`
	DeletedCount  int `json:"deletedCount"`
}

// SemanticJSON is the JSON structure for semantic analysis results.
type SemanticJSON struct {
	AffectedFiles  int            `json:"affectedFiles"`
	TotalChanges   int            `json:"totalChanges"`
	CategoryCounts map[string]int `json:"categoryCounts"`
}

// WriteOutput writes the status result to the given writer.
func WriteOutput(w io.Writer, result *Result, format OutputFormat) error {
	return WriteOutputWithSemantic(w, result, nil, format)
}

// WriteOutputWithSemantic writes the status result with optional semantic analysis.
func WriteOutputWithSemantic(w io.Writer, result *Result, semantic *SemanticResult, format OutputFormat) error {
	switch format {
	case FormatNameOnly:
		return writeNameOnly(w, result)
	case FormatJSON:
		return writeJSONWithSemantic(w, result, semantic)
	default:
		return writeDefaultWithSemantic(w, result, semantic)
	}
}

// writeNameOnly outputs paths with single-letter status prefixes.
func writeNameOnly(w io.Writer, result *Result) error {
	for _, path := range result.Added {
		fmt.Fprintf(w, "A %s\n", path)
	}
	for _, path := range result.Modified {
		fmt.Fprintf(w, "M %s\n", path)
	}
	for _, path := range result.Deleted {
		fmt.Fprintf(w, "D %s\n", path)
	}
	return nil
}

// writeJSONWithSemantic outputs structured JSON with optional semantic info.
func writeJSONWithSemantic(w io.Writer, result *Result, semantic *SemanticResult) error {
	output := JSONOutput{
		Baseline:    util.BytesToHex(result.BaselineID),
		BaselineRef: result.BaselineRef,
		NoBaseline:  result.NoBaseline,
		Added:       result.Added,
		Modified:    result.Modified,
		Deleted:     result.Deleted,
		Summary: Summary{
			TotalChanges:  result.TotalChanges(),
			AddedCount:    len(result.Added),
			ModifiedCount: len(result.Modified),
			DeletedCount:  len(result.Deleted),
		},
	}

	// Handle nil slices for cleaner JSON
	if output.Added == nil {
		output.Added = []string{}
	}
	if output.Modified == nil {
		output.Modified = []string{}
	}
	if output.Deleted == nil {
		output.Deleted = []string{}
	}

	// Add semantic info if available
	if semantic != nil && len(semantic.ChangeTypes) > 0 {
		categoryCounts := make(map[string]int)
		for cat, count := range semantic.CategoryCounts {
			categoryCounts[string(cat)] = count
		}
		output.Semantic = &SemanticJSON{
			AffectedFiles:  semantic.AffectedFiles,
			TotalChanges:   len(semantic.ChangeTypes),
			CategoryCounts: categoryCounts,
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

// writeDefaultWithSemantic outputs grouped status with counts and optional semantic info.
func writeDefaultWithSemantic(w io.Writer, result *Result, semantic *SemanticResult) error {
	// Handle no-baseline case (like git status before first commit)
	if result.NoBaseline {
		if len(result.Added) == 0 {
			fmt.Fprintln(w, "No files to capture")
			return nil
		}
		fmt.Fprintf(w, "Files to be captured (%d):\n", len(result.Added))
		for _, path := range result.Added {
			fmt.Fprintf(w, "  + %s\n", path)
		}
		return nil
	}

	if !result.HasChanges() {
		fmt.Fprintf(w, "No changes since %s\n", formatBaselineRef(result))
		return nil
	}

	fmt.Fprintf(w, "Changes since %s:\n", formatBaselineRef(result))
	fmt.Fprintln(w)

	if len(result.Added) > 0 {
		fmt.Fprintf(w, "  Added (%d):\n", len(result.Added))
		for _, path := range result.Added {
			fmt.Fprintf(w, "    + %s\n", path)
		}
	}

	if len(result.Modified) > 0 {
		fmt.Fprintf(w, "  Modified (%d):\n", len(result.Modified))
		for _, path := range result.Modified {
			fmt.Fprintf(w, "    ~ %s\n", path)
		}
	}

	if len(result.Deleted) > 0 {
		fmt.Fprintf(w, "  Deleted (%d):\n", len(result.Deleted))
		for _, path := range result.Deleted {
			fmt.Fprintf(w, "    - %s\n", path)
		}
	}

	// Add semantic summary if available
	if semantic != nil && len(semantic.ChangeTypes) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, FormatSemanticSummary(semantic))
	}

	return nil
}

// formatBaselineRef returns a display string for the baseline.
func formatBaselineRef(result *Result) string {
	shortID := util.BytesToHex(result.BaselineID)[:12]
	if result.BaselineRef != "" && !strings.HasPrefix(result.BaselineRef, "@") {
		return fmt.Sprintf("%s (%s)", result.BaselineRef, shortID)
	}
	return shortID
}
