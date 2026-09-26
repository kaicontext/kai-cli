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
  beside it. The outcome is **completed_with_unresolved** and the command exits
  successfully: the assessment finished, with explicit unresolved questions.
  The legacy `incomplete: true` flag remains for older consumers, so they cannot
  mistake unresolved questions for a clean review. New consumers use `outcome`
  and display a neutral check, never an all-clear. A draft decision the
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

Citations are by location, in ONE declared coordinate system per source. Each
source's header says which numbers to cite:

- a `kai_view` result is shown verbatim — the tool's own `N: text` file line
  numbers, its git header, its truncation trailer — and is cited by **file line
  numbers**, and only within the lines the tool actually **returned**. The
  call's `offset`/`limit` say what was requested; the returned rows say what
  came back (a short file, an empty result or a truncation return less), and
  only those lines are valid targets. The header, the trailer and the harness
  footer are outside the mapping and cannot be cited;
- every other source — the prompt and diff, grep and other tool output,
  experiment results — is shown with **row numbers** at the left and cited by
  those.

The call's `offset` is interpreted exactly as the file tool interprets it
(integer, `null`, `"0.0"`, `" 3 "`, a float — truncated; a negative clamped
to 0). A `kai_view` result whose file mapping cannot be established — no file
rows, rows that do not start where the offset says, an offset the tool would
refuse — is **unmapped**: shown for context, citable by nothing; it is never
silently re-addressed by rows, which would let "file line 1" resolve to the
tool-call header. Validation uses only the declared system; it never guesses
another one, and a citation that does not resolve there is invalid. This replaced a rendering
that stacked the system's row numbers in front of the tool's file line
numbers; the model cited file lines, and on large files viewed in slices they
fell outside the row range, which withheld whole reviews (kai-cli#119's own
review, 2026-09-17). The cited lines are copied by the system — the model
never reproduces an excerpt — and a citation still establishes only location,
never that the lines support the claim. Each recorded citation says which
coordinate resolved it.

An invalid citation location no longer withholds the review. The submission
gets ONE correction round under the original deadline, with tools restricted
to `submit_review`, that reports **every** invalid location at once (check,
citation, source, range, reason). The corrected answer is validated in full.
Whatever is still unresolvable afterwards makes its allegation or decision
**unresolved** — a verdict cannot rest on evidence that points nowhere — with
the exact reason, while every other item is published as validated and the
review is marked incomplete. A correction that cannot be obtained (deadline,
provider error, malformed resubmission) publishes the first answer in that
degraded form. Semantic uncertainty (`unverified`) is not a citation error and
does not trigger the correction.

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

## Execution outcomes and rollout

Bundles now carry `outcome`: `completed`, `completed_with_unresolved`, or
`interrupted`. A failure before a bundle exists still exits non-zero. Actual
interruption continues to emit its coverage first and then fail. Confirmed
findings, unresolved reasons, withheld remedies and readiness caps are unchanged.
Deploy the server reader before pinning the CLI image; older readers continue to
show the conservative incomplete state. Do not infer an improved completion rate
from relabeling unresolved questions.

Dependency preflight now considers unchanged root-module requirements imported
by changed Go files, not only go.mod changes. Ordinary GitHub release tags are
resolved to a commit and subsequent source requests use that commit. There is no
fallback to main. Existing limits remain: ten seconds, three package attempts,
48 KiB total source, six source files per package. Source headers record the
commit; a partial source listing never establishes that the whole package was
read. Replaced modules, nested modules, non-GitHub modules and unresolvable tags
remain outside this fetch path. This is bounded evidence retrieval, not a claim
that every dependency question can now be settled.

The draft prompt asks for a demonstrated trigger before raising an allegation
(e.g. a concurrent caller for a suspected race). Unavailable evidence belongs in
limitations; the challenge still retains any unresolved allegation already made.
This prompt change needs live evaluation, not just unit-test assertions.
