package main

import (
	"strings"
	"testing"
)

// A specialist bullet must name a trigger and cite evidence; anything else is
// dropped before the fact-check.
func TestSpecialistContractKeepsOnlyGroundedBullets(t *testing.T) {
	in := []string{
		"a.go:10 — inverted check | trigger: an admin who is not owner calls Add | evidence: a.go:10, b.go:4 | consequence: admins are blocked",
		"a.go:12 — inverted check | Trigger:  a request with no token | Evidence: a.go:12 | consequence: x",
		"a.go:20 — missing timeout on the fetch",
		"a.go:30 — x | trigger: | evidence: a.go:30",
		"a.go:40 — x | evidence: a.go:40 | trigger: y",
	}
	kept, dropped := rcSpecialistContractFilter(in)
	if len(kept) != 2 || dropped != 3 {
		t.Fatalf("kept %d dropped %d: %q", len(kept), dropped, kept)
	}
}

// Every specialist is off unless a stage names a model, so the prototype
// changes nothing by default.
func TestSpecialistsAreOffByDefault(t *testing.T) {
	t.Skip("candidate/specialists-on-v2: the kai org's review image runs the specialists by default; main keeps them off")
	for _, st := range rcSpecialistStages {
		if m := rcEnsembleModel(st); m != "" {
			t.Errorf("%s defaults to %q, want off", st, m)
		}
		if strings.TrimSpace(rcSpecialistFocus[st]) == "" {
			t.Errorf("%s has no focus prompt", st)
		}
	}
	t.Setenv("KAI_SPEC_SECURITY_MODEL", "z-ai/glm-5.2")
	if m := rcEnsembleModel(rcStageSpecSecurity); m != "z-ai/glm-5.2" {
		t.Errorf("env did not turn the security specialist on: %q", m)
	}
}
