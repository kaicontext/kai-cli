package autofix

// prompts.go builds the prompts the loop sends and parses the reviewer's
// reply. The semantic judge reuses verify.SemanticSystem /
// BuildSemanticPrompt — this file only owns the fixer and reviewer
// prompts, which are autofix-specific.

import (
	"fmt"
	"strings"
)

// BuildFixPrompt frames the issue as a concrete task for the agent loop.
// The runner splits a leading "System: ..." block off as the system
// role (see agent.Options.Prompt), so we follow that convention.
func BuildFixPrompt(iss *Issue) string {
	var b strings.Builder
	b.WriteString("System: You are kai operating fully headlessly — no human will review your ")
	b.WriteString("intermediate steps. Implement a correct, minimal, well-scoped fix for the ")
	b.WriteString("GitHub issue below. Make the smallest change that fully resolves it. Match ")
	b.WriteString("the surrounding code's style. Add or update a test that would have caught ")
	b.WriteString("the bug when it is reasonable to do so. Do NOT touch unrelated code, bump ")
	b.WriteString("versions, or reformat files. When done, end with a 2-4 sentence summary of ")
	b.WriteString("what you changed and why.\n\n")
	fmt.Fprintf(&b, "User: Fix this issue.\n\n#%d: %s\n\n", iss.Number, iss.Title)
	if body := strings.TrimSpace(iss.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	} else {
		b.WriteString("(no description provided)\n")
	}
	return b.String()
}

// ReviewSystem instructs the second agent to read the diff adversarially.
// It mirrors the project's /code-review bias: report only findings it is
// confident about, and gate "approve" on the change being sound.
const ReviewSystem = `You are a strict code reviewer. You are given the ISSUE being fixed and the
DIFF that claims to fix it. Judge ONLY the change in the diff — its correctness,
safety, and whether it could break existing behavior.

Rules:
- Report a BLOCKING finding only when you are confident the change is wrong,
  unsafe, or incomplete in a way that would fail in production. Be specific.
- Style nits, speculative concerns, and "could be cleaner" are NOT blocking;
  fold them into NOTES if worth mentioning at all.
- If the change is sound, approve it. Do not invent blocking issues to seem rigorous.

Respond in exactly this format:
VERDICT: approve|request-changes
NOTES: <one or two sentences, or "none">
BLOCKING:
- <a specific must-fix finding, or omit the line if none>`

// BuildReviewPrompt assembles the reviewer's user message.
func BuildReviewPrompt(issueTitle, issueBody, diff string) string {
	var b strings.Builder
	b.WriteString("ISSUE:\n")
	b.WriteString(strings.TrimSpace(issueTitle))
	if body := strings.TrimSpace(issueBody); body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	b.WriteString("\n\nDIFF:\n")
	if strings.TrimSpace(diff) == "" {
		b.WriteString("(no changes)\n")
	} else {
		b.WriteString(diff)
		b.WriteString("\n")
	}
	return b.String()
}

// ParseReview extracts the reviewer's verdict from its reply. Tolerant of
// formatting drift, and conservative: anything that isn't a clean
// "approve" with no blocking findings is treated as not-approved.
func ParseReview(raw string) ReviewResult {
	var res ReviewResult
	inBlocking := false
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(lower, "verdict:"):
			inBlocking = false
			v := strings.TrimSpace(lower[len("verdict:"):])
			res.Approved = strings.HasPrefix(v, "approve")
		case strings.HasPrefix(lower, "notes:"):
			inBlocking = false
			n := strings.TrimSpace(t[len("notes:"):])
			if !strings.EqualFold(n, "none") {
				res.Notes = n
			}
		case strings.HasPrefix(lower, "blocking:"):
			inBlocking = true
			if rest := strings.TrimSpace(t[len("blocking:"):]); rest != "" && rest != "-" {
				res.Blocking = append(res.Blocking, strings.TrimPrefix(rest, "- "))
			}
		case inBlocking && strings.HasPrefix(t, "-"):
			f := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			if f != "" && !strings.EqualFold(f, "none") {
				res.Blocking = append(res.Blocking, f)
			}
		}
	}
	// A blocking finding overrides a stray "approve".
	if len(res.Blocking) > 0 {
		res.Approved = false
	}
	return res
}

// BranchName is the deterministic branch a given issue maps to, so the
// idempotency check and the create step agree.
func BranchName(issueNum int) string {
	return fmt.Sprintf("kai/issue-%d", issueNum)
}
