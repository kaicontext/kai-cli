package main

import (
	"fmt"
	"io"

	"kai/internal/contract"
	"kai/internal/verify"
)

// renderVerificationStatus prints the verification surface at the top of
// `kai status`: in-flight contracts with their structural verdicts, residue,
// and daemon liveness. Best-effort — if the store can't open or there's
// nothing to show, it prints nothing and `kai status` proceeds as before.
//
// Phase 2 has no intent attribution yet, so every contract reflects the same
// tree-wide deterministic verdict from the last daemon pass. It can never show
// `verified` (that's the semantic layer, Phase 3).
func renderVerificationStatus(w io.Writer) {
	store, err := contract.Open(kaiDir)
	if err != nil {
		return
	}
	defer store.Close()

	cs, err := store.List()
	if err != nil {
		return
	}
	var inflight []*contract.Contract
	for _, c := range cs {
		if !c.Closed {
			inflight = append(inflight, c)
		}
	}
	structural, ran, _ := store.GetStructural()

	if len(inflight) == 0 {
		// Nothing declared — still surface the no-intent safety net if the
		// daemon found a hard problem in hand-written code.
		if ran && structural.TestsPass != nil && !*structural.TestsPass {
			fmt.Fprintf(w, "%s no-intent: structural broken\n", contract.VerdictBroken.Glyph())
			for _, f := range structural.Failures {
				fmt.Fprintf(w, "    %s\n", firstLine(f))
			}
			fmt.Fprintln(w)
		}
		return
	}

	fmt.Fprintf(w, "%d contract(s) in flight\n\n", len(inflight))
	var residue []contract.ResidueItem
	var latestSemantic int64
	for _, c := range inflight {
		// A contract that's had a semantic check uses its stored verdict (which
		// can be `verified`); otherwise fall back to the live structural verdict.
		v := c.Status
		if c.Semantic.RanAt == 0 && ran {
			v = verify.Verdict(structural, true)
		}
		fmt.Fprintf(w, "%s %-44s %s\n", v.Glyph(), truncate(c.Statement, 44), v.Display())
		residue = append(residue, c.Residue...)
		if c.Semantic.RanAt > latestSemantic {
			latestSemantic = c.Semantic.RanAt
		}
	}
	if len(residue) > 0 {
		fmt.Fprintf(w, "\nresidue (%d)\n", len(residue))
		for _, r := range residue {
			fmt.Fprintf(w, "  ? %s\n", r.Prompt)
		}
	}

	structPhrase := "never run (try 'kai watch')"
	if ran {
		structPhrase = relTime(structural.RanAt)
	}
	semPhrase := "not run (try 'kai verify')"
	if latestSemantic > 0 {
		semPhrase = relTime(latestSemantic)
	}
	fmt.Fprintf(w, "\ndaemon · structural %s · semantic %s\n\n", structPhrase, semPhrase)
}
