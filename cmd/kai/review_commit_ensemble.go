package main

// The ensemble around the grounded review: more than one finder, then a
// choice of what to publish.
//
// One agent and one sweep on one model find a different slice of a change's
// defects on every run: two benchmark runs of the same configuration matched
// about 90 known defects each and only 68 of them in common. So the review
// proposes from more than one place — a second finder agent on another model
// family (rcStartSecondFinder) and a second sweep (rcStartSweep) — and the
// fact-check settles which proposals are true, as before.
//
// True is not the same as worth publishing. A fact-check that confirms every
// true statement publishes typos in test names beside a data race, and the
// more finders propose, the more of those it confirms. The selection step
// (rcRankSupported) reads the confirmed defects together and keeps the ones a
// careful maintainer would want fixed, most important first, a few per change.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// The ensemble stages' defaults. Each can be set by the review profile or by
// its environment variable; "off" turns the stage off.
var rcEnsembleDefaults = map[string]struct{ modelEnv, model, effortEnv, effort string }{
	// Another model family from the main finder, so the two miss different
	// things. Low effort: it runs a full agent loop. Tried first: GPT-5.5
	// (about $2 a review with the fact-check), Kimi K2.6 (a minute a turn),
	// Gemini 3.5 Flash (two tool calls, no issues).
	rcStageFinder2: {"KAI_FINDER2_MODEL", "openai/gpt-5.6-sol", "KAI_FINDER2_EFFORT", "low"},
	// The second sweep reads the same hunks with a different model.
	rcStageSweep2: {"KAI_SWEEP2_MODEL", "z-ai/glm-5.2", "KAI_SWEEP2_EFFORT", ""},
	// One call per review, so the strongest judgement available.
	rcStageRank: {"KAI_RANK_MODEL", "anthropic/claude-opus-5.5", "KAI_RANK_EFFORT", "medium"},
	// Wording, not judgement: one short call that rewrites what is already
	// settled, so a strong writer with no reasoning effort.
	rcStageHumanize: {"KAI_HUMANIZE_MODEL", "anthropic/claude-sonnet-5.5", "KAI_HUMANIZE_EFFORT", ""},
	// Specialist finders (review_commit_specialists.go): off unless a profile
	// or environment names a model.
	rcStageSpecLogic:     {"KAI_SPEC_LOGIC_MODEL", "off", "KAI_SPEC_LOGIC_EFFORT", ""},
	rcStageSpecSecurity:  {"KAI_SPEC_SECURITY_MODEL", "off", "KAI_SPEC_SECURITY_EFFORT", ""},
	rcStageSpecContracts: {"KAI_SPEC_CONTRACTS_MODEL", "off", "KAI_SPEC_CONTRACTS_EFFORT", ""},
}

// rcEnsembleModel is an ensemble stage's model: the profile's, else its
// environment variable, else the default. "" means the stage is off.
func rcEnsembleModel(stage string) string {
	d := rcEnsembleDefaults[stage]
	m := d.model
	// Set but empty falls back to the default, like the other stage
	// variables; "off" or "none" turns the stage off.
	if v := strings.TrimSpace(os.Getenv(d.modelEnv)); v != "" {
		m = v
	}
	m = rcStageModel(stage, m)
	if m == rcEffortOff || m == "none" {
		return ""
	}
	return m
}

// rcEnsembleEffort is an ensemble stage's reasoning effort when the profile
// does not set one (rcStageEffort consults the profile first).
func rcEnsembleEffort(stage string) string {
	d := rcEnsembleDefaults[stage]
	e := d.effort
	if v, ok := os.LookupEnv(d.effortEnv); ok {
		e = strings.TrimSpace(v)
	}
	if e == rcEffortOff {
		return ""
	}
	return e
}

// rcSecondFinderGrace is how long the review waits for the second finder
// once the main finder is done. The second finder is extra coverage, not the
// review: a slow model must not hold every review to its hard deadline.
var rcSecondFinderGrace = 5 * time.Minute

// rcSecondFinder is a running second finder.
type rcSecondFinder struct {
	ch     chan rcFinderResult
	cancel context.CancelFunc
	model  string
}

// rcFinderResult is what the second finder hands back.
type rcFinderResult struct {
	Model   string
	Raw     string   // its review, when it ended with a usable coda
	Issues  []string // that review's ISSUES
	Elapsed time.Duration
	Err     error
}

