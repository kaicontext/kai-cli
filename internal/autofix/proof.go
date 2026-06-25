package autofix

// proof.go holds the parts that decide whether a fix is publishable and
// render the evidence — kept free of I/O so they're unit-testable. The
// publish gate is the whole point of the headless loop: a human isn't in
// the room, so "open the PR as ready" must be earned by three independent
// signals agreeing, and the default on any doubt is draft, never ready.

import (
	"fmt"
	"strings"

	"kai/internal/contract"
)

// ReviewResult is the second agent's adversarial read of the diff. It is
// distinct from the semantic judge (which asks "does this implement the
// intent?") — the reviewer asks "is this change itself sound?".
type ReviewResult struct {
	Approved bool     // reviewer found nothing blocking
	Blocking []string // must-fix findings; non-empty ⇒ not Approved
	Notes    string   // free-form summary for the PR body
}

// PublishDecision is the gate's verdict: may this PR open as ready, and
// why / why not. Reasons are always populated so a draft PR can explain
// itself in its own body.
type PublishDecision struct {
	Ready   bool
	Reasons []string
}

// Decide combines the three verification signals into a publish verdict.
// Ready requires ALL of:
//   - deterministic checks did not fail (tests/typecheck not Broken),
//   - the semantic judge returned a confirmed match (Matches == true),
//   - the reviewer approved with no blocking findings.
//
// Anything short of all three keeps the PR a draft. Crucially, semantic
// "unsure" (Matches == nil) is NOT enough — that's the clean_unconfirmed
// boundary the verify layer is built to preserve, and shipping on it
// would be the wrong-"yes" failure the whole design avoids.
func Decide(det contract.Verdict, sem contract.SemanticResult, rev ReviewResult) PublishDecision {
	var reasons []string
	ready := true

	switch det {
	case contract.VerdictBroken:
		ready = false
		reasons = append(reasons, "deterministic checks failed (tests or typecheck)")
	case contract.VerdictNoIntent, contract.VerdictDrifting:
		// No hard failure, but no positive signal either — let the
		// semantic + review gates carry it, just note the weak floor.
		reasons = append(reasons, "deterministic layer inconclusive: "+verdictLabel(det))
	case contract.VerdictCleanUnconfirmed, contract.VerdictVerified:
		reasons = append(reasons, "deterministic checks passed")
	}

	switch {
	case sem.Matches == nil:
		ready = false
		reasons = append(reasons, "semantic judge unsure the change implements the issue")
	case !*sem.Matches:
		ready = false
		reasons = append(reasons, "semantic judge says the change does NOT implement the issue")
	default:
		reasons = append(reasons, "semantic judge confirms the change implements the issue")
	}

	if rev.Approved && len(rev.Blocking) == 0 {
		reasons = append(reasons, "code review found nothing blocking")
	} else {
		ready = false
		if len(rev.Blocking) > 0 {
			reasons = append(reasons, fmt.Sprintf("code review raised %d blocking finding(s)", len(rev.Blocking)))
		} else {
			reasons = append(reasons, "code review did not approve")
		}
	}

	return PublishDecision{Ready: ready, Reasons: reasons}
}

// String renders a Verdict for human/markdown output. (contract.Verdict
// has its own label method internally; this keeps proof.go self-contained
// for the few values we surface.)
func verdictLabel(v contract.Verdict) string {
	switch v {
	case contract.VerdictVerified:
		return "verified"
	case contract.VerdictCleanUnconfirmed:
		return "clean (unconfirmed)"
	case contract.VerdictBroken:
		return "broken"
	case contract.VerdictDrifting:
		return "drifting"
	case contract.VerdictNoIntent:
		return "no intent"
	default:
		return string(v)
	}
}

// Evidence is everything that goes into a PR body — gathered by the
// command, rendered here so the format is testable in isolation.
type Evidence struct {
	IssueNumber int
	IssueTitle  string
	IssueURL    string
	Branch      string
	Model       string

	FilesChanged []string
	TestSummary  string // VerifyResult.Summary (deterministic run)
	DetVerdict   contract.Verdict
	Semantic     contract.SemanticResult
	Residue      []string
	Review       ReviewResult
	AgentSummary string // the fixer agent's own final text

	Decision PublishDecision

	TokensIn  int
	TokensOut int
}

// marker tags PRs/comments this loop produced so re-runs can find and
// update them instead of duplicating.
const marker = "<!-- kai-autofix -->"

// RenderPRBody builds the structured proof artifact. It is intentionally
// a report, not prose: every claim ("tests passed", "judge confirmed")
// is backed by a line a reviewer can check.
func RenderPRBody(e Evidence) string {
	var b strings.Builder
	b.WriteString(marker + "\n")
	fmt.Fprintf(&b, "Closes #%d.\n\n", e.IssueNumber)

	readyEmoji := "🟡 opened as **draft**"
	if e.Decision.Ready {
		readyEmoji = "🟢 **ready for review**"
	}
	fmt.Fprintf(&b, "## Auto-fix result: %s\n\n", readyEmoji)
	b.WriteString("**Why:**\n")
	for _, r := range e.Decision.Reasons {
		mark := "✓"
		if strings.Contains(r, "failed") || strings.Contains(r, "unsure") ||
			strings.Contains(r, "NOT") || strings.Contains(r, "blocking") ||
			strings.Contains(r, "did not") || strings.Contains(r, "inconclusive") {
			mark = "✗"
		}
		fmt.Fprintf(&b, "- %s %s\n", mark, r)
	}
	b.WriteString("\n")

	b.WriteString("## What changed\n\n")
	if e.AgentSummary != "" {
		b.WriteString(strings.TrimSpace(e.AgentSummary) + "\n\n")
	}
	if len(e.FilesChanged) > 0 {
		b.WriteString("Files touched:\n")
		for _, f := range e.FilesChanged {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Verification\n\n")
	fmt.Fprintf(&b, "- **Tests / typecheck:** %s — %s\n",
		verdictLabel(e.DetVerdict), oneLine(e.TestSummary))
	fmt.Fprintf(&b, "- **Semantic judge:** %s", semLabel(e.Semantic))
	if e.Semantic.Note != "" {
		fmt.Fprintf(&b, " — %s", e.Semantic.Note)
	}
	b.WriteString("\n")
	reviewLine := "approved"
	if !e.Review.Approved || len(e.Review.Blocking) > 0 {
		reviewLine = "changes requested"
	}
	fmt.Fprintf(&b, "- **Code review:** %s", reviewLine)
	if e.Review.Notes != "" {
		fmt.Fprintf(&b, " — %s", oneLine(e.Review.Notes))
	}
	b.WriteString("\n")

	if len(e.Review.Blocking) > 0 {
		b.WriteString("\n**Blocking review findings:**\n")
		for _, f := range e.Review.Blocking {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(e.Residue) > 0 {
		b.WriteString("\n**Open questions for a human** (why this stayed a draft):\n")
		for _, q := range e.Residue {
			fmt.Fprintf(&b, "- [ ] %s\n", q)
		}
	}

	b.WriteString("\n---\n")
	fmt.Fprintf(&b, "_Generated headlessly by kai · model `%s` · %d in / %d out tokens._\n",
		e.Model, e.TokensIn, e.TokensOut)
	return b.String()
}

func semLabel(s contract.SemanticResult) string {
	switch {
	case s.Matches == nil:
		return "unsure"
	case *s.Matches:
		return "confirmed match"
	default:
		return "does not match"
	}
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		return s[:197] + "…"
	}
	if s == "" {
		return "(none)"
	}
	return s
}
