package verify

import (
	"fmt"
	"strings"

	"github.com/kaicontext/kai-engine/contract"
)

// SemanticSystem is the system prompt for the intent-match judge. The bias is
// deliberate: a wrong `verified` poisons the whole proposition, so the judge
// must abstain (unsure) rather than guess "yes". Over-reporting uncertainty is
// the correct failure mode.
const SemanticSystem = `You are a verification judge. Given a declared INTENT and a code DIFF, judge
ONLY whether the diff implements the declared intent — not whether the code is
good, not whether tests pass (that's checked separately).

Rules:
- Answer "yes" ONLY when the diff clearly and completely implements the intent.
- Answer "no" when the diff contradicts the intent or implements something else.
- Answer "unsure" when you cannot confirm the intent is met from the diff alone
  (partial implementation, the relevant change isn't in the diff, ambiguity).
- A wrong "yes" is far worse than "unsure". When in doubt, answer "unsure".
- Raise a RESIDUE question for anything a human should confirm (e.g. intent says
  "retry", diff adds backoff — not the same thing).

Respond in exactly this format:
VERDICT: yes|no|unsure
NOTE: <one sentence explaining the verdict>
RESIDUE:
- <a specific yes/no question for a human, or omit the line if none>`

// BuildSemanticPrompt assembles the user message: the declared intent (+ folded
// plan when present) and the diff to judge it against.
func BuildSemanticPrompt(intent string, plan contract.Plan, diff string) string {
	var b strings.Builder
	b.WriteString("INTENT:\n")
	b.WriteString(strings.TrimSpace(intent))
	b.WriteString("\n")
	if len(plan.Steps) > 0 {
		b.WriteString("\nPLAN:\n")
		for i, s := range plan.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	}
	b.WriteString("\nDIFF:\n")
	if strings.TrimSpace(diff) == "" {
		b.WriteString("(no changes in the working tree)\n")
	} else {
		b.WriteString(diff)
		b.WriteString("\n")
	}
	return b.String()
}

// ParseSemantic extracts the verdict, note, and residue questions from the
// model's response. Tolerant of formatting drift. Matches is nil for "unsure"
// (the clean_unconfirmed boundary), &true for "yes", &false for "no".
func ParseSemantic(raw string) (contract.SemanticResult, []string) {
	var res contract.SemanticResult
	var residue []string
	inResidue := false

	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(lower, "verdict:"):
			inResidue = false
			v := strings.ToLower(strings.TrimSpace(t[len("verdict:"):]))
			switch {
			case strings.HasPrefix(v, "yes"):
				yes := true
				res.Matches = &yes
			case strings.HasPrefix(v, "no"):
				no := false
				res.Matches = &no
			default: // unsure / anything else → nil (not established)
				res.Matches = nil
			}
		case strings.HasPrefix(lower, "note:"):
			inResidue = false
			res.Note = strings.TrimSpace(t[len("note:"):])
		case strings.HasPrefix(lower, "residue:"):
			inResidue = true
			// a question may trail on the same line
			if rest := strings.TrimSpace(t[len("residue:"):]); rest != "" && rest != "-" {
				residue = append(residue, strings.TrimPrefix(rest, "- "))
			}
		case inResidue && strings.HasPrefix(t, "-"):
			q := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			if q != "" && !strings.EqualFold(q, "none") {
				residue = append(residue, q)
			}
		}
	}
	return res, residue
}

// SemanticVerdict combines the deterministic and semantic layers into the final
// verdict. A hard deterministic failure beats any semantic opinion (you can't
// be verified with failing tests). `verified` requires a confirmed semantic
// match; everything short of that stays clean_unconfirmed — never verified.
func SemanticVerdict(structural contract.CheckResult, sem contract.SemanticResult) contract.Verdict {
	if structural.TestsPass != nil && !*structural.TestsPass {
		return contract.VerdictBroken
	}
	if sem.Matches != nil && *sem.Matches {
		return contract.VerdictVerified
	}
	return contract.VerdictCleanUnconfirmed
}