// rcStartSecondFinder runs a second review agent beside the first, on the
// finder2 model, with the same prompt, tools and deadline. It keeps no session
// (so it cannot contend with the first agent's) and gets no coverage gate; if
// it ends without a coda, one is written from its transcript. A nil channel
// means the stage is off or would duplicate the main finder.
func rcStartSecondFinder(runCtx, concludeCtx context.Context, opts agent.Options, prov provider.Provider) *rcSecondFinder {
	model := rcEnsembleModel(rcStageFinder2)
	if model == "" || model == opts.Model {
		return nil
	}
	runCtx, cancel := context.WithCancel(runCtx)
	second := opts
	second.Model = model
	second.ReasoningEffort = rcStageEffort(rcStageFinder2)
	second.SessionStore = nil
	second.SessionID = ""
	second.TaskName = "review-commit-finder2"
	second.Hooks = agent.Hooks{OnToolCall: func(name, inputJSON string) {
		fmt.Fprintf(os.Stderr, "  [finder2] → %s %s\n", name, rcOneLine(inputJSON, 70))
	}}
	effort := second.ReasoningEffort
	if effort == "" {
		effort = "off"
	}
	fmt.Fprintf(os.Stderr, "  second finder: %s (effort %s)\n", model, effort)
	sf := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: cancel, model: model}
	ch := sf.ch
	go func() {
		started := time.Now()
		out := rcFinderResult{Model: model}
		res, err := agent.Run(runCtx, second)
		if err != nil {
			out.Err = err
		} else if res != nil {
			raw := rcRestoreCodaMarker(strings.TrimSpace(res.FinalText))
			if rcNeedsConclusion(raw) {
				if c, _ := rcConcludeWithModel(concludeCtx, prov, model, res.Transcript); c != "" {
					raw = rcRestoreCodaMarker(c)
				}
			}
			if rcUsableCoda(raw) {
				out.Raw = raw
				out.Issues = rcIssuesOf(raw)
			}
		}
		out.Elapsed = time.Since(started)
		ch <- out
	}()
	return sf
}

// rcStopSecondFinder ends a second finder whose review will not be used.
func rcStopSecondFinder(sf *rcSecondFinder) {
	if sf != nil {
		sf.cancel()
	}
}

// rcAwaitSecondFinder waits for the second finder, at most
// rcSecondFinderGrace past the main finder; a finder still running then is
// cancelled and the review goes on without it. Nil yields nothing.
func rcAwaitSecondFinder(sf *rcSecondFinder) rcFinderResult {
	if sf == nil {
		return rcFinderResult{}
	}
	var r rcFinderResult
	select {
	case r = <-sf.ch:
	case <-time.After(rcSecondFinderGrace):
		sf.cancel()
		fmt.Fprintf(os.Stderr, "  second finder (%s) still running %s after the main finder — continuing without it\n", sf.model, rcSecondFinderGrace)
		return rcFinderResult{Model: sf.model}
	}
	sf.cancel()
	switch {
	case r.Err != nil:
		fmt.Fprintf(os.Stderr, "  second finder (%s) failed after %s: %v\n", r.Model, r.Elapsed.Round(time.Second), r.Err)
	case r.Raw == "":
		fmt.Fprintf(os.Stderr, "  second finder (%s) ended without a review after %s\n", r.Model, r.Elapsed.Round(time.Second))
	default:
		fmt.Fprintf(os.Stderr, "  second finder (%s): %d issue(s) in %s\n", r.Model, len(r.Issues), r.Elapsed.Round(time.Second))
	}
	return r
}

// rcWithSecondFinder merges the second finder's issues into the first
// finder's draft. When the first ended without a usable review and the second
// has one, the second's review stands in for it.
func rcWithSecondFinder(raw string, r rcFinderResult) string {
	switch {
	case !rcUsableCoda(raw) && r.Raw != "":
		fmt.Fprintf(os.Stderr, "  the main finder ended without a review; the second finder's review stands in\n")
		return r.Raw
	case len(r.Issues) > 0 && rcUsableCoda(raw):
		before := len(rcIssuesOf(raw))
		merged := rcDraftWithSweep(raw, r.Issues)
		fmt.Fprintf(os.Stderr, "  second finder: %d added to the main finder's %d\n", len(rcIssuesOf(merged))-before, before)
		return merged
	}
	return raw
}

// rcRankCap is how many confirmed defects a review publishes at most, by the
// number of files the change touches. A small change rarely has more than a
// few defects worth a maintainer's attention; past the cap, a review stops
// being read.
func rcRankCap(changedFiles int) int {
	switch {
	case changedFiles <= 3:
		return 3
	case changedFiles <= 10:
		return 4
	case changedFiles <= 25:
		return 5
	}
	return 6
}

