// Package contract is the verification-layer contract store: a living, folded
// statement of desired end-state with classified prompt provenance.
//
// Horizon 1 / Phase 1 scope: the persistent data model and store, manual only.
// No daemon, no classifier, no semantic layer yet — those land in later phases
// and populate the Plan/Continuous/Semantic fields defined here.
//
// A contract is the consolidated desired end-state folded out of a prompt
// stream; most prompts are events that never touch a contract. The verdict
// vocabulary is built to over-report uncertainty: a confidently wrong
// "verified" poisons the whole proposition, so clean_unconfirmed and no_intent
// exist to keep the daemon honest about the boundary of what it checked.
package contract

// Verdict is the load-bearing UI signal. The words must stay visually
// distinct — never collapse clean_unconfirmed or no_intent into verified.
type Verdict string

const (
	// VerdictVerified — semantic layer confirmed implementation matches intent.
	VerdictVerified Verdict = "verified"
	// VerdictCleanUnconfirmed — deterministic layer passed, semantic match not
	// established. The honest-uncertainty state; must look different from verified.
	VerdictCleanUnconfirmed Verdict = "clean_unconfirmed"
	// VerdictDrifting — tree or intent changed since the last verified snapshot.
	VerdictDrifting Verdict = "drifting"
	// VerdictBroken — an invariant, test, or blast-radius check failed.
	VerdictBroken Verdict = "broken"
	// VerdictNoIntent — hand-written code with no contract; structure only.
	VerdictNoIntent Verdict = "no_intent"
)

// Display returns the human-facing form of the verdict for the CLI surface.
func (v Verdict) Display() string {
	switch v {
	case VerdictVerified:
		return "verified"
	case VerdictCleanUnconfirmed:
		return "clean · unconfirmed"
	case VerdictDrifting:
		return "drifting"
	case VerdictBroken:
		return "broken"
	case VerdictNoIntent:
		return "no-intent"
	default:
		return string(v)
	}
}

// Glyph returns the status glyph. drifting reuses ~ (see the open design call).
func (v Verdict) Glyph() string {
	switch v {
	case VerdictVerified:
		return "✓"
	case VerdictBroken:
		return "✗"
	default: // clean_unconfirmed, drifting, no_intent
		return "~"
	}
}

// Source sets confidence and residue width. kit is native/high-confidence;
// traced is best-effort with wider residue (permanently asymmetric by design).
type Source string

const (
	SourceKit    Source = "kit"
	SourceTraced Source = "traced"
)

// EventKind classifies a prompt in the provenance stream.
type EventKind string

const (
	EventIntent     EventKind = "intent"     // opens a new contract
	EventRefinement EventKind = "refinement" // mutates an existing contract's statement
	EventAmend      EventKind = "amend"      // manual refinement from the CLI
	EventQuestion   EventKind = "question"   // never touches a contract
	EventCommand    EventKind = "command"    // never touches a contract
	EventChatter    EventKind = "chatter"    // never touches a contract
	EventUnsure     EventKind = "unsure"     // abstain — routed to low-confidence bucket
)

// AppliesToContract reports whether an event of this kind mutates contract state.
func (k EventKind) AppliesToContract() bool {
	return k == EventIntent || k == EventRefinement || k == EventAmend
}

// Event is one classified prompt in a contract's provenance trail.
type Event struct {
	Ref     string    `json:"ref"`  // p1, p3, a1
	Text    string    `json:"text"`
	Kind    EventKind `json:"kind"`
	Applied bool      `json:"applied"` // did it mutate the contract
	TS      int64     `json:"ts"`
}

// ResidueOrigin records why an item was flagged for human review.
type ResidueOrigin string

const (
	ResidueSemanticUnconfirmed ResidueOrigin = "semantic_unconfirmed"
	ResidueClassifierUnsure    ResidueOrigin = "classifier_unsure"
	ResidueOverride            ResidueOrigin = "override"
)

// ResidueItem is a specific question the daemon surfaced for a human. Residue
// is not a status — it's a list attached to a contract and shown in status.
type ResidueItem struct {
	Contract string        `json:"contract"`
	Prompt   string        `json:"prompt"` // human-readable question for the reviewer
	Origin   ResidueOrigin `json:"origin"`
}

// Plan is the structured materialization of the folded intent — the
// verification target (verifying against the plan, not the raw request, keeps
// intent pinned to something checkable). Folded in later phases.
type Plan struct {
	Steps      []string `json:"steps,omitempty"`
	FoldedFrom int      `json:"foldedFrom,omitempty"` // count of prompts behind the plan
}

// CheckResult is the continuous deterministic layer's live result (Phase 2).
type CheckResult struct {
	Typecheck  *bool    `json:"typecheck,omitempty"`
	TestsPass  *bool    `json:"testsPass,omitempty"`
	TestsTotal int      `json:"testsTotal,omitempty"`
	Failures   []string `json:"failures,omitempty"`
	RanAt      int64    `json:"ranAt,omitempty"`
}

// SemanticResult is the expensive LLM layer's timestamped result (Phase 3).
// Matches == nil means "not established" — the clean_unconfirmed boundary. The
// UI must never present a stale semantic check as live, hence RanAt.
type SemanticResult struct {
	Matches *bool  `json:"matches,omitempty"`
	Note    string `json:"note,omitempty"`
	RanAt   int64  `json:"ranAt,omitempty"`
}

// Contract is the consolidated desired end-state with classified provenance.
type Contract struct {
	ID         string         `json:"id"`        // stable human-readable slug
	Statement  string         `json:"statement"` // current folded intent (display form)
	Plan       Plan           `json:"plan"`
	Status     Verdict        `json:"status"`
	Source     Source         `json:"source"`
	Provenance []Event        `json:"provenance"`
	Residue    []ResidueItem  `json:"residue"`
	Continuous CheckResult    `json:"continuous"`
	Semantic   SemanticResult `json:"semantic"`
	Closed     bool           `json:"closed,omitempty"` // intentionally done (kai contract close)
	CreatedAt  int64          `json:"createdAt"`
	UpdatedAt  int64          `json:"updatedAt"`
}

// AddEvent appends a classified prompt to provenance, stamping Applied from the
// event kind. Updating the folded Statement is the caller's responsibility
// (folding); this only records the trail so a misclassification stays a
// visible, editable line rather than a silent corruption.
func (c *Contract) AddEvent(e Event) {
	e.Applied = e.Kind.AppliesToContract()
	c.Provenance = append(c.Provenance, e)
}
