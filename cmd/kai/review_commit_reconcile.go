package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/kaicontext/kai-engine/provider"
)

// Reconciling verdicts across separate checks.
//
// Allegations are checked in batches, and a failed batch is re-checked one
// allegation at a time — all side by side, each in its own conversation. Two
// allegations about the same code can therefore be judged on different
// evidence. On kaicontext/kai-vcs#1 (2026-10-14) two allegations about
// Bundle::read's inventory loop were re-checked separately: one opened
// package::validate, saw it reject short tuples before the loop, and refuted
// "a short entry panics"; the other never opened it and supported "type
// 65546 is truncated to 10" — although the same validator rejects types
// outside 1..65535 a few lines further on. The review published the false
// one as its headline defect.
//
// So after every verdict is in, a supported allegation that sits next to a
// refuted one whose reason is that the input never gets there (a guard, a
// validator, an earlier rejection) is checked again with that reason in
// front of it. The second check decides; when it cannot finish, the
// allegation is withheld as unresolved rather than published against a
// sibling's contrary finding.

// rcSiblingWindow is how close (in lines of the same file) two allegations
// must be for one's refutation to bear on the other.
const rcSiblingWindow = 40

// rcMaxReconciles bounds the extra checks per review.
const rcMaxReconciles = 4

// rcUpstreamGuardRe recognises a refutation that rests on the input not
// reaching the line: something before it validates, guards or rejects it.
// Refutations for other reasons (generic advice, intended behaviour, a
// duplicate) carry nothing a sibling needs to reconsider.
var rcUpstreamGuardRe = regexp.MustCompile(`(?i)(not reachable|unreachable|never reach|cannot reach|can't reach|never gets? there|is rejected|are rejected|rejects|returns? err|validat|guard|checked before|checks? .{0,40}before|before .{0,80}reach)`)

// rcIssueLocations returns every path:line an allegation names: its lead
// location and any "(also: path:line)" ones.
func rcIssueLocations(issue string) []rcLoc {
	var out []rcLoc
	if p, l, ok := rcIssueLocation(issue); ok {
		out = append(out, rcLoc{p, l})
	}
	if i := strings.Index(issue, "(also:"); i >= 0 {
		for _, m := range rcAlsoLocRe.FindAllStringSubmatch(issue[i:], -1) {
			var n int
			fmt.Sscanf(m[2], "%d", &n)
			if n > 0 {
				out = append(out, rcLoc{m[1], n})
			}
		}
	}
	return out
}

type rcLoc struct {
	path string
	line int
}

// rcNear reports whether two allegations name lines within rcSiblingWindow of
// each other in the same file.
func rcNear(a, b string) bool {
	for _, x := range rcIssueLocations(a) {
		for _, y := range rcIssueLocations(b) {
			if x.path != y.path {
				continue
			}
			d := x.line - y.line
			if d < 0 {
				d = -d
			}
			if d <= rcSiblingWindow {
				return true
			}
		}
	}
	return false
}

// rcGuardSiblings pairs each supported allegation with the nearby refuted
// ones whose refutation is an upstream guard. Duplicate refutations ("Duplicate
// of issue N") are skipped: they say nothing about reachability.
func rcGuardSiblings(allegations []rcAllegationResult) map[int][]int {
	out := map[int][]int{}
	for i, s := range allegations {
		if s.Status != rcStatusSupported {
			continue
		}
		for j, r := range allegations {
			if i == j || r.Status != rcStatusRefuted {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(r.Reason), "Duplicate of") || !rcUpstreamGuardRe.MatchString(r.Reason) {
				continue
			}
			if rcNear(s.Issue, r.Issue) {
				out[i] = append(out[i], j)
			}
		}
	}
	return out
}

// rcSiblingContext is what the second check reads before the allegation: the
// nearby refutations, quoted as another checker's conclusion to verify, not
// to accept.
func rcSiblingContext(allegations []rcAllegationResult, siblings []int) string {
	var b strings.Builder
	b.WriteString("A SEPARATE CHECK OF NEARBY CODE in the same file refuted the allegation(s) below because the input never reaches the line. Decide whether the same guard or validation also applies to the allegation you are checking: open the code that reasoning names before you rely on it, and do not take it on trust. If it rejects the input your allegation needs, REFUTE and name the check; if you cannot settle that, the verdict is unverified, not supported.\n")
	for _, j := range siblings {
		fmt.Fprintf(&b, "\n- Refuted: %s\n  Because: %s\n", allegations[j].Issue, allegations[j].Reason)
	}
	return b.String()
}

// rcReconcileSiblings re-checks supported allegations that a nearby
// upstream-guard refutation contradicts, and returns how many it re-checked.
func rcReconcileSiblings(ctx context.Context, prov provider.Provider, model, prose string, res *rcChallengeResult, sources, extra []rcSource, sandbox *rcShellSandbox) int {
	pairs := rcGuardSiblings(res.Allegations)
	if len(pairs) == 0 {
		return 0
	}
	done := 0
	for i := range res.Allegations {
		sib, ok := pairs[i]
		if !ok || done >= rcMaxReconciles || ctx.Err() != nil {
			continue
		}
		done++
		a := res.Allegations[i]
		fmt.Fprintf(os.Stderr, "  challenge: re-checking a supported allegation against %d nearby refutation(s) of the same code: %s\n", len(sib), rcClip(a.Issue, 120))
		draft := rcSubDraft(strings.TrimSpace(prose)+"\n\n"+rcSiblingContext(res.Allegations, sib), []string{a.Issue}, nil)
		again, err := rcChallengeDraft(ctx, prov, model, draft, rcBatchSources(sources, extra, []string{a.Issue}), sandbox)
		if err != nil || again == nil || len(again.Allegations) == 0 {
			why := "the re-check did not complete"
			if err != nil {
				why = fmt.Sprintf("the re-check did not complete (%v)", err)
			}
			withheld := a.WithheldRemedy
			if a.Remedy != "" {
				withheld = a.Remedy
			}
			res.Allegations[i] = rcAllegationResult{ID: a.ID, Issue: a.Issue, Status: rcStatusUnresolved,
				Reason:   "a nearby allegation on the same code was refuted because the input never reaches it, and " + why,
				Evidence: a.Evidence, WithheldRemedy: withheld}
			fmt.Fprintf(os.Stderr, "  challenge: re-check incomplete — withheld as unresolved\n")
			continue
		}
		r := again.Allegations[0]
		r.ID = a.ID
		r.Issue = a.Issue
		res.Allegations[i] = r
		fmt.Fprintf(os.Stderr, "  challenge: re-check: %s\n", r.Status)
	}
	return done
}
