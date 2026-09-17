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

**Every allegation's final result is structured data** (`rcAllegationResult`):
its id, status (`supported` / `refuted` / `unresolved`), the validated evidence
references (each marked whether it is an experiment from this challenge), the
reason it is unresolved, and any proposed remedy. **A remedy is attached to its
allegation id and published as actionable only when that allegation is
supported.** A fix the model proposed for an allegation that is not supported is
recorded as `withheldRemedy` and never published as advice. The log records the
*final* validated verdict — including a downgrade the model did not ask for —
not the verdict the model submitted.

The published review is **assembled by the system from those results,** not
copied from a model-authored blob. The challenger returns a scope list, a
limitations list, per-supported-finding text and remedy, per-decision
assessments, `intent_match`, and `merge_ready`; the system builds the body and
the coda. There is deliberately **no free-form assessment or summary field.**
The `SUMMARY` and the incomplete status are derived from the final counts and
statuses, and the only review-level prose is structured scope and limitations,
which describe coverage rather than outcomes. That leaves no free-text slot in
which a refuted or unresolved allegation could be restated as a confident defect
— verbatim *or paraphrased*. Consistency is by construction; the gate does not
rely on matching strings.

**Decisions are assessed, not asserted.** The unrestricted "Decisions" path is
gone: the challenger cannot introduce a decision of its own — it can only assess
the decisions the *draft* made, each with a verdict and a citation into the
sources, exactly like an allegation. A decision the draft never made is dropped
(and logged); a draft decision left unassessed fails the challenge closed. A
different heading therefore cannot carry repair advice around the evidence
requirement. Genuine design decisions the draft made are preserved when
supported, in both the prose and the `DECISIONS` coda.

The status travels with the review. The emitted bundle carries the `incomplete`
flag and the full structured `challenge` record (allegations, decisions,
unresolved list); the CLI's text output prints an explicit `Status: INCOMPLETE`
line; and the run exits non-zero. Atlas and CI therefore read the same verdict
the gate decided.

To be precise about what is and is not verified: the system controls the
**structure and selection** — which allegations get a section at all, which
remedies are actionable, and what the summary and coda say. The text *inside* a
supported finding's description and remedy, and the scope, limitation, and
decision items, is still model-authored; the system selects and places it.

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

## Experiment fidelity and result interpretation

The forensics of a wrong verdict (`docs/review-evidence-glm-runs.md`) showed a
concrete mechanism: the model tested a *different* command from the one the
code generates — a hand-escaped one — and took its success as proof the real
one was safe; it also misread a printout that plainly showed the unescaped
metacharacters. Three changes address exactly that, and nothing broader:

- **Execute the generated command unchanged.** In `review_shell`'s fidelity
  mode the model supplies `construct` — Node code that builds and prints the
  command string — and optional `setup`. The harness feeds the **generated
  string verbatim** into `/bin/sh`. What this enforces is narrow and should be
  stated narrowly: the model cannot alter the string *between its own
  construction and execution*. **The construction code is still
  model-authored.** Nothing verifies that it matches the source under review;
  a model that wrote a subtly different construction would have it executed
  faithfully and wrongly. Unverified construction is a separate risk — it was
  **not** the demonstrated cause of the observed wrong verdict (that run's
  constructor matched the code by its own account), but it remains open.
- **Explicit assertions.** The model declares what it expects to observe —
  `exit`, `stdout_contains`, `stdout_not_contains`, `pwd`, `pwd_not` — and the
  harness evaluates each as PASS/FAIL against the recorded exit status, stdout,
  and the working directory the command actually left behind.
- **The verdict accounts for the assertion results — as currently implemented,
  too strongly.** The implemented rule is: a supported/refuted verdict on a
  runtime claim may cite an experiment only if it declared at least one
  assertion and **all of them passed**; a bare printout (no assertions) can back
  nothing. That rule correctly stops a failed path-equality check from backing
  "this quoting is safe." But "all passed" is **not necessary for useful
  evidence**: if the requirement is "run in the exact workspace directory," a
  *failed* directory-equality assertion can itself demonstrate the defect. The
  current rule discards that observation. The right distinction is between a
  **broken experiment** (it did not run, or its record is incomplete) and a
  **completed experiment whose assertion failed** — the latter is a valid
  observation ("expected X, observed Y") that can support an allegation of
  exactly that behavior. Implementation is paused; this is recorded as a known
  flaw of the rule, not fixed here.

**The complete experiment record is preserved:** mode, setup, construct,
generated command, exit code, observed working directory, stdout, stderr, and
each assertion with its observed value travel in the challenge result and the
emitted bundle (`challenge.experiments`, keyed by the source number the
verdicts cite). Only the console summary is bounded.

This does not make review correctness general, and it did **not** close the
demonstrated failure. What is established: the generated string is executed
unchanged, and an experiment whose assertions failed cannot be cited as if it
had confirmed the model's expectation. What is not established: that the
construction matches the source, or that the tested input bears on the
allegation. The end-to-end wrong "safe" verdict that followed this change was
produced by a faithful experiment on an **inadequate input** — a double quote,
which `JSON.stringify` does escape — whose passing assertions were taken as a
general claim of safety about `$` and backticks. Passing assertions establish
behavior for the input tested; they cannot refute a claim about a different
input. The open design question is recorded in `review-evidence-glm-runs.md`.

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
