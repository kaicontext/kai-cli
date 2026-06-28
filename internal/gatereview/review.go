package gatereview

import (
	"context"
	"fmt"
	"strings"

	"github.com/kaicontext/kai-engine/message"
	"kai/internal/agent/provider"
	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/util"
)

// Recommendation is the gate reviewer's terminal verdict. APPROVE and
// REJECT are self-explanatory; FIX_THEN_APPROVE means the audit found
// machine-fixable issues (Phase 2 will spawn a fix agent); ASK means
// the reviewer can't tell and the human should decide.
type Recommendation string

const (
	RecApprove        Recommendation = "APPROVE"
	RecFixThenApprove Recommendation = "FIX_THEN_APPROVE"
	RecReject         Recommendation = "REJECT"
	RecAsk            Recommendation = "ASK"
)

// Issue is one item the audit step turned up. Fixable=true means the
// fix agent (Phase 2) is expected to handle it; false means it needs
// human judgment regardless of model confidence.
type Issue struct {
	Description string
	Fixable     bool
}

// Result is the structured output the CLI / TUI render. Raw is the
// model's verbatim response so callers can fall back to it when the
// parser couldn't extract sections (rare, but shouldn't lose data).
type Result struct {
	SnapshotID     []byte
	Verdict        string
	BlastRadius    int
	Reasons        []string
	Touches        []string
	Summary        string
	AuditClean     bool
	Issues         []Issue
	Recommendation Recommendation
	RecReason      string
	Raw            string
}

// Inputs collects the optional context passed into Review. SessionTurns
// is a pre-formatted "USER: ... / KAI: ..." block (the REPL already
// owns this format); the prompt injects it as <session_context> so the
// model can spot mismatches between what the user asked for and what
// the held snapshot actually does.
type Inputs struct {
	SessionTurns string

	// VerifySummary is the result of a ground-truth build+test run
	// over the held change (see orchestrator.VerifyWorkspace). When
	// it reports a failure, the reviewer must not recommend APPROVE —
	// execution beats opinion. Empty when no verification was run.
	VerifySummary string

	// OnState, if set, is forwarded to the provider's
	// Request.OnState so the TUI can render real HTTP/SSE call
	// state (sent / connected / streaming / done) during the
	// audit's LLM round-trip — a 10s gate review otherwise looks
	// like a frozen TUI.
	OnState func(provider.RequestState)
}