const (
	rcRankTimeout      = 4 * time.Minute
	rcRankMaxDiffBytes = 60 * 1024
)

const rcRankSystem = `You choose which confirmed defects a code review publishes.

A fact-check has already confirmed that each defect below is true of the change. Your job is different: order them by how much they matter to a maintainer of this repository, fold repeated statements of the same defect, and say which are minor.

Publish every defect that is not minor and not skipped, most important first. Order first the ones that:
- produces a wrong result, crashes, or throws where the surrounding code expects success;
- breaks a contract with callers, a caller that was not updated, or a value of the wrong kind or unit;
- opens a security hole: missing authorization, injection, a leaked secret, an unsafe default;
- loses or corrupts data, or leaves state half-written;
- races, deadlocks, leaks a resource, or can hang;
- shows users wrong text, or a test that asserts the wrong value or cannot fail.

Put a real defect that is small under "minor" instead of skipping it. Minor defects are published after the others, so a maintainer still sees them. They include:
- a metric name, tag key, log field or event key that something reads (a dashboard, an alert, a parser) and that differs from what sibling code uses for the same thing. That splits or loses data; it is not naming;
- a test step that does not do what it says: a wait that is a no-op because the test patched it, setup that never runs, a check that never executes. The test then does not exercise what it claims;
- an edge case on unusual but allowed input.

Skip only:
- a second statement of a defect you publish: the same code and the same mechanism, said again (keep the clearest statement). Two defects in the same file or function, or with similar wording, are not duplicates unless fixing one fixes the other;
- a point with no effect on what the program does: formatting, comment or log-message wording, the name of a local variable.

Do not re-judge what the fact-check settled. It has already decided that each defect is real, reachable today, introduced or touched by this change, and not the author's stated intent. So never skip a defect because its trigger seems unlikely or latent, because no current caller seems to reach it, because it might be pre-existing, because it might be intended, or because its impact is small — order it lower, or put it under "minor". A Critical or High consequence (a crash, a security hole, lost data, a check on the wrong variable) is never minor.

When unsure between two, prefer the one with the more concrete failure. Answer with JSON only, no prose before or after:
{"publish": [ids, most important first], "minor": [ids, most important first], "skip": [{"id": id, "why": "one short phrase"}]}
Every id appears exactly once, in publish, minor or skip.`

var rcRankJSON = regexp.MustCompile(`(?s)\{.*\}`)

