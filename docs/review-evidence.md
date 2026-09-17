# Checking review findings before publication

`kai review-commit` now challenges proposed defects before returning a review.
This applies to both the fast pass and the deep review, including reviews written
by the conclusion fallback. A draft with neither `ISSUES` nor `DECISIONS` skips
this step.

The challenge uses a fresh model conversation containing the draft, the original
review context, and complete successful tool results. It tries to disprove each
issue, looks for contradictory reasoning, and checks proposed repairs against the
supported inputs. It returns **one structured result per allegation** —
supported, refuted, or unverified — with its reasoning, its citations, and, for
a supported allegation, the finding text and any remedy. It assesses each of the
draft's `DECISIONS` the same way.

The published review is **assembled in code** from those results. The challenger
no longer rewrites the review: that second piece of model prose could contradict
its own checks — keep an allegation it had just refuted, or lose one it had
supported — and any such slip withheld the entire review. Findings, remedies,
the summary, the counts and the `ISSUES` coda are now the same data, so they
cannot disagree:

- a **supported** allegation is published as a finding, with its remedy;
- a **refuted** allegation is not published, and neither is its remedy (the
  proposal is kept in the bundle's record as withheld, never as advice);
- an **unverified** allegation or decision is listed as unresolved, with its
  reason and without repair advice. Supported findings are still published
  beside it, but the review is marked **incomplete**: the bundle carries
  `incomplete: true`, the text says so, and the command exits non-zero, so a
  partial review is never read as a completed one. A draft decision the
  challenger did not assess, or a supported verdict with no finding text, is
  unresolved in the same way rather than sinking the whole review;
- readiness is only ever **capped** from what the challenger proposed — at
  "small fixes" when a defect is confirmed, at "decide, then merge" when a
  decision is open or anything is unresolved — and never raised.

Structural failures still prevent publication: malformed responses, a missing or
duplicated check, a check for an issue the draft never raised, a supported or
refuted verdict with no evidence, citations to locations that do not exist,
invalid intent or readiness values, and timeouts. The original draft is never
used as the fallback for a failed challenge. Deep reviews then emit an
incomplete bundle and a nonzero exit; fast reviews return an error so the
workflow can continue to its deep pass.

This makes what is published follow the challenger's per-item verdicts. It does
not make those verdicts right: a wrongly supported allegation is published, with
its remedy, exactly as faithfully as a correct one.

Citations are by location. Every source is shown to the model with one-based
numbered lines; a citation names the source number and a line range, and the
system copies those lines itself. This removes the requirement that the model
reproduce an excerpt byte-for-byte — the failure that withheld whole reviews
over a mis-copied quotation. It does not check that the cited lines support the
claim: a location that exists is valid whatever it says, and a location that
does not exist (unknown source, range before line 1, reversed, or past the last
line) still fails validation.

A bad citation location gets one correction attempt before the challenge fails.
The diagnostic identifies the check, citation, source number, line range, and
reason. The model receives that diagnostic alongside its original answer and the
unchanged numbered evidence. It can only resubmit the complete answer; no
additional experiments are available. Every original validation is applied
again, and a second failure withholds the review. Semantic uncertainty
(`unverified`) is not a citation error and does not trigger this retry.

The fast pass may draft with a substituted non-reasoning model; the challenge
that decides publication is always sent to the configured review model, and the
run logs which model was requested for each phase.

This adds a model call when a draft has issues. The challenge has a three-minute
ceiling, including any citation correction; a fast review keeps its existing overall `KAI_FAST_BUDGET`. Large reviews
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
GOWORK=off go test ./cmd/kai -run 'TestReviewChallenge|TestReviewConclusion|TestFastReviewDoesNotPublish|TestReviewSandbox'
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
