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

const rcRankSystem = `You choose which confirmed defects a code review publishes, and in what order.

A fact-check has settled that each item is true, reachable, part of this change and not the author's stated intent; do not re-judge that. Order the items by what matters to a maintainer, fold repeats, and put each in publish, minor or skip.

PUBLISH, most important first: a wrong result, crash, or exception where callers expect success; a security hole; lost or half-written data; a broken contract (a caller not updated, a wrong kind or unit, a check on the wrong variable); a race, deadlock, leak or hang; wrong text shown to users; a test that asserts the wrong value or cannot fail. A Critical or High consequence is never minor. A rare trigger, no current caller, or a possibly pre-existing cause is a reason to order lower, never to skip.

MINOR, published after the others, small behaviour defects before nits:
- an edge case on unusual but allowed input; a wrong error status (a 500 where a 4xx or an error page belongs); a value stale until the next reload or cache expiry;
- a metric name, tag key, log field or event key that differs from what sibling code uses for the same thing;
- a test that does not exercise what it claims: a wait the test patched into a no-op, setup that never runs, a request the code does not serve, a name or docstring that states a different input or result than the body;
- ` + rcNitDefinition + `

SKIP only:
- a repeat of the same code and mechanism (keep the clearest); defects are repeats only if one fix fixes both;
- formatting, a local variable's name, rewording a comment that is still accurate, a choice between two correct styles;
- hygiene that cannot affect running code: docs, changelog or sample config not updated, a migration without a down path, a stray file in the commit, test cleanup order, missing coverage, a problem the author already marks TODO or FIXME;
- a failure whose only trigger lies outside the program and its inputs: an install path or host name containing some word, an operator reconfiguring a running system.

Answer with JSON only:
{"publish": [ids, most important first], "minor": [ids, most important first], "skip": [{"id": id, "why": "one short phrase; a repeat starts: repeat of <id>"}]}
Every id appears exactly once.`

var rcRankJSON = regexp.MustCompile(`(?s)\{.*\}`)

// rcRankSupported chooses which supported allegations the review publishes.
// The chosen ones come first, in order of importance, at most rcRankCap of
// them; the rest become refuted with the selection's reason, so they are kept
// in the record but not published. A review with a confirmed defect always
// publishes at least one. Any failure keeps everything the fact-check
// supported, as before this step existed. Reports whether the selection ran.
func rcRankSupported(ctx context.Context, prov provider.Provider, intent, diff string, changedFiles int, res *rcChallengeResult) bool {
	model := rcEnsembleModel(rcStageRank)
	if model == "" || res == nil {
		return false
	}
	var supported []int
	for i, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			supported = append(supported, i)
		}
	}
	if len(supported) < 2 {
		return false
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
		return false
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
		return false
	}
	// The answer itself, by allegation id, so a published defect can be traced
	// to the main list, the minor list or the overflow past the cap: the
	// bundle keeps only the outcome, which cannot tell them apart.
	fmt.Fprintf(os.Stderr, "  selection answer: cap=%d publish=%v minor=%v skip=%v\n", limit, ans.Publish, ans.Minor, rcSortedKeys(ans.Skip))
	kept, dropped := rcApplyRank(res, ans, limit)
	fmt.Fprintf(os.Stderr, "  selection (%s, %s): %d of %d confirmed defect(s) published, %d held back\n",
		model, time.Since(started).Round(time.Second), kept, len(supported), dropped)
	rcFinalize(res)
	return true
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
		// Never publish nothing when the check confirmed something: keep the
		// first confirmed defect in the draft's own order.
		for i, a := range res.Allegations {
			if a.Status == rcStatusSupported {
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
// defects: the list of claims the check could not settle, the reviewer's
// limitations and its decisions. All stay in the challenge record; on the
// pull request, every sentence reads as a claim about the code, so only
// defects belong there.
func rcDefectsOnlyProse(prose string) string {
	for _, heading := range []string{"## Could not verify", "## Limitations", "## Decisions"} {
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
