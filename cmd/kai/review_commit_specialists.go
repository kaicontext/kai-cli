package main

// Specialist finders: focused reviewers beside the main one.
//
// On the Martian 50-PR benchmark Kai looks at the right code for nearly every
// serious bug it misses — only 1 of 44 was never proposed in any run — but 37%
// of those misses are near misses (the right lines, a weaker or inverted
// mechanism: stale denials for stale grants, missing timeouts for SSRF), and
// many of its correct statements fail its own fact-check for want of a
// concrete trigger. A bigger finder model did not help (Fable 5.1, −2.4 F1);
// neither did loosening or second-guessing the check. What the leading tools
// describe and Kai lacks is focused finders — logic, security, cross-file
// contracts — whose findings name the trigger and cite the evidence
// (kai-bench trace and research, 2026-10-20).
//
// Each specialist is the second finder's mechanism (a clone of the main
// reviewer's agent options: same tools, same prompt input) with a focus
// added to its system prompt and an output contract. Bullets that do not name
// a trigger and cite evidence are dropped before the fact-check. Every
// specialist is off unless its stage names a model.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/provider"
)

const (
	rcStageSpecLogic     = "spec_logic"
	rcStageSpecSecurity  = "spec_security"
	rcStageSpecContracts = "spec_contracts"
)

var rcSpecialistStages = []string{rcStageSpecLogic, rcStageSpecSecurity, rcStageSpecContracts}

// rcSpecialistContract is appended to every specialist's prompt.
const rcSpecialistContract = `

OUTPUT CONTRACT FOR THIS FOCUS. Report only defects inside your focus; other reviewers cover the rest. Write every ISSUES bullet in exactly this shape, on one line:
- path:line — <the mechanism: what the code does wrong> | trigger: <the concrete input, caller, state or configuration that reaches it, and why that occurs today> | evidence: path:line[, path:line] | consequence: <what goes wrong, for whom>
"evidence" names the lines that show it — the defective line and, where the defect depends on code elsewhere, that code (the caller, the sibling, the definition) — every one of which you opened. A bullet without a trigger or without evidence is discarded. If you find nothing inside your focus, leave ISSUES empty: an empty list is a correct answer.`

var rcSpecialistFocus = map[string]string{
	rcStageSpecLogic: `

YOUR FOCUS: LOGIC AND STATE. Walk every function the change adds or modifies, line by line. For each condition, comparison, loop bound, early return and branch, ask: what input makes it take the wrong path, and what happens then? Check specifically: an inverted or wrong boolean operator (&& vs ||, a missing or extra !, < vs <=); the same variable checked twice where a different one was meant; a value used before it is set or after it is reset; null, undefined, empty, zero and negative inputs the signature allows; asymmetric handling of two cases that should be treated alike (a cache trusted for one answer but not the other, one branch normalizing input and its sibling not); state written or cached on an error path; order-dependent updates (read-modify-write, check-then-act); off-by-one in ranges and pagination; wrong unit, sign or default. Prefer the precise mechanism over a general worry: name the input and the wrong result.`,
	rcStageSpecSecurity: `

YOUR FOCUS: SECURITY. List every value an attacker, another tenant or another user can control in this change: request parameters, headers, cookies, webhook and API payloads, URLs, uploaded or fetched content, file names, user-entered names and emails, configuration a lower-privileged user can set. Follow each one to where it is used. Report a defect only for a reachable source-to-sink path: SQL or query built from it (injection); a server-side fetch of a URL it controls (SSRF, file:// or internal hosts); HTML, JS, templates or postMessage built from it (XSS, origin checks); authorization or ownership checks it can bypass (missing tenant or org scoping, a check on the wrong object, case or Unicode variants that compare differently, a guard present on one path and missing on its sibling); redirects; headers that disable protection (framing, CORS); secrets or tokens logged, returned or compared non-constant-time; deserialization of untrusted data. State the attacker, the input they send and what they gain.`,
	rcStageSpecContracts: `

YOUR FOCUS: CROSS-FILE CONTRACTS. For every signature, return type or shape, enum value, feature flag, configuration key, database or API schema, event, and behaviour this change alters, find what depends on it outside the changed lines — callers, implementations of the same interface, sibling handlers or adapters doing the same job, serializers, tests, documentation that code reads — using kai_callers, kai_dependents and kai_grep. Report what was not updated or now disagrees: a caller still passing the old arguments or reading the old shape; one implementation or adapter that skips the shared helper or guard the others use; an interface method added but not implemented everywhere; a flag checked under one name and set under another, or gating the wrong version; a value produced in one unit or format and consumed in another; a test that asserts the old behaviour or cannot fail. Cite both sides: the changed line and the code that disagrees with it.`,
}

// rcSpecialistBulletRe accepts a bullet that keeps the output contract.
var rcSpecialistBulletRe = regexp.MustCompile(`(?i)\|\s*trigger:\s*\S.*\|\s*evidence:\s*\S`)