// rcRankSupported chooses which supported allegations the review publishes.
// The chosen ones come first, in order of importance, at most rcRankCap of
// them; the rest become refuted with the selection's reason, so they are kept
// in the record but not published. A review with a confirmed defect always
// publishes at least one. Any failure keeps everything the fact-check
// supported, as before this step existed.
func rcRankSupported(ctx context.Context, prov provider.Provider, intent, diff string, changedFiles int, res *rcChallengeResult) {
	model := rcEnsembleModel(rcStageRank)
	if model == "" || res == nil {
		return
	}
	var supported []int
	for i, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			supported = append(supported, i)
		}
	}
	// Recovered findings: ones the fact-check could not disprove but could not
	// fully confirm (unresolved). KAI_REVIEW_GATE_RECOVER offers them to the
	// selection as UNCONFIRMED, so a plainly-real defect the check was merely
	// unsure of is not lost. The selection runs only when rank is configured,
	// so recovered findings always pass through its judgement and the cap —
	// never published unfiltered. Default off: the pool is supported-only, as
	// before.
	var recovered []int
	if rcGateRecover() {
		for i, a := range res.Allegations {
			if a.Status == rcStatusUnresolved {
				recovered = append(recovered, i)
			}
		}
	}
	if len(supported)+len(recovered) < 2 {
		return
	}
	limit := rcRankCap(changedFiles)

	var b strings.Builder
	b.WriteString("WHAT THE CHANGE IS FOR:\n")
	b.WriteString(strings.TrimSpace(intent))
	b.WriteString("\n\nCONFIRMED DEFECTS:\n")
	for _, i := range supported {
		a := res.Allegations[i]
		fmt.Fprintf(&b, "\n[%d] %s\n", a.ID, a.Issue)
		if a.Finding != "" {
			fmt.Fprintf(&b, "    what the check found: %s\n", rcOneLine(a.Finding, 700))
		}
		if a.Reason != "" {
			fmt.Fprintf(&b, "    why it is confirmed: %s\n", rcOneLine(a.Reason, 500))
		}
	}
	if len(recovered) > 0 {
		b.WriteString("\nUNCONFIRMED DEFECTS (the check could neither confirm nor disprove these — publish one ONLY if the diff plainly shows it is a real defect; prefer the confirmed defects above):\n")
		for _, i := range recovered {
			a := res.Allegations[i]
			fmt.Fprintf(&b, "\n[%d] %s\n", a.ID, a.Issue)
			if a.Reason != "" {
				fmt.Fprintf(&b, "    what the check could not settle: %s\n", rcOneLine(a.Reason, 500))
			}
		}
	}
	d := diff
	if len(d) > rcRankMaxDiffBytes {
		cut := rcRankMaxDiffBytes
		for cut > 0 && !utf8.RuneStart(d[cut]) {
			cut--
		}
		d = d[:cut] + "\n... (diff truncated)"
	}
	b.WriteString("\nDIFF:\n")
	b.WriteString(d)

	effort := rcStageEffort(rcStageRank)
	cctx, cancel := context.WithTimeout(ctx, rcRankTimeout)
	defer cancel()
	started := time.Now()
	resp, err := prov.Send(cctx, provider.Request{
		Model:           model,
		System:          rcRankSystem,
		MaxTokens:       rcTokensFor(2000, effort),
		ReasoningEffort: effort,
		Messages:        []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: b.String()}}}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  selection (%s) failed: %v — publishing every confirmed defect\n", model, err)
		return
	}
	var text strings.Builder
	for _, part := range resp.Parts {
		if t, ok := part.(message.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	ans, perr := rcParseRank(text.String())
	if perr != nil {
		fmt.Fprintf(os.Stderr, "  selection (%s) answered unusably (%v) — publishing every confirmed defect\n", model, perr)
		return
	}
	// The answer itself, by allegation id, for the log (main list, minor list
	// or overflow past the cap).
	fmt.Fprintf(os.Stderr, "  selection answer: cap=%d publish=%v minor=%v skip=%v\n", limit, ans.Publish, ans.Minor, rcSortedKeys(ans.Skip))
	// Promote the recovered findings ONLY now, after a usable selection answer.
	// On any early return above (selection off, pool too small, model error,
	// unusable answer) they stay unresolved and are never published — the
	// selection must have actually judged them. They are marked recovered so
	// rcApplyRank's "never publish nothing" fallback, which is for confirmed
	// defects, never force-publishes one the selection skipped.
	for _, i := range recovered {
		if res.Allegations[i].Finding == "" {
			res.Allegations[i].Finding = rcRecoveredFinding(res.Allegations[i].Issue)
		}
		res.Allegations[i].Status = rcStatusSupported
		res.Allegations[i].recovered = true
	}
	kept, dropped := rcApplyRank(res, ans, limit)
	fmt.Fprintf(os.Stderr, "  selection (%s, %s): %d of %d defect(s) published (%d confirmed + %d unconfirmed in the pool), %d held back\n",
		model, time.Since(started).Round(time.Second), kept, len(supported)+len(recovered), len(supported), len(recovered), dropped)
	rcFinalize(res)
}

// rcGateRecover reports whether the selection should also consider the
// fact-check's unresolved findings (KAI_REVIEW_GATE_RECOVER=1). Default off.
func rcGateRecover() bool { return os.Getenv("KAI_REVIEW_GATE_RECOVER") == "1" }

// rcRecoveredFinding turns an unresolved allegation's issue bullet into a
// published finding: the bullet without its trailing severity marker.
func rcRecoveredFinding(issue string) string {
	if i := strings.LastIndex(issue, " (severity:"); i >= 0 {
		return strings.TrimSpace(issue[:i])
	}
	return strings.TrimSpace(issue)
}

// rcRankAnswer is the selection's answer: the defects to publish first, the
// small ones to publish after them, and why the rest are left out.
type rcRankAnswer struct {
	Publish []int
	Minor   []int
	Skip    map[int]string
}

// rcRankMinorCap is how many minor defects a review publishes after the
// others. Small and real is still worth a line, but a review that lists every
// one of them stops being read.
const rcRankMinorCap = 3

// rcSortedKeys lists a skip map's ids in order, for the log.
func rcSortedKeys(m map[int]string) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// rcParseRank reads the selection's answer.
func rcParseRank(text string) (rcRankAnswer, error) {
	m := rcRankJSON.FindString(text)
	if m == "" {
		return rcRankAnswer{}, fmt.Errorf("no JSON object")
	}
	var ans struct {
		Publish []int `json:"publish"`
		Minor   []int `json:"minor"`
		Skip    []struct {
			ID  int    `json:"id"`
			Why string `json:"why"`
		} `json:"skip"`
	}
	if err := json.Unmarshal([]byte(m), &ans); err != nil {
		return rcRankAnswer{}, err
	}
	skip := map[int]string{}
	for _, s := range ans.Skip {
		skip[s.ID] = strings.TrimSpace(s.Why)
	}
	return rcRankAnswer{Publish: ans.Publish, Minor: ans.Minor, Skip: skip}, nil
}

// rcSelectionSkipPrefix starts the reason of a confirmed defect the selection
// step left out. The defect is still true, so nothing that reads refutations
// as "no longer holds" may count it (rcEarlierStatuses, rcCountAllegations).
const rcSelectionSkipPrefix = "Not published by the selection step: "

// rcApplyRank publishes the chosen allegations, in the chosen order and at
// most limit of them, then up to rcRankMinorCap minor ones, and refutes the
// other supported ones with the reason. Unsupported allegations keep their
// place after the published ones.
func rcApplyRank(res *rcChallengeResult, ans rcRankAnswer, limit int) (kept, dropped int) {
	byID := map[int]int{}
	for i, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			byID[a.ID] = i
		}
	}
	var order []int
	seen := map[int]bool{}
	for _, id := range ans.Publish {
		if i, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			order = append(order, i)
		}
	}
	// Past the main cap, a defect the selection chose to publish joins the
	// minor ones instead of being dropped: on sentry#93824 the cap of 4 cut a
	// real test defect ("outside the 4 most important") that a reviewer with
	// room for one more line would have kept.
	var overflow []int
	if len(order) > limit {
		for _, i := range order[limit:] {
			overflow = append(overflow, res.Allegations[i].ID)
			seen[res.Allegations[i].ID] = false
		}
		order = order[:limit]
	}
	// Minor defects come after the main ones, with a cap of their own.
	minor := 0
	for _, id := range append(overflow, ans.Minor...) {
		if minor == rcRankMinorCap {
			break
		}
		if i, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			order = append(order, i)
			minor++
		}
	}
	if len(order) == 0 {
		// Never publish nothing when the check CONFIRMED something: keep the
		// first confirmed defect in the draft's own order. A recovered
		// (unconfirmed) finding is never eligible here — if the pool was all
		// recovered and the selection chose none, the review publishes nothing.
		for i, a := range res.Allegations {
			if a.Status == rcStatusSupported && !a.recovered {
				order = append(order, i)
				break
			}
		}
	}
	chosen := map[int]bool{}
	for _, i := range order {
		chosen[i] = true
	}
	var out []rcAllegationResult
	for _, i := range order {
		out = append(out, res.Allegations[i])
	}
	for i, a := range res.Allegations {
		if chosen[i] {
			continue
		}
		if a.Status == rcStatusSupported {
			why := ans.Skip[a.ID]
			if why == "" {
				why = fmt.Sprintf("outside the %d most important defects of this change", limit)
			}
			a.Status = rcStatusRefuted
			a.Reason = rcSelectionSkipPrefix + why
			dropped++
		}
		out = append(out, a)
	}
	res.Allegations = out
	return len(order), dropped
}

// rcDefectsOnlyProse removes the parts of the published review that are not
// defects: the list of claims the check could not settle and the reviewer's
// limitations. Both stay in the challenge record; on the pull request, every
// sentence reads as a claim about the code, so only defects belong there.
func rcDefectsOnlyProse(prose string) string {
	for _, heading := range []string{"## Could not verify", "## Limitations"} {
		for from := 0; ; {
			// Only a heading at a line start is a section; a finding that
			// quotes the heading mid-line is skipped, not a stop (Kai's
			// review of #188).
			j := strings.Index(prose[from:], heading)
			if j < 0 {
				break
			}
			i := from + j
			if i > 0 && prose[i-1] != '\n' {
				from = i + len(heading)
				continue
			}
			end := strings.Index(prose[i+len(heading):], "\n## ")
			if end < 0 {
				prose = prose[:i]
			} else {
				prose = prose[:i] + prose[i+len(heading)+end+1:]
			}
		}
	}
	return strings.TrimRight(prose, "\n") + "\n"
}
