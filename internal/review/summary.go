// Package review provides code review functionality for Kai changesets.
package review

import (
	"fmt"
	"sort"
	"strings"

	"kai/internal/diff"
)

// SemanticChange represents a high-level description of what changed.
type SemanticChange struct {
	Summary     string            // "Added user authentication flow"
	Kind        ChangeKind        // feature, fix, refactor, etc.
	Files       []string          // Files involved
	Symbols     []SymbolChange    // Functions/classes added/modified/removed
	Impact      *ImpactAnalysis   // What this affects
	Suggestions []AISuggestion    // AI review suggestions for this change
}

// ChangeKind categorizes the type of change.
type ChangeKind string

const (
	KindFeature  ChangeKind = "feature"
	KindFix      ChangeKind = "fix"
	KindRefactor ChangeKind = "refactor"
	KindDocs     ChangeKind = "docs"
	KindTest     ChangeKind = "test"
	KindChore    ChangeKind = "chore"
	KindBreaking ChangeKind = "breaking"
)

// SymbolChange describes a change to a code symbol.
type SymbolChange struct {
	Kind      diff.UnitKind // function, class, method, etc.
	Name      string        // Symbol name
	Action    diff.Action   // added, modified, removed
	File      string        // File path
	Signature string        // Function signature (if applicable)
	OldSig    string        // Previous signature (for modifications)
	Breaking  bool          // Is this a breaking change?
}

// ImpactAnalysis describes what a change affects.
type ImpactAnalysis struct {
	AffectedPaths []string   // API paths, routes affected
	Callers       []string   // Functions/files that call this
	Dependencies  []string   // New/changed dependencies
	Breaking      []string   // Breaking changes description
	RiskLevel     string     // low, medium, high
}

// AISuggestion represents an AI-generated review comment.
type AISuggestion struct {
	Level    string // info, warning, error
	Message  string
	File     string
	Line     int
	Symbol   string // Related symbol name
	Category string // security, performance, style, bug
}

// ReviewSummary is the top-level review structure.
type ReviewSummary struct {
	// Level 1: What changed (macro)
	Title       string           // Auto-generated or user-provided
	Description string           // Longer description
	Changes     []SemanticChange // Grouped semantic changes

	// Level 2: Aggregate impact
	TotalFiles    int
	TotalLines    int // Approximate
	APIChanges    int // Public API surface changes
	BreakingCount int

	// Level 3: AI suggestions
	Suggestions []AISuggestion

	// Raw data for drill-down
	SemanticDiff *diff.SemanticDiff
}

// FileCategorizer maps a file path to a category name for grouping.
// If nil, the default heuristic (path-based) categorizer is used.
type FileCategorizer func(path string) string

// BuildReviewSummary creates a ReviewSummary from a SemanticDiff.
// Pass a non-nil categorizer to group files by module instead of path heuristics.
func BuildReviewSummary(sd *diff.SemanticDiff, categorizers ...FileCategorizer) *ReviewSummary {
	var categorizer FileCategorizer
	if len(categorizers) > 0 && categorizers[0] != nil {
		categorizer = categorizers[0]
	}

	summary := &ReviewSummary{
		SemanticDiff: sd,
		TotalFiles:   len(sd.Files),
	}

	// Group changes by semantic meaning
	changes := groupChanges(sd, categorizer)
	summary.Changes = changes

	// Count API changes and breaking changes
	for _, change := range changes {
		for _, sym := range change.Symbols {
			if isAPISymbol(sym) {
				summary.APIChanges++
			}
			if sym.Breaking {
				summary.BreakingCount++
			}
		}
	}

	// Generate title if not provided
	if summary.Title == "" {
		summary.Title = generateTitle(changes)
	}

	return summary
}

