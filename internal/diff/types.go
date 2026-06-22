// Package diff provides semantic diff types and operations.
package diff

// Action describes what happened to a code unit.
type Action string

const (
	ActionAdded    Action = "added"
	ActionModified Action = "modified"
	ActionRemoved  Action = "removed"
)

// UnitKind describes the type of semantic unit.
type UnitKind string

const (
	UnitFunction UnitKind = "function"
	UnitMethod   UnitKind = "method"
	UnitClass    UnitKind = "class"
	UnitStruct   UnitKind = "struct"
	UnitConst    UnitKind = "const"
	UnitVar      UnitKind = "var"
	UnitType     UnitKind = "type"
	UnitImport   UnitKind = "import"
)

// SemanticDiff represents the semantic difference between two snapshots.
type SemanticDiff struct {
	Base  string     // Base snapshot ID
	Head  string     // Head snapshot ID
	Files []FileDiff // Per-file changes
}

// FileDiff represents changes to a single file.
type FileDiff struct {
	Path       string     // File path
	Action     Action     // added, modified, removed
	BeforeHash string     // Content hash before (empty if added)
	AfterHash  string     // Content hash after (empty if removed)
	Units      []UnitDiff // Semantic unit changes within the file
}

// UnitDiff represents a change to a semantic code unit (function, class, etc).
type UnitDiff struct {
	Kind       UnitKind // function, class, method, etc.
	Name       string   // Symbol name
	FQName     string   // Fully-qualified name (pkg.Class.method)
	Action     Action   // added, modified, removed
	BeforeSig  string   // Signature before (for modifications)
	AfterSig   string   // Signature after
	ChangeType string   // API_SURFACE_CHANGED, IMPLEMENTATION_CHANGED, etc.
	BeforeBody string   // Body before (optional, for detailed diff)
	AfterBody  string   // Body after (optional)
}

// NewSemanticDiff creates a SemanticDiff from changeset data.
func NewSemanticDiff(base, head string, files []FileDiff) *SemanticDiff {
	return &SemanticDiff{
		Base:  base,
		Head:  head,
		Files: files,
	}
}