// Review runs the LLM review on a single held snapshot and returns
// a structured Result. The diff is loaded internally via
// HeldSnapshotDiff; graph context (touches, reasons) comes off the
// snapshot's payload — no extra graph traversal is needed because the
// gate decision already wrote those fields when the integration was
// classified.
//
// Diffs over ~24KB are truncated to keep the call within a single LLM
// turn; the model is told the diff was clipped so it doesn't claim
// completeness it can't verify.
func Review(ctx context.Context, prov provider.Provider, model string, db *graph.DB, snap *graph.Node, in Inputs) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if prov == nil {
		return nil, fmt.Errorf("Review: provider is nil")
	}
	patch, err := HeldSnapshotDiff(db, snap)
	if err != nil {
		return nil, err
	}
	const maxPatchBytes = 24000
	truncated := false
	if len(patch) > maxPatchBytes {
		patch = patch[:maxPatchBytes]
		truncated = true
	}

	verdict, _ := snap.Payload["gateVerdict"].(string)
	blastF, _ := snap.Payload["gateBlastRadius"].(float64)
	blast := int(blastF)
	reasons := stringList(snap.Payload["gateReasons"])
	touches := stringList(snap.Payload["gateTouches"])
	criteria := stringList(snap.Payload["acceptanceCriteria"])

	short := util.BytesToHex(snap.ID)
	if len(short) > 12 {
		short = short[:12]
	}

	system := `You are Kai's safety gate reviewer. A code change was held for human review.
Summarize what it does, audit it for problems a human reviewer would catch, and recommend an action.

Audit checklist (not exhaustive):
- Functions deleted without replacement (callers will break)
- Test coverage on changed functions
- Secrets or credentials in the diff
- Obvious logic errors
- Import changes that break downstream
- Files that were changed but shouldn't have been (unrelated edits)
- Intent, not just instruction: judge the change against what the user actually
  wanted and WHY (see session_context, and acceptance_criteria if present), not
  only the literal wording. A change can be technically correct and still miss
  the point — e.g. "dedupe this so a new case can't be forgotten" answered with
  a per-case helper that a new case must STILL be added to. If an
  acceptance_criteria block is present, check the change against EACH criterion
  and flag any it fails. A correct-but-shallow change is an ISSUE.
- Test quality, not just test presence: new tests should be well-structured.
  Several near-identical test functions differing only in a value should be a
  single table-driven test; flag copy-pasted test bodies and missing cases.

What is and is not an issue. Most held changes are correct and narrowly
scoped — for those the right audit is CLEAN and the right recommendation is
APPROVE. Do not manufacture issues to look thorough: a short clean audit on a
good change is the correct outcome, not a failure to find something. If the
change does what was asked and the tests pass, the default answer is APPROVE.

Raise an issue ONLY for a real defect — something that makes the change wrong
or unsafe: broken callers, a logic error, a failed acceptance criterion,
missing coverage on changed behavior, a secret in the diff, an unrelated edit.

Do NOT raise an issue for cosmetics: a comment that is now slightly stale or
imprecise, wording that could be tighter, a narrative that could read more
clearly, a test value you would have picked differently. None of those make
the change wrong. If one genuinely seems worth mentioning, put it in ONE
sentence on the RECOMMENDATION line — never as a numbered issue. "[fixable]"
means the code or behavior is wrong and an edit is required; it does not mean
prose could be improved. A change that is correct and tested gets APPROVE even
if a nearby comment could read better — withholding APPROVE over comment
wording is itself a mistake.

A "kai-rename-keep" comment is a deliberate, load-bearing annotation, not
cruft. Kai's rename-completeness gate skips any line carrying it; it marks a
reference to an old or renamed name that is intentionally retained (a
still-valid model in a picker, a deliberate test fixture). It is meant to be
committed and to stay. NEVER flag a "kai-rename-keep" comment as a leftover
marker, tooling artifact, or something to remove, and never recommend
stripping it — removing it re-triggers the rename gate and re-blocks the
change. The only legitimate issue to raise about one is if the retention it
claims is actually wrong (the reference genuinely should have been updated),
or if the surrounding comment prose is now factually stale and should be
reworded — in which case say exactly that, and do not propose deleting the
annotation itself.

Cross-reference the diff against the session_context: changes outside what the user asked for in this session
deserve extra scrutiny and lean toward REJECT or ASK.

The <affected_files> in graph_context are downstream callers/importers of the changed code (the blast
radius) — they are NOT expected to appear in the diff. A file listed there but absent from the diff is
normal and is not a discrepancy; only flag it if a caller's behavior is actually broken by the change.

If a <verification> block is present and reports a FAILURE (build error, failing test, or timeout/hang),
you MUST NOT recommend APPROVE or FIX_THEN_APPROVE-as-clean. Execution is ground truth and overrides your
reading of the diff: if the tests are red, the change is broken even if the code looks correct to you.
Recommend FIX_THEN_APPROVE and make the failing verification the first audit issue.

Respond in this exact format and nothing else:

SUMMARY
2-3 sentences describing what the change does. Be specific about what was added, removed, or modified. Do not editorialize.

AUDIT
One of:
- CLEAN (no issues found)
- ISSUES
  1. [fixable|human] Description of issue
  2. [fixable|human] Description of issue

RECOMMENDATION
One of: APPROVE | FIX_THEN_APPROVE | REJECT | ASK
One sentence explaining why.`

	var prompt strings.Builder
	prompt.WriteString("<held_change>\n")
	fmt.Fprintf(&prompt, "  <snapshot_id>%s</snapshot_id>\n", short)
	fmt.Fprintf(&prompt, "  <verdict>%s</verdict>\n", verdict)
	fmt.Fprintf(&prompt, "  <blast_radius>%d</blast_radius>\n", blast)
	if len(reasons) > 0 {
		fmt.Fprintf(&prompt, "  <reasons>%s</reasons>\n", strings.Join(reasons, "; "))
	}
	prompt.WriteString("</held_change>\n\n<graph_context>\n")
	if len(touches) > 0 {
		fmt.Fprintf(&prompt, "  <affected_files count=\"%d\">\n", len(touches))
		for _, t := range touches {
			fmt.Fprintf(&prompt, "    %s\n", t)
		}
		prompt.WriteString("  </affected_files>\n")
	} else {
		prompt.WriteString("  (no downstream callers/importers affected at depth 1)\n")
	}
	prompt.WriteString("</graph_context>\n\n<session_context>\n")
	if strings.TrimSpace(in.SessionTurns) != "" {
		prompt.WriteString(in.SessionTurns)
		prompt.WriteString("\n")
	} else {
		prompt.WriteString("(no active session — held snapshot may be from a previous session; treat with extra caution)\n")
	}
	prompt.WriteString("</session_context>\n\n")
	if len(criteria) > 0 {
		prompt.WriteString("<acceptance_criteria>\n")
		prompt.WriteString("(What the planner defined as \"done\" — the intent. Audit the diff against EACH.)\n")
		for _, c := range criteria {
			fmt.Fprintf(&prompt, "  - %s\n", c)
		}
		prompt.WriteString("</acceptance_criteria>\n\n")
	}
	if strings.TrimSpace(in.VerifySummary) != "" {
		prompt.WriteString("<verification>\n")
		prompt.WriteString(in.VerifySummary)
		prompt.WriteString("\n</verification>\n\n")
	}
	prompt.WriteString("<diff>\n")
	prompt.WriteString(patch)
	if truncated {
		prompt.WriteString("\n... (diff truncated for review)\n")
	}
	prompt.WriteString("\n</diff>\n")

	resp, err := prov.Send(ctx, provider.Request{
		Model:     model,
		System:    system,
		MaxTokens: 800,
		OnState:   in.OnState,
		Messages: []message.Message{{
			Role:  message.RoleUser,
			Parts: []message.ContentPart{message.TextContent{Text: prompt.String()}},
		}},
	})
	if err != nil {
		return nil, err
	}
	var raw strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			raw.WriteString(t.Text)
		}
	}
	rawStr := raw.String()

	res := parseResponse(rawStr)
	res.SnapshotID = snap.ID
	res.Verdict = verdict
	res.BlastRadius = blast
	res.Reasons = reasons
	res.Touches = touches
	res.Raw = rawStr
	return res, nil
}