// groupChanges groups file/unit diffs into semantic changes.
func groupChanges(sd *diff.SemanticDiff, categorizer FileCategorizer) []SemanticChange {
	classify := categorizeFile
	if categorizer != nil {
		classify = categorizer
	}

	groups := map[string]*SemanticChange{}

	for _, file := range sd.Files {
		category := classify(file.Path)
		group := groups[category]
		if group == nil {
			group = &SemanticChange{Kind: kindForCategory(category), Summary: category + " changes"}
			groups[category] = group
		}

		group.Files = append(group.Files, file.Path)

		for _, unit := range file.Units {
			sym := SymbolChange{
				Kind:      unit.Kind,
				Name:      unit.Name,
				Action:    unit.Action,
				File:      file.Path,
				Signature: unit.AfterSig,
				OldSig:    unit.BeforeSig,
				Breaking:  unit.ChangeType == "API_SURFACE_CHANGED" && unit.Action == diff.ActionModified,
			}
			group.Symbols = append(group.Symbols, sym)
		}
	}

	// Sort group keys for stable output
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var result []SemanticChange
	for _, key := range keys {
		g := groups[key]
		if len(g.Files) > 0 {
			g.Summary = buildGroupSummary(g)
			result = append(result, *g)
		}
	}

	return result
}

// kindForCategory maps a category name to a ChangeKind.
func kindForCategory(category string) ChangeKind {
	switch category {
	case "api":
		return KindFeature
	case "test":
		return KindTest
	case "docs":
		return KindDocs
	case "config":
		return KindChore
	default:
		return KindRefactor
	}
}

// categorizeFile determines the category of a file.
func categorizeFile(path string) string {
	lower := strings.ToLower(path)

	// Test files
	if strings.Contains(lower, "_test.") || strings.Contains(lower, ".test.") ||
		strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") ||
		strings.HasSuffix(lower, "_test.go") || strings.HasSuffix(lower, ".spec.ts") {
		return "test"
	}

	// Documentation
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".txt") ||
		strings.Contains(lower, "/docs/") {
		return "docs"
	}

	// Configuration
	if strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".yaml") ||
		strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".toml") ||
		strings.Contains(lower, "config") || lower == "package.json" ||
		lower == "go.mod" || lower == "go.sum" {
		return "config"
	}

	// API/handlers
	if strings.Contains(lower, "/api/") || strings.Contains(lower, "/handler") ||
		strings.Contains(lower, "/route") || strings.Contains(lower, "/endpoint") ||
		strings.Contains(lower, "controller") {
		return "api"
	}

	return "internal"
}

// buildGroupSummary creates a human-readable summary for a change group.
func buildGroupSummary(g *SemanticChange) string {
	if len(g.Symbols) == 0 {
		return fmt.Sprintf("%d files changed", len(g.Files))
	}

	// Count by action
	added, modified, removed := 0, 0, 0
	for _, s := range g.Symbols {
		switch s.Action {
		case diff.ActionAdded:
			added++
		case diff.ActionModified:
			modified++
		case diff.ActionRemoved:
			removed++
		}
	}

	parts := []string{}
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", added))
	}
	if modified > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", modified))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}

	return fmt.Sprintf("%d files, %s", len(g.Files), strings.Join(parts, ", "))
}

// generateTitle creates an auto-generated title from changes.
func generateTitle(changes []SemanticChange) string {
	if len(changes) == 0 {
		return "No changes"
	}

	// Find most significant change
	var primary *SemanticChange
	for i := range changes {
		c := &changes[i]
		if c.Kind == KindFeature || c.Kind == KindBreaking {
			primary = c
			break
		}
		if primary == nil || len(c.Symbols) > len(primary.Symbols) {
			primary = c
		}
	}

	if primary == nil || len(primary.Symbols) == 0 {
		return fmt.Sprintf("%d files changed", countFiles(changes))
	}

	// Build title from primary symbols
	sym := primary.Symbols[0]
	switch sym.Action {
	case diff.ActionAdded:
		return fmt.Sprintf("Add %s %s", sym.Kind, sym.Name)
	case diff.ActionModified:
		return fmt.Sprintf("Update %s %s", sym.Kind, sym.Name)
	case diff.ActionRemoved:
		return fmt.Sprintf("Remove %s %s", sym.Kind, sym.Name)
	}

	return "Code changes"
}

