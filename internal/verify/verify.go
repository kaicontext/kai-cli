// Package verify is the continuous deterministic layer of the verification
// daemon (Horizon 1, Phase 2). It runs cheap, LLM-free checks — typecheck and
// tests today, graph/invariant checks as they're wired — and maps the result
// to a structural verdict.
//
// By construction this layer can NEVER produce `verified`: confirming that an
// implementation matches declared intent is the semantic layer's job (Phase 3).
// The most this layer can say is clean_unconfirmed (deterministic checks pass,
// semantic match not established) — which is exactly the honesty the verdict
// vocabulary is built to preserve.
package verify

import (
	"context"
	"time"

	"kai/internal/contract"
	"kai/internal/orchestrator"
)

// Continuous runs the deterministic checks over the working tree at dir and
// returns a timestamped CheckResult. No tokens, no LLM. A missing test
// convention yields an inconclusive result (checks nil), not a failure.
func Continuous(ctx context.Context, dir string, timeout time.Duration) contract.CheckResult {
	cr := contract.CheckResult{RanAt: time.Now().UnixMilli()}

	// VerifyWorkspace compiles (catching typecheck/build errors) and runs the
	// project's tests — a single deterministic pass/fail over the tree.
	res := orchestrator.VerifyWorkspace(ctx, dir, timeout)
	if !res.Ran {
		return cr // no convention detected — deterministic layer is inconclusive
	}
	ok := res.OK
	cr.Typecheck = &ok
	cr.TestsPass = &ok
	if !ok {
		cr.Failures = []string{res.Summary}
	}
	return cr
}

// Verdict maps a deterministic CheckResult to a structural verdict.
//
// It never returns VerdictVerified — that requires the semantic layer. The
// ordering encodes the honesty rules: a hard failure is broken; code with no
// contract is no_intent (structure only, never an intent claim); an
// inconclusive result keeps a contract drifting; a clean deterministic pass on
// a contract is clean_unconfirmed, never verified.
func Verdict(cr contract.CheckResult, hasContract bool) contract.Verdict {
	if cr.TestsPass != nil && !*cr.TestsPass {
		return contract.VerdictBroken
	}
	if !hasContract {
		return contract.VerdictNoIntent
	}
	if cr.TestsPass == nil {
		return contract.VerdictDrifting // no deterministic signal yet
	}
	return contract.VerdictCleanUnconfirmed
}
