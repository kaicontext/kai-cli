package autofix

// baseline.go makes the deterministic gate fair: it compares the test result
// AFTER the fix against a baseline taken BEFORE it, so a suite that was
// already red (failures unrelated to the issue) doesn't sink an otherwise-good
// fix. Only failures the change *introduces* should block readiness.

import (
	"regexp"
	"sort"
	"strings"

	"github.com/kaicontext/kai-engine/contract"
)

// failRe matches Go's two failure shapes in test output:
//
//	--- FAIL: TestName (0.00s)
//	FAIL\tgithub.com/org/repo/pkg\t0.4s
//
// Either identifies what's red; together they're a robust fingerprint of a
// failing suite without needing to model every test runner.
var failRe = regexp.MustCompile(`(?m)^(?:--- FAIL: (\S+)|FAIL\s+(\S+))`)

// ExtractFailures pulls a sorted, de-duplicated set of failure identifiers
// (test names and failing package paths) out of a verify summary/output.
// Returns nil when it recognizes nothing — the caller then falls back to the
// raw pass/fail bool rather than guessing.
func ExtractFailures(summary string) []string {
	seen := map[string]bool{}
	for _, m := range failRe.FindAllStringSubmatch(summary, -1) {
		id := m[1]
		if id == "" {
			id = m[2]
		}
		if id != "" {
			seen[id] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// AdjustForBaseline reclassifies a head deterministic result against a
// baseline captured before the change. If the head run failed but every
// failing identifier was already failing in the baseline, the change
// introduced no new breakage and the returned CheckResult is marked passing
// (so verify.Verdict won't say "broken") — with the pre-existing failures
// reported separately for the PR body. If any failure is new, the head result
// is returned unchanged (still broken) and the new failures are listed.
//
// It only ever *softens* a failure that the baseline already had; it never
// turns a real pass into a fail.
func AdjustForBaseline(baseline, head contract.CheckResult) (adjusted contract.CheckResult, preexisting, introduced []string) {
	// Head didn't run or passed → nothing to reclassify.
	if head.TestsPass == nil || *head.TestsPass {
		return head, nil, nil
	}

	headFails := ExtractFailures(strings.Join(head.Failures, "\n"))
	if len(headFails) == 0 {
		// Couldn't parse the failures — can't prove they're pre-existing, so
		// stay conservative and leave it broken.
		return head, nil, nil
	}
	baseFails := map[string]bool{}
	for _, f := range ExtractFailures(strings.Join(baseline.Failures, "\n")) {
		baseFails[f] = true
	}
	for _, f := range headFails {
		if baseFails[f] {
			preexisting = append(preexisting, f)
		} else {
			introduced = append(introduced, f)
		}
	}
	if len(introduced) > 0 {
		// The change broke something new — genuinely broken.
		return head, preexisting, introduced
	}
	// All head failures were already red before the change: soften to passing.
	pass := true
	adjusted = head
	adjusted.TestsPass = &pass
	adjusted.Typecheck = &pass
	adjusted.Failures = nil
	return adjusted, preexisting, nil
}
