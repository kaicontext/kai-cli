package agent

import (
	"regexp"
	"strings"
)

// buildSuccessGuardNudge is the user-role message injected when the
// model narrates "build succeeded" / "tests pass" while its most
// recent bash command exited non-zero. Round-21 dogfood produced this
// exact pattern: worker ran `go build ./cmd/kai/...` three times — all
// failed with "directory prefix does not contain main module" (a cwd
// bug since fixed) — then said "The build/vet commands succeeded with
// no errors (clean output)." The orchestrator's downstream build_check
// would still catch the real state, but the agent's own narration
// polluted scrollback and the trailer.
//
// Phrasing mirrors the absence and hallucination guards: name the
// specific contradiction, demand a re-verification, and reject the
// shortcut of re-claiming success without a fresh exit-zero command.
const buildSuccessGuardNudge = `Your most recent bash command exited non-zero, but your final message claims build/tests succeeded. Those two statements cannot both be true.

Before declaring this work done, you must:
  1. Re-run the relevant command (build, test, vet — whichever you claimed succeeded).
  2. Report the actual exit code and the relevant lines of output.
  3. If exit is still non-zero, do not declare success — diagnose the failure or report yourself blocked.

A claim of success that is not backed by a visible exit-zero command is a hallucination. The downstream harness runs its own build check and will block integrate if the working tree doesn't compile; your narration of success in that case is a lie to the user, not a shortcut around the gate.`

// buildSuccessClaimRe matches phrasings the model uses to declare
// build/test/compile success. Case-insensitive, scoped to the
// assistant's final-text turn (so we don't false-positive on
// intermediate "I'll now check that the build succeeds" framings).
//
// Each alternative is the affirmative form of one of the things the
// model commonly narrates in a closing summary:
//   - "build succeeded" / "build passed" / "build is clean"
//   - "tests pass" / "tests passed" / "all tests pass"
//   - "compiles cleanly" / "compiled cleanly" / "no compile errors"
//   - "go build .* succeeded" / "go test .* passed"
//   - "vet succeeded" / "lint passed" / "no errors"
//   - "everything compiles" / "everything passes"
//
// Tuned for false-negative tolerance over false-positive: better to
// let an unusually-phrased claim slip than to nudge on benign prose
// like "the build script should help you verify."
var buildSuccessClaimRe = regexp.MustCompile(`(?i)\b(` +
	// Primary form: a tool name (optionally paired with another via
	// "/", "+", " and "), optionally followed by "command(s)", followed
	// by a success verb. Covers "build succeeded", "build/vet commands
	// succeeded", "tests passed", "vet ran clean", etc.
	`(?:build|vet|test|tests|lint)(?:\s*[/+]\s*(?:vet|test|tests|lint)|\s+and\s+(?:vet|test|tests|lint))?(?:\s+command(?:s)?)?\s+(?:succeed(?:ed|s)|pass(?:ed|es)?|completed\s+(?:cleanly|successfully)|exited\s+(?:0|zero)|ran\s+clean|is\s+clean|are\s+clean|are\s+green)` +
	`|compile[sd]?\s+(?:cleanly|successfully|without\s+errors?)` +
	`|no\s+(?:compile|build|vet|lint|test)\s+errors?` +
	`|everything\s+(?:compiles|passes|builds)` +
	`|all\s+tests?\s+(?:pass(?:ed|es)?|succeed(?:ed|s)?|green)` +
	`|clean\s+output\)?\s*$` +
	`)\b`)

// ClaimsBuildSuccess reports whether the text contains a claim that a
// build, test, vet, or lint command succeeded. Used by the build-
// success hallucination guard alongside the lastBashFailed signal:
// the cross-reference catches narration that contradicts the most
// recent bash exit code.
//
// Negation-aware in a coarse way: phrases starting with "no" that ARE
// the success claim ("no compile errors") are positive matches; phrases
// where a negation precedes the claim ("build didn't succeed",
// "tests aren't passing") are not. We let the regex's keyword form
// carry that — "didn't succeed" doesn't match "succeed(ed|s)" because
// the trailing characters differ. Coarse but adequate for the dogfood
// signal we're catching.
func ClaimsBuildSuccess(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	return buildSuccessClaimRe.MatchString(t)
}