func countFiles(changes []SemanticChange) int {
	seen := make(map[string]bool)
	for _, c := range changes {
		for _, f := range c.Files {
			seen[f] = true
		}
	}
	return len(seen)
}

func isAPISymbol(sym SymbolChange) bool {
	if len(sym.Name) == 0 {
		return false
	}

	// Infer language from file extension
	ext := strings.ToLower(sym.File)
	if idx := strings.LastIndex(ext, "."); idx >= 0 {
		ext = ext[idx:]
	}

	switch ext {
	case ".go":
		// Go: exported if first letter is uppercase
		return sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	case ".py":
		// Python: public if doesn't start with underscore
		return sym.Name[0] != '_'
	case ".rb":
		// Ruby: public by default; private/protected are method-level, not naming
		// Treat all as API since we can't tell from name alone
		return true
	case ".rs":
		// Rust: exported with `pub` keyword, but we only have the name here
		// Fall back to uppercase-starting types being API
		return sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	case ".js", ".ts", ".jsx", ".tsx", ".mjs", ".cjs":
		// JS/TS: exported via `export` keyword, not naming convention
		// Without export info, treat all top-level functions/classes as API
		return sym.Kind == diff.UnitFunction || sym.Kind == diff.UnitClass
	default:
		// Unknown language: uppercase = exported (Go convention as fallback)
		return sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	}
}

// FormatSummary returns a CLI-friendly summary string.
func (rs *ReviewSummary) FormatSummary() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## %s\n\n", rs.Title))

	sb.WriteString("CHANGES\n")
	for i, change := range rs.Changes {
		prefix := "  "
		if change.Kind == KindBreaking {
			prefix = "! "
		}
		sb.WriteString(fmt.Sprintf("%s[%d] %s (%s)\n", prefix, i+1, change.Summary, change.Kind))
	}
	sb.WriteString("\n")

	if rs.BreakingCount > 0 {
		sb.WriteString(fmt.Sprintf("⚠ %d breaking changes\n\n", rs.BreakingCount))
	}

	if len(rs.Suggestions) > 0 {
		sb.WriteString("SUGGESTIONS\n")
		for _, s := range rs.Suggestions {
			icon := "•"
			if s.Level == "warning" {
				icon = "⚠"
			} else if s.Level == "error" {
				icon = "✗"
			}
			sb.WriteString(fmt.Sprintf("  %s %s\n", icon, s.Message))
		}
	}

	return sb.String()
}

// FormatChange returns detailed info for a single change.
func (rs *ReviewSummary) FormatChange(index int) string {
	if index < 0 || index >= len(rs.Changes) {
		return "Invalid change index"
	}

	change := rs.Changes[index]
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("━━━ %s ━━━\n\n", change.Summary))

	sb.WriteString("Files:\n")
	for _, f := range change.Files {
		sb.WriteString(fmt.Sprintf("  • %s\n", f))
	}
	sb.WriteString("\n")

	if len(change.Symbols) > 0 {
		sb.WriteString("Symbols:\n")

		// Sort by action (added, modified, removed)
		sorted := make([]SymbolChange, len(change.Symbols))
		copy(sorted, change.Symbols)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Action < sorted[j].Action
		})

		for _, sym := range sorted {
			action := "~"
			switch sym.Action {
			case diff.ActionAdded:
				action = "+"
			case diff.ActionRemoved:
				action = "-"
			}

			line := fmt.Sprintf("  %s %s %s", action, sym.Kind, sym.Name)
			if sym.Signature != "" {
				line += fmt.Sprintf(" → %s", sym.Signature)
			}
			if sym.Breaking {
				line += " [BREAKING]"
			}
			sb.WriteString(line + "\n")
		}
	}

	return sb.String()
}