// parseResponse pulls SUMMARY / AUDIT / RECOMMENDATION sections out of
// the model's text output. Tolerant of formatting drift: leading
// whitespace, inconsistent indentation, missing sections all fall back
// gracefully to the raw text.
func parseResponse(resp string) *Result {
	res := &Result{}
	lines := strings.Split(resp, "\n")

	type section int
	const (
		secNone section = iota
		secSummary
		secAudit
		secRec
	)
	cur := secNone
	var summary, rec []string

	for _, line := range lines {
		trim := strings.TrimSpace(line)
		upper := strings.ToUpper(trim)
		switch {
		case upper == "SUMMARY":
			cur = secSummary
			continue
		case upper == "AUDIT":
			cur = secAudit
			continue
		case upper == "RECOMMENDATION":
			cur = secRec
			continue
		}
		switch cur {
		case secSummary:
			summary = append(summary, line)
		case secAudit:
			parseAuditLine(res, trim)
		case secRec:
			if r := parseRecLine(trim); r != "" {
				res.Recommendation = Recommendation(r)
			} else if trim != "" {
				rec = append(rec, line)
			}
		}
	}
	res.Summary = strings.TrimSpace(strings.Join(summary, "\n"))
	res.RecReason = strings.TrimSpace(strings.Join(rec, "\n"))

	// If the audit emitted no ISSUES line and no CLEAN sentinel,
	// default to clean rather than leaving an ambiguous state — the
	// model has been silent, which we read as "nothing to flag."
	if !res.AuditClean && len(res.Issues) == 0 {
		res.AuditClean = true
	}
	return res
}

func parseAuditLine(res *Result, trim string) {
	upper := strings.ToUpper(trim)
	if upper == "CLEAN" || strings.HasPrefix(upper, "CLEAN ") {
		res.AuditClean = true
		return
	}
	if upper == "ISSUES" || strings.HasPrefix(upper, "- ISSUES") {
		res.AuditClean = false
		return
	}
	// Numbered issue line: "1. [fixable] foo" or "2. [human] bar"
	// Tolerate leading "- " from a markdown bullet too.
	body := strings.TrimPrefix(trim, "- ")
	if i := strings.Index(body, "."); i > 0 && i < 4 {
		// Looks like "1. ..." — drop the prefix.
		body = strings.TrimSpace(body[i+1:])
	}
	if body == "" {
		return
	}
	fixable := false
	if strings.HasPrefix(body, "[fixable]") {
		fixable = true
		body = strings.TrimSpace(strings.TrimPrefix(body, "[fixable]"))
	} else if strings.HasPrefix(body, "[human]") {
		body = strings.TrimSpace(strings.TrimPrefix(body, "[human]"))
	} else {
		return
	}
	res.Issues = append(res.Issues, Issue{Description: body, Fixable: fixable})
	res.AuditClean = false
}

func parseRecLine(trim string) string {
	upper := strings.ToUpper(trim)
	for _, r := range []string{"APPROVE", "FIX_THEN_APPROVE", "REJECT", "ASK"} {
		if upper == r || strings.HasPrefix(upper, r+" ") || strings.HasPrefix(upper, r+":") {
			return r
		}
	}
	return ""
}

func stringList(v interface{}) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []interface{}:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
