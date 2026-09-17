# Checking review findings before publication

`kai review-commit` now challenges proposed defects before returning a review.
This applies to both the fast pass and the deep review, including reviews written
by the conclusion fallback. A draft without an `ISSUES` entry skips this step.

The challenge uses a fresh model conversation containing the draft, the original
review context, and complete successful tool results. It tries to disprove each
issue, looks for contradictory reasoning, and checks proposed repairs against the
supported inputs. Each issue receives a supported, refuted, or unverified
assessment with a reason, an explicit runtime-evidence classification, and
evidence.

The published review is **assembled by the system from the validated results,**
not copied from a model-authored blob. The challenger returns structured fields —
a scope list, a limitations list, per-supported-finding text, decisions,
`intent_match`, and `merge_ready` — and the system builds the body and the coda.
There is deliberately **no free-form assessment or summary field.** The `SUMMARY`
is derived from the final supported/refuted/unresolved counts and statuses, and
the only review-level prose is structured scope and limitations, which describe
coverage rather than outcomes. That leaves no free-text slot in which a refuted
or unresolved allegation could be restated as a confident defect — verbatim *or
paraphrased*. Consistency is achieved by construction; the gate does not rely on
matching strings to establish it.

To be precise about what is and is not verified: the system controls the
**structure and selection** — which allegations get a section at all, and what
the summary and coda say. The text *inside* a supported finding's section, and
the scope, limitations, and decision items, is still model-authored. A refuted or
unresolved allegation gets no section and no place in the summary; a supported
finding's description is the model's, selected and placed by the system.
Decisions (correct changes that still need a human's yes) are a genuinely
separate list, preserved explicitly in both the prose and the `DECISIONS` coda.

Evidence is cited **by location, not by copied text.** Every source is shown to
the model with numbered lines; a citation is a source number and a line range,
and the system extracts those exact lines itself. The reviewer already holds all
the source material, so a published finding never depends on the model
reproducing text byte-for-byte — the failure mode where a whitespace or escaping
difference in a re-typed quote sank an otherwise sound review is gone.

Each allegation is judged **on its own evidence.** A citation whose source
number or line range is out of bounds is dropped (and logged) — it costs that one
citation, never the whole review. A supported or refuted verdict needs at least
one usable citation; a verdict with none is downgraded to unverified. So one
malformed reference can, at most, drop the single finding it belonged to, while
every independently supported finding still publishes.

Every check must **classify whether the allegation requires runtime evidence.**
The classification is mandatory — an omitted flag fails the challenge closed, so
a model cannot skip the declaration to dodge the experiment requirement. Some
allegations are settled by reading the source; others assert runtime behavior
that reading cannot establish (what a shell does after a successful `cd`, whether
a quoting scheme survives a hostile path). The condition is *does this allegation
require runtime evidence?* — not *is a sandbox configured?* A runtime allegation
must be backed by a successful `review_shell` experiment; without one it stays
**unverified**, whatever the reasoning. Missing runtime evidence is never
permission to substitute confident reasoning.

Unverified allegations do not withhold the review, but they do make it
**incomplete** — as a real bundle state, not only prose. The independently
supported findings publish; each unverified allegation is preserved and listed
with why it could not be settled; `MERGE_READY` is capped so an unresolved
runtime claim cannot ride out under a ready-to-merge score; and the emitted
bundle's `incomplete` flag is set and the run exits non-zero, so Atlas and CI
never read a partial review as a completed one. Structural failures fail closed
and withhold the draft entirely: malformed responses, an unknown or duplicated
check, a missing check, a missing runtime classification, a supported finding
with no description, an incoherent verdict/readiness pair, timeouts. The original
draft is never used as the fallback for a withheld challenge. Deep reviews emit
an incomplete bundle and a nonzero exit; fast reviews return an error so the
workflow can continue to its deep pass.

This adds a model call when a draft has issues. The challenge has a three-minute
ceiling; a fast review keeps its existing overall `KAI_FAST_BUDGET`. Large reviews
may need more context: the conclusion no longer cuts every tool result at 2,000
characters or retries with only the tail of the conversation. Evidence above a
1 MiB serialized limit leaves the review incomplete instead of silently removing
the source needed to check an allegation.

The challenge is still model judgment. A source citation establishes provenance,
not proof of behavior, and a second model assessment cannot guarantee correctness.
The existing finding schema and Atlas citation badges are unchanged by this PR.

## Optional isolated shell experiments

Set `KAI_REVIEW_SANDBOX_IMAGE` to a trusted, **preloaded, digest-pinned** Docker
image containing `/bin/sh`, for example `registry/review@sha256:<64 hex digits>`.
The operator provisions that image and Docker access separately. The reviewer
never pulls an image automatically and never falls back to executing model code
on the host.

This exposes `review_shell` to the challenge pass. Each experiment uses a new
container with no network or host mounts, a read-only root filesystem, an
unprivileged user, all capabilities dropped, and no-new-privileges. Only a 16 MiB
temporary `/tmp` is writable. Each invocation is limited to five seconds, 32
processes, 64 MiB memory, half a CPU, and 16 KiB each of stdout/stderr. The Docker
container is removed on completion, error, cancellation, and timeout. At most
four experiments are allowed per challenge. Only successful, complete tool
results can be cited; shell syntax errors are valid observed outcomes.

The tool checks synthetic POSIX shell examples. It does not reproduce the user's
interactive terminal, a Windows shell, or the application backend. Without the
configured runtime, the challenge uses supplied source evidence and must leave
unestablished runtime claims unverified. Deploying this CLI does not automatically
provision a sandbox in existing review pods.

## Regression checks

Normal unit tests cover publication failure, rejected versus supported claims,
missing/invented citations, full source preservation, and bounded execution setup:

```sh
GOWORK=off go test ./cmd/kai -run 'TestReviewChallenge|TestReviewCitation|TestReviewConclusion|TestFastReviewDoesNotPublish|TestReviewSandbox'
```

To execute the PR #418 examples in the actual restricted container, set the image
above and run:

```sh
GOWORK=off KAI_REVIEW_SANDBOX_TEST=1 go test ./cmd/kai -run '^TestReviewSandboxDesktop418$' -count=1 -v
```

It verifies that a successful `cd` persists across lines, the original heredoc
works, appending `; }` breaks the heredoc, double-quoted paths expand, and a failed
`cd` does not guard later independent lines. It also checks container identity,
read-only root, and the experiment deadline.

The model evaluation is opt-in and incurs usage through the configured Kai
provider. It is separate from unit tests so a mocked answer is never presented
as evidence of model quality:

```sh
GOWORK=off KAI_REVIEW_LIVE_EVAL=1 KAI_REVIEW_EVAL_CONFIG_DIR="$HOME/.kai" \
  go test ./cmd/kai -run '^TestReviewChallengeLiveDesktop418$' -count=1 -v
```

This uses the configured sandbox and requires the checker to reject the false
multiline allegation while retaining the real escaping defect. Use
`KAI_REVIEW_MODEL` to compare models against the same case.

Review CI runs a pinned Kai image. Merging this CLI change alone does not update
the production reviewer: rebuild the CI image and update the server's workflow
image pin as a separate rollout, after the live evaluation passes.
