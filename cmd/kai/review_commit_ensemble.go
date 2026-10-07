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
	"strings"
	"time"

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
}

// rcEnsembleModel is an ensemble stage's model: the profile's, else its
// environment variable, else the default. "" means the stage is off.
func rcEnsembleModel(stage string) string {
	d := rcEnsembleDefaults[stage]
	m := d.model
	if v, ok := os.LookupEnv(d.modelEnv); ok {
		m = strings.TrimSpace(v)
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
	runCtx, cancel := context.WithCancel(rcUsageStage(runCtx, "finder2"))
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
				if c := rcConcludeFromTranscript(concludeCtx, prov, model, res.Transcript); c != "" {
					raw = rcRestoreCodaMarker(c)
				}
			}
			if rcUsableCoda(raw) {
				decoded, _ := rcDecodeReview(raw)
				out.Raw = decoded.draft()
				out.Issues = decoded.Findings
			}
		}
		out.Elapsed = time.Since(started)
		ch <- out
	}()
	return sf
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

A fact-check has already confirmed that each defect below is true of the change. Your job is different: decide which ones are worth a maintainer's attention on this pull request, and order them by how much they matter.

Publish a defect when a careful maintainer of this repository, shown it, would want it fixed before or right after merging, because on inputs, callers, configuration or timing that occur it:
- produces a wrong result, crashes, or throws where the surrounding code expects success;
- breaks a contract with callers, a caller that was not updated, or a value of the wrong kind or unit;
- opens a security hole: missing authorization, injection, a leaked secret, an unsafe default;
- loses or corrupts data, or leaves state half-written;
- races, deadlocks, leaks a resource, or can hang;
- shows users wrong text, or a test that asserts the wrong value or cannot fail.

Do not publish:
- a second defect with the same root cause as one you publish (keep the clearest statement of it);
- critique of test style, naming, structure or coverage, unless the test asserts the wrong thing or cannot fail;
- cosmetic points: formatting, comments, naming, log wording, dead code with no effect;
- hardening advice ("consider validating", "add a timeout") without a concrete input that fails today;
- anything whose trigger needs a future code change;
- a product or policy choice the author made on purpose.

When unsure between two, prefer the one with the more concrete failure. Answer with JSON only, no prose before or after:
{"publish": [ids, most important first], "skip": [{"id": id, "why": "one short phrase"}]}
Every id appears exactly once, in publish or in skip.`

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
	if len(supported) < 2 {
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
	d := diff
	if len(d) > rcRankMaxDiffBytes {
		d = d[:rcRankMaxDiffBytes] + "\n... (diff truncated)"
	}
	b.WriteString("\nDIFF:\n")
	b.WriteString(d)

	effort := rcStageEffort(rcStageRank)
	cctx, cancel := context.WithTimeout(ctx, rcRankTimeout)
	defer cancel()
	started := time.Now()
	resp, err := prov.Send(rcUsageStage(cctx, "rank"), provider.Request{
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
	publish, skip, perr := rcParseRank(text.String())
	if perr != nil {
		fmt.Fprintf(os.Stderr, "  selection (%s) answered unusably (%v) — publishing every confirmed defect\n", model, perr)
		return
	}
	kept, dropped := rcApplyRank(res, publish, skip, limit)
	fmt.Fprintf(os.Stderr, "  selection (%s, %s): %d of %d confirmed defect(s) published, %d held back\n",
		model, time.Since(started).Round(time.Second), kept, len(supported), dropped)
	rcFinalize(res)
}

// rcParseRank reads the selection's answer.
func rcParseRank(text string) (publish []int, skip map[int]string, err error) {
	m := rcRankJSON.FindString(text)
	if m == "" {
		return nil, nil, fmt.Errorf("no JSON object")
	}
	var ans struct {
		Publish []int `json:"publish"`
		Skip    []struct {
			ID  int    `json:"id"`
			Why string `json:"why"`
		} `json:"skip"`
	}
	if err := json.Unmarshal([]byte(m), &ans); err != nil {
		return nil, nil, err
	}
	skip = map[int]string{}
	for _, s := range ans.Skip {
		skip[s.ID] = strings.TrimSpace(s.Why)
	}
	return ans.Publish, skip, nil
}

// rcApplyRank publishes the chosen allegations, in the chosen order and at
// most limit of them, and refutes the other supported ones with the reason.
// Unsupported allegations keep their place after the published ones.
func rcApplyRank(res *rcChallengeResult, publish []int, skip map[int]string, limit int) (kept, dropped int) {
	byID := map[int]int{}
	for i, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			byID[a.ID] = i
		}
	}
	var order []int
	seen := map[int]bool{}
	for _, id := range publish {
		if i, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			order = append(order, i)
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
	if len(order) > limit {
		order = order[:limit]
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
			why := skip[a.ID]
			if why == "" {
				why = fmt.Sprintf("outside the %d most important defects of this change", limit)
			}
			a.Status = rcStatusRefuted
			a.Reason = "Not published by the selection step: " + why
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
		for {
			i := strings.Index(prose, heading)
			if i < 0 || (i > 0 && prose[i-1] != '\n') {
				break
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