type rcSpecialistRun struct {
	stage string
	sf    *rcSecondFinder
}

// rcStartSpecialists launches every specialist whose stage names a model.
func rcStartSpecialists(runCtx, concludeCtx context.Context, opts agent.Options, prov provider.Provider) []rcSpecialistRun {
	var runs []rcSpecialistRun
	for _, stage := range rcSpecialistStages {
		model := rcEnsembleModel(stage)
		if model == "" {
			continue
		}
		ctx, cancel := context.WithCancel(runCtx)
		spec := opts
		spec.Model = model
		spec.ReasoningEffort = rcStageEffort(stage)
		spec.System = opts.System + rcSpecialistFocus[stage] + rcSpecialistContract
		spec.SessionStore = nil
		spec.SessionID = ""
		spec.TaskName = "review-commit-" + stage
		label := stage
		spec.Hooks = agent.Hooks{OnToolCall: func(name, inputJSON string) {
			fmt.Fprintf(os.Stderr, "  [%s] → %s %s\n", label, name, rcOneLine(inputJSON, 70))
		}}
		fmt.Fprintf(os.Stderr, "  specialist %s: %s (effort %s)\n", stage, model, rcEffortLabel(spec.ReasoningEffort))
		sf := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: cancel, model: model}
		go func(sf *rcSecondFinder, spec agent.Options) {
			started := time.Now()
			out := rcFinderResult{Model: spec.Model}
			res, err := agent.Run(ctx, spec)
			if err != nil {
				out.Err = err
			} else if res != nil {
				raw := rcRestoreCodaMarker(strings.TrimSpace(res.FinalText))
				if rcNeedsConclusion(raw) {
					if c, _ := rcConcludeWithModel(concludeCtx, prov, spec.Model, res.Transcript); c != "" {
						raw = rcRestoreCodaMarker(c)
					}
				}
				if rcUsableCoda(raw) {
					out.Raw = raw
					out.Issues = rcIssuesOf(raw)
				}
			}
			out.Elapsed = time.Since(started)
			sf.ch <- out
		}(sf, spec)
		runs = append(runs, rcSpecialistRun{stage: stage, sf: sf})
	}
	return runs
}

func rcEffortLabel(e string) string {
	if e == "" {
		return "off"
	}
	return e
}

// rcStopSpecialists cancels every specialist still running.
func rcStopSpecialists(runs []rcSpecialistRun) {
	for _, r := range runs {
		rcStopSecondFinder(r.sf)
	}
}

// rcAwaitSpecialists waits for every specialist under one shared deadline
// (rcSecondFinderGrace past the main finder) and returns the bullets that keep
// the output contract.
func rcAwaitSpecialists(runs []rcSpecialistRun) []string {
	if len(runs) == 0 {
		return nil
	}
	deadline := time.After(rcSecondFinderGrace)
	var kept []string
	for _, run := range runs {
		var r rcFinderResult
		select {
		case r = <-run.sf.ch:
		case <-deadline:
			fmt.Fprintf(os.Stderr, "  specialist %s still running %s after the main finder — continuing without it\n", run.stage, rcSecondFinderGrace)
			rcStopSpecialists(runs)
			return kept
		}
		run.sf.cancel()
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "  specialist %s failed after %s: %v\n", run.stage, r.Elapsed.Round(time.Second), r.Err)
			continue
		}
		good, dropped := rcSpecialistContractFilter(r.Issues)
		fmt.Fprintf(os.Stderr, "  specialist %s: %d issue(s), %d kept, %d without trigger/evidence dropped, in %s\n",
			run.stage, len(r.Issues), len(good), dropped, r.Elapsed.Round(time.Second))
		for _, g := range good {
			kept = append(kept, g)
			fmt.Fprintf(os.Stderr, "  specialist %s proposed: %s\n", run.stage, rcOneLine(g, 160))
		}
	}
	return kept
}

// rcSpecialistContractFilter keeps the bullets that name a trigger and cite
// evidence.
func rcSpecialistContractFilter(issues []string) (kept []string, dropped int) {
	for _, is := range issues {
		if rcSpecialistBulletRe.MatchString(is) {
			kept = append(kept, is)
		} else {
			dropped++
		}
	}
	return kept, dropped
}

// rcWithSpecialists merges the specialists' bullets into the draft the way the
// second finder's are merged.
func rcWithSpecialists(raw string, issues []string) string {
	if len(issues) == 0 || !rcUsableCoda(raw) {
		return raw
	}
	before := len(rcIssuesOf(raw))
	merged := rcDraftWithSweep(raw, issues)
	fmt.Fprintf(os.Stderr, "  specialists: %d added to the draft's %d\n", len(rcIssuesOf(merged))-before, before)
	return merged
}
