# GLM-5.2 live evaluation record — preserved before further changes

This file preserves the failed model responses and the exact code revisions made
in reaction to them, in order, so that later outcomes can be judged against what
actually happened rather than against a final passing run. It is a record, not a
claim of reliability.

Model under evaluation: `z-ai/glm-5.2` (the model that produced the original
#418 and #429 failures). Selected via `KAI_REVIEW_MODEL`; the test logs the
resolved model on every run. Sandbox for with-sandbox runs: a Node-capable,
digest-pinned image `node@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32`
(`node:22-alpine`, `/bin/sh` + `node`).

Earlier runs against `anthropic/claude-opus-5` are recorded in the PR
description and are **not** evidence about GLM.

## Baseline code state for these runs

Commit `726281c` on `feat/review-cite-by-location`: cite-by-location, structured
per-allegation results, remedies gated by status, decisions assessed from the
draft, derived summary/incomplete, final-verdict logging, non-fatal
unavailable-sandbox handling. Readiness coherence at this commit was
**fail-closed** (an incoherent `merge_ready` withheld the review).

## Without a sandbox — first attempt, no retries

| case | outcome | resolved model |
|---|---|---|
| #418 | PASS — both runtime claims `unresolved`, review `incomplete`, no remedy | `z-ai/glm-5.2` |
| #429 | PASS — `unresolved`, `incomplete`, no remedy | `z-ai/glm-5.2` |

GLM explicitly declined to substitute reasoning ("plausible and likely correct,
but cannot be confirmed without runtime evidence"). No code was changed in
reaction to these runs.

## With the Node sandbox — #429, first attempt, no retries

PASS (24.99s). Experiment 2 executed `JSON.stringify` under `node`:

```
node -e 'const s = "foo`echo PWNED`bar$HOME"; ... console.log(JSON.stringify(s))'
JSON.stringify output: "foo`echo PWNED`bar$HOME"
contains $ : true
contains backtick: true
```

Supported verdict cited that experiment (`Source 3, lines 17-19, Experiment:true`)
plus a shell experiment. Remedy published as actionable. No code was changed in
reaction to this run.

## With the Node sandbox — #418, attempt by attempt

### Attempt 1 — FAIL (70.47s), code at `726281c`

```
review_commit_challenge_test.go:399: challenge readiness contradicts the surviving findings
--- FAIL: TestReviewChallengeLiveDesktop418/with-sandbox (70.47s)
```

GLM ran two experiments (a `cd "$ws" && pwd` heredoc; a single-quoted control
that printed `/tmp/literal_$HOME` and a `can't cd to /tmp/literal_/tmp/fakehome`
stderr). It then submitted a `merge_ready` that failed the coherence check, and
the gate **withheld the whole review**, including any supported finding.

**Not captured:** which of the three coherence clauses fired, and GLM's
per-allegation verdicts. At this revision the final-verdict log ran *after* the
coherence check, so a fail-closed left no verdict record. This is a gap in the
record, stated as such.

**Revision R1 (reaction):** readiness coherence changed from fail-closed to a
clamp. As first written the clamp included a *raise* (`kept==0 && unresolved==0
&& readiness < DecideThenMerge → DecideThenMerge`). That raise turns a
contradictory answer into a **more permissive** merge recommendation and is
being removed — see "Corrections" below.

### Attempt 2 — FAIL (33.03s), code at R1

```
challenge: shell experiment 1 … 4   (four experiments, outputs logged)
review_commit_challenge_test.go:398: invalid challenge JSON: invalid character 'I' looking for beginning of value
--- FAIL: TestReviewChallengeLiveDesktop418/with-sandbox (33.03s)
```

After its fourth experiment — when the only tool still offered was
`submit_review` — GLM's final turn was **prose beginning with "I"**, not a
`submit_review` call and not JSON. The gate failed closed on the whole review.

**Not captured:** the prose itself. Logging of a malformed final answer was
added only in R2, after this attempt.

**Revision R2 (reaction):** (a) an embedded JSON object in a text answer is
extracted and validated; (b) exactly one "call `submit_review` now" nudge when
the final answer is not a submission, under the original context deadline, with
`available` reduced to `submit_review` only and no change to sources or the
experiment set; the nudged answer is fully revalidated; a second non-submission
fails closed. (c) Malformed-final-answer head and each experiment's output are
logged.

### Attempt 3 — FAIL on the test expectation (48.69s), code at R2 (+R3 logging)

```
challenge: shell experiment 1 … 4
challenge: final answer was prose, not a submission — nudging once to call submit_review
challenge: unresolved — …later lines run outside the workspace after a successful cd
    requires a runtime experiment, and none from this run backs it
challenge: unresolved — …workspace expansion is possible inside double quotes
    requires a runtime experiment, and none from this run backs it
MERGE_READY: 2
SUMMARY: 0 confirmed findings, 2 unresolved. Review incomplete; …
review_commit_challenge_test.go:444: expected only the genuine escaping defect: []
```

The nudge worked: GLM submitted. The gate produced a coherent, `incomplete`
review. GLM's submission cited **only Source 1 (the code)** for both checks:

```
record=[{ID:1 … Status:unresolved … Evidence:[{Source:1 LineStart:1 LineEnd:5 Experiment:false}]
          WithheldRemedy: Wrap each line of the command individually with the workspace cd prefix,
          or … use a subshell wrapper like '(cd workspace && <command>)' for each line.}
        {ID:2 … Status:unresolved … Evidence:[{Source:1 LineStart:3 LineEnd:6 Experiment:false}]
          WithheldRemedy: Single-quote the workspace path or escape shell metacharacters …}]
```

Two things to state precisely:

1. **The gate behaved correctly.** No experiment source was cited, so both
   runtime claims were downgraded and both remedies withheld. Allegation #1's
   withheld remedy is the subshell/brace-wrapper advice from the original #418
   failure — GLM still believed the false claim and proposed the bad fix, and
   the gate withheld it. That is the "unresolved cd cannot publish brace advice"
   regression holding live on GLM.
2. **GLM did not meet the correctness target.** With a sandbox available and
   four experiments run, the real escaping defect should have been supported.
   GLM ran experiments but did not cite them. That is a GLM protocol weakness.

**Revision R4 (reaction, untested at the time of this record):** the prompt now
states that each `review_shell` result is returned as a numbered SOURCE and that
an experiment counts only if its source number is cited in the check's
evidence. This is the fourth reaction. A pass after it does **not** establish
reliability; see "Reporting rule" below.

## Corrections required by review (applied after this record)

- **Readiness is derived conservatively.** The R1 raise clause is removed. The
  system only ever *caps* readiness (a confirmed defect → at most small-fixes;
  an open decision or an unresolved claim → at most decide-then-merge). A
  contradictory answer is never turned into a more permissive recommendation.
- **Ambiguous multiple JSON objects are rejected explicitly.** The extraction
  no longer relies on `json.Valid` incidentally failing on two concatenated
  objects; two top-level objects in one answer is an ambiguity, and the gate
  does not guess which the model intended.

## Reporting rule

First-attempt failures and retry outcomes are reported separately. The
sequence above is one attempt per revision, not repeated attempts of the same
revision, so it says nothing about the pass rate of any single revision. To
claim reliability for the current revision, run it repeatedly (N runs, no
changes between them) and report the pass rate and each attempt's outcome.

## End-to-end CLI run — #429 scratch repo, first attempt, code at `fae62b5`

Command: `kai review-commit c478d2f --format json` inside a scratch repo whose
HEAD commit is the real #429 change (`'cd ' + JSON.stringify(wsPath) + ' && '`),
with `KAI_REVIEW_MODEL=z-ai/glm-5.2` and the Node sandbox.

```
z-ai/glm-5.2 is a reasoning model (hidden chain-of-thought); fast pass uses anthropic/claude-haiku-4-5 — override with KAI_FAST_MODEL
fast pass: one call over the diff, no graph (model anthropic/claude-haiku-4-5, budget 1m40s)…
challenge: shell experiment 1
Error: fast review challenge incomplete (unchecked draft withheld): invalid challenge JSON:
  json: cannot unmarshal string into Go struct field rcChallengeAnswer.checks of type []main.rcIssueCheck
EXIT=1   (stdout empty — no bundle emitted)
```

Facts, stated separately:

- **Draft author ≠ challenger on the fast path.** `rcFastModel` substitutes a
  non-reasoning model (`claude-haiku-4-5`) for the one-call fast draft when the
  review model is a reasoning model. The challenge itself ran on GLM-5.2.
- **GLM's `submit_review` call was structurally malformed:** the `checks` field
  was a JSON string, not an array. This is a schema-shape violation, distinct
  from the prose non-submission of #418 attempt 2.
- **Gate outcome:** failed closed. On the fast path a challenge failure emits
  **no bundle** and exits 1 (documented: the workflow continues to `--deep`).
  The "partial results reach the emitted bundle" property could not be observed
  here because the answer had no validated results at all.
- **Not captured:** the raw malformed submission. Only a prose non-submission's
  head is logged at this revision; an unparseable `submit_review` payload is not.

No code was changed before this record was written.

## Repeated runs of the UNCHANGED revision `46a449f` — with the Node sandbox, GLM-5.2, `-count=3`

No code changed between these attempts. Each attempt is independent.

| case | attempt 1 | attempt 2 | attempt 3 | pass rate |
|---|---|---|---|---|
| #418 | PASS (177s): false-cd refuted, escaping supported | PASS (85s): refuted, supported | **FAIL (127s)**: prose → nudged once → GLM marked BOTH allegations `supported`, **including the false cd claim**; the gate published both | 2/3 |
| #429 | PASS (38s): supported | **FAIL (66s)**: failed before any verdict was logged — reason **not captured** (see below) | PASS (88s): supported | 2/3 |

### #418 attempt 3 — the serious one

The false allegation ("later lines run outside the workspace after a successful
cd") was published as a **confirmed defect**. Because it was `supported` with
`requires_runtime`, the gate's rules mean an experiment source **was** cited.
So GLM ran an experiment and cited it in support of a claim the experiment
does not actually establish (a persistent shell's cwd survives a successful
cd). The gate verified provenance — a real experiment, really cited — but it
does not, and structurally cannot, verify that the cited output *supports* the
claim. This is the original #418 failure recurring through a satisfied evidence
requirement. Its remedy (if any) would have been published as actionable.

**Not captured:** the experiment content GLM cited, and the remedy text. My
output filter dropped the `record=` line. This is a capture failure on my
side, not a gap in what the code logs.

### #429 attempt 2 — reason unknown

The run failed without reaching a verdict line and my filter did not match the
error text. Whether it was a malformed submission, a truncated answer, an
unexpected tool, or a missing check is **unknown**. Stated as such.

### What this establishes

The current revision does **not** reliably handle the GLM cases: 4/6 with one
false-positive publication. A single passing run of this revision would have
been misleading; three were needed to see this.

**Process fix for all further runs:** full `-v` output is written to a file
before any filtering.

## End-to-end CLI run — #429 scratch repo, revision R5 (`3ffccc6`), attempt 1

Same command and repo as the `fae62b5` attempt. Full stdout/stderr saved.

```
fast pass: one call over the diff, no graph (model anthropic/claude-haiku-4-5, budget 1m40s)…
challenge: shell experiment 1
challenge: shell experiment 2
Error: fast review challenge incomplete (unchecked draft withheld): challenge produced an unknown intent verdict ""
EXIT=1   (stdout empty — no bundle emitted)
```

- GLM's `submit_review` payload **parsed this time** (`checks` was an array), so
  the R5 parse-failure repair was not involved.
- It omitted the required `intent_match` field (empty string). Under the
  stated rule this is a substantive validation failure, not a parse failure, so
  it failed closed immediately with no retry and **no bundle**.
- This is a **different GLM failure mode** from the `fae62b5` attempt
  (`checks` as a string). Two e2e attempts, two distinct schema omissions.
- The repair rule was **not** widened in reaction. Whether a missing required
  top-level field should count as a "format" failure eligible for the single
  repair is a design question left open here rather than decided to make a run
  pass.

## End-to-end CLI run — revision R5 (`3ffccc6`), attempts 2 and 3 (no code changes)

| attempt | exit | outcome |
|---|---|---|
| 2 | 1 | `checks` as a string again. The R5 single repair **fired**: parse error fed back, GLM resubmitted — with the **same** malformed shape. Failed closed after the one allowed repair. **No bundle.** |
| 3 | 0 | Bundle emitted, `depth=fast`, `readiness=4`, complete (no unresolved). **The real JSON.stringify defect was REFUTED**, with two experiments cited (`exp=[True,True]`). GLM's stated reason: "JSON.stringify will escape them" — which is false (node prints `$` and backtick through unescaped). |

### Attempt 3 — the mirror of #418 attempt 3

A real defect was published as **refuted**, backed by cited experiments. The
gate verified provenance (experiments were run and cited) and status
coherence, and it cannot verify that the cited output supports the conclusion.
#418 attempt 3 was a false positive through a satisfied evidence requirement;
this is a false negative through the same door. Both are model-judgment
failures the structural gate does not catch, and are stated as such.

Allegation #1 in this run was a Haiku-drafted non-issue (a ternary guard) and
was correctly refuted.

### E2E on R5: 0 of 3 correct

- 1: required field omitted → no bundle
- 2: malformed shape, repair exercised and correctly bounded → no bundle
- 3: bundle emitted, wrong conclusion (false negative)

The repair path behaved as designed in attempt 2. The revision does **not**
make GLM-5.2 reliable on this case, and no run here is presented as if it did.

### Consequence for the render check

GLM produced no **partial** bundle (supported + unresolved) in three attempts,
so a real partial review from GLM is not available to render. The render check
below therefore uses (a) the real attempt-3 bundle (a complete review) and
(b) a bundle in the exact shape the CLI's emission struct produces, carrying
gate results from a **stubbed** model — rendered through the real server
`buildReviewBody`. (b) verifies the server renders a partial review correctly;
it does not verify that GLM produces one.
