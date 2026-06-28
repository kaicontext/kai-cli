package verify

import (
	"testing"

	"github.com/kaicontext/kai-engine/contract"
)

func boolp(b bool) *bool { return &b }

func TestVerdict_NeverVerified(t *testing.T) {
	cases := []struct {
		name        string
		cr          contract.CheckResult
		hasContract bool
		want        contract.Verdict
	}{
		{"failing tests → broken (with contract)", contract.CheckResult{TestsPass: boolp(false)}, true, contract.VerdictBroken},
		{"failing tests → broken (no contract)", contract.CheckResult{TestsPass: boolp(false)}, false, contract.VerdictBroken},
		{"clean pass, no contract → no_intent", contract.CheckResult{TestsPass: boolp(true)}, false, contract.VerdictNoIntent},
		{"clean pass, with contract → clean_unconfirmed", contract.CheckResult{TestsPass: boolp(true)}, true, contract.VerdictCleanUnconfirmed},
		{"inconclusive, with contract → drifting", contract.CheckResult{}, true, contract.VerdictDrifting},
		{"inconclusive, no contract → no_intent", contract.CheckResult{}, false, contract.VerdictNoIntent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Verdict(c.cr, c.hasContract)
			if got != c.want {
				t.Fatalf("Verdict = %q, want %q", got, c.want)
			}
			// The load-bearing invariant: the deterministic layer must never
			// claim verified.
			if got == contract.VerdictVerified {
				t.Fatal("deterministic layer must never produce verified")
			}
		})
	}
}
