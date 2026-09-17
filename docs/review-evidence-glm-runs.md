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

## CLI → bundle → server rendering, observed (not inferred)

Rendered through the real server path — `json.Unmarshal` into `findingView`,
`countReview`, `buildReviewBody`, headline via `reviewIncomplete` — using a
throwaway, uncommitted harness (deleted after the run).

### (a) The real GLM bundle from e2e R5 attempt 3 (a complete review)

Renders cleanly and **wrongly**: headline "it does what it says, and nothing
jumped out at me ✅", "4/5 — your call, then merge", "0 confirmed findings, 2
refuted". The change contains a real defect that GLM refuted with experiments
cited. The renderer displayed the verdict it was given correctly; the verdict
was incorrect. That is the substantive risk: a false negative rendered to a
reader as a clean review. No `withheldRemedy` text appears (the server drops
the `challenge` record as an unknown field).

### (b) A partial review in the CLI's emission shape (model STUBBED)

GLM produced no partial bundle in three e2e attempts, so this input carries
gate results from a stubbed model. It verifies the server renders a partial
review correctly; it says nothing about GLM. All three required properties
held in the rendered body:

1. **Unresolved claims keep the review incomplete** — headline: "This review
   did not finish, so treat the change as _not reviewed_ …⚠️"; readiness 3/5;
   summary "1 confirmed finding, 1 unresolved. Review incomplete".
2. **Supported findings remain visible** — the `### …after a successful cd`
   section renders with its **Remedy:** line, and the incomplete banner lists
   the unresolved allegation with its reason and "No remedy is published for
   them".
3. **`withheldRemedy` never appears as actionable advice** — the withheld text
   ("Single-quote the path…") is absent from the rendered body, as is the
   literal `withheldRemedy`.

**Fixture artifact, stated precisely:** the rendered partial body also showed
the raw `===REVIEW-DATA===` coda inside "Full read-through". That is because
my fixture put the *full* assembled string in `review`. The real CLI emits
`prose` — the text before the marker — as `review`, and the real attempt-3
render confirms no coda appears. This is an inaccuracy in my fixture, not a
pipeline defect.

**Server wording note (out of scope, not changed):** for a *partial* review
the headline says "treat the change as _not reviewed_" even though a confirmed
finding is present. The summary line directly beneath carries the true counts,
so the information is there, but the headline overstates. A kai-server wording
refinement for the partial case, not made here.

## Bottom line for this evaluation

| configuration | GLM-5.2 result |
|---|---|
| #418 / #429 without sandbox | 2/2 first-attempt PASS (correctly unresolved + incomplete) |
| #429 with Node sandbox, unit specimen | 3/4 PASS; 1 FAIL reason not captured; the pass executed `JSON.stringify` under node |
| #418 with Node sandbox, unit specimen | 2/3 PASS; 1 FAIL published the FALSE cd claim as supported (experiment misread) |
| e2e CLI, #429 scratch repo, R5 | 0/3 correct: field omitted; malformed twice (repair bounded correctly); false negative emitted |
| render, real complete bundle | displays correctly; verdict itself wrong |
| render, partial (stubbed) | all three properties hold |

The structural gate works as designed: provenance, status coherence,
remedy gating, incomplete status through CLI/bundle/server. It does **not**
make GLM-5.2 reliable on these cases, and it cannot catch a cited experiment
whose output the model misreads — in either direction. Not ready to merge.

## Forensics — the false NEGATIVE (e2e R5 attempt 3: real JSON.stringify defect refuted)

Source: the saved full stderr/bundle of that run, plus deterministic replays of
GLM's own scripts in the real Node container (`cmd/kai/review_commit_forensics_test.go`,
`KAI_REVIEW_SANDBOX_TEST=1`). All four replays passed with their stated expected
results.

**1. What exact script ran?** GLM cited two experiments for the refutation.

- *Source 2* (fully recoverable): a Node script printing `JSON.stringify(path)`
  and the resulting `cd <json> && echo test` for eight paths (`$dollar`,
  `` `backticks` ``, `$(command)`, quotes, backslash…). It only *prints*; it
  never executes a cd.
- *Source 3* (first ~36 lines recoverable; the tail, including the cited output
  lines 50–62, was cut by the 40-line experiment-log bound): it `mkdir`s
  directories literally named `test$dir`, ``test`dir` ``, `test$(dir)`, then
  runs `sh -c 'cd "/tmp/test\$dir" && pwd && echo "SUCCESS: dollar"'` and the
  same for backticks and `$(…)`. **The paths inside the double quotes carry
  backslash-escaped metacharacters (`\$`, `` \` ``).** It also used a `[[ … ]]`
  bashism (invalid in the container's POSIX sh) in a later block.

**2. What did its output establish?**

- Source 2's output shows `cd "/path/with$dollar" && echo test`,
  ``cd "/path/with`backticks`" && echo test``, `cd "/path/with$(command)" && echo test`
  — the metacharacters pass through **unescaped** — while `\` and `"` are
  escaped. Replay confirmed (`TestForensicsGLMSource2ShowsMetacharactersUnescaped`).
  This is evidence **for** the allegation.
- Source 3's "Tests 2–4" print `SUCCESS: dollar/backticks/command-sub`. Replay
  confirmed (`TestForensicsGLMEscapedCDTestsSucceedBecauseTheyAreHandEscaped`).
  They succeed because `\$` and `` \` `` inside double quotes are literals in
  POSIX sh — i.e. the experiment cd'd into the literal directories.

**3. Did it reproduce the alleged behavior?** **No.** `JSON.stringify` never
emits the backslashes Source 3 relied on, so Source 3 tested a hand-escaped
string, not the string the code produces. The corrected counterpart — building
`'cd ' + JSON.stringify("/tmp/test$dir") + ' && pwd'` with the real output and
running it — is misdirected: `$dir` expands, the cd **fails (exit 2)**, `pwd` is
never reached, while a single-quoted control reaches the literal directory
(`TestForensicsRealJSONStringifyOutputMisdirectsCD`). GLM never ran that
variant. The alleged behavior reproduces exactly as alleged.

**4. Where did the conclusion diverge from the result?** Three separable faults:

- **Bad experiment construction (Source 3):** the input under test was not the
  code's output. Its SUCCESS lines answer a question that was not asked.
- **Misreading of a sound experiment (Source 2):** its output plainly shows the
  unescaped metacharacters; GLM cited those lines as showing the quoting is safe.
- **A false prior overriding the evidence:** GLM's stated reason —
  "double-quoted strings in POSIX sh prevent expansion of these characters
  (backticks and $() are not expanded inside double quotes)" — is the opposite
  of POSIX behavior. Replay: inside double quotes `$HOME` expands and a backtick
  runs (`TestForensicsDoubleQuotesDoNotSuppressExpansion`). This belief is what
  turned "the characters pass through" into "therefore it is safe."

**Diagnosability gap (not changed now):** the experiment log is bounded to 40
lines, which cut Source 3's tail and the very lines GLM cited (50–62). The
tool-call input (the full script) is not logged separately. That bound is too
small for forensics; recorded here rather than changed, per "stop changing the
gate for now."

## Forensics — the false POSITIVE (#418 attempt 3: false `cd` allegation supported)

**The original run's script is not recoverable**: my output filter dropped the
`record=` line and the experiment content. A fresh collection with full output
captured is recorded below if a false positive recurs; any such instance is a
*new occurrence*, not the original.

### Fresh #418 collection — with Node sandbox, GLM-5.2, `-count=3`, full output saved (`a3938f1`)

No false positive recurred, so the false-positive forensics remain
**unrecoverable**. I am not sampling further. The three attempts failed in
three *different* ways, none of them a wrong verdict:

| attempt | experiments | outcome |
|---|---|---|
| 1 | 3 | **3-minute challenge deadline expired** before any submission (`context deadline exceeded`). No verdict. |
| 2 | 4 | `submit_review` payload unparseable (began with a letter `R` — prose inside the tool input). The single format repair **fired**; the resubmission parsed, then failed a **substantive** check ("omitted reasoning, duplicated a check, or checked an unknown issue") and was correctly **not** retried. No bundle. |
| 3 | 4 | Submission parsed; failed the same substantive check outright. No bundle. |

Across everything recorded in this file, GLM-5.2 has now exhibited eight
distinct failure modes on these two cases: readiness contradiction; prose
final answer; experiments run but not cited; `checks` as a string; required
field omitted; prose inside the tool payload; a substantive check-shape
failure; deadline expiry — plus the two wrong verdicts with valid citations and
real experiments.

**Diagnosability gaps (recorded, not changed):**

- For a substantive validation failure the submitted `issue` strings are not
  logged, so whether GLM altered the bullet text (a common cause of "unknown
  issue" — e.g. a changed dash or a paraphrase), duplicated a check, or left a
  reason empty **cannot be told from the record**.
- That single error message conflates three different conditions.
- The 40-line experiment-log bound cut the very lines GLM cited in the
  false-negative run.

## Fidelity revision `06bb76c` — GLM-5.2, Node sandbox, full output saved

Same model, same image, no code changes between attempts. Each attempt reported
separately. "Wrong verdict" = a supported/refuted verdict contrary to the
truth of the allegation.

### Unit specimens, `-count=3` (`glm-fidelity-unit.log`)

| case | attempt | experiments (mode) | final statuses | outcome |
|---|---|---|---|---|
| #418 | 1 | 3 construct (`cd "/tmp/$HOME" && pwd` → pwd `/tmp`) | — | **3-minute challenge deadline expired** before submission |
| #429 | 1 | 3 construct (`cd "/tmp/ws$HOME"`, `` cd "/tmp/ws`pwd`" `` → exit 2, pwd `/tmp`, assertions **FAILED**) | unresolved | defect reproduced, but GLM's assertions encoded the wrong expectation (that the cd succeeds); rule refused a verdict → **unresolved, no wrong verdict** |
| #418 | 2 | 3 construct (CR-joined multi-line, exit 2) | unresolved, unresolved | no wrong verdict; real escaping defect not established |
| #429 | 2 | 2 construct (`$HOME`, `` `echo PWNED` `` → exit 2, assertions FAILED) | unresolved | same as #429/1: defect reproduced, expectation wrong → **unresolved** |
| #418 | 3 | 4 construct incl. `cd "/tmp/$XYZ" && pwd` → pwd `/tmp/expanded_real`, assertions **PASSED** | unresolved (false cd), **supported** (escaping) | **the real defect correctly supported** by a well-designed passing assertion; false cd left unresolved rather than refuted |
| #429 | 3 | 1 script (no assertions) | — | cut off by `go test`'s 10-minute overall limit — a harness artifact, not an outcome |

**Five completed attempts, zero wrong verdicts** (the previous revision: two
wrong verdicts in six). The dominant outcome shifted from *wrong* to
*unresolved*: GLM repeatedly asserted that the `cd` would succeed, the harness
reported FAIL, and the rule correctly refused to let a failed-assertion
experiment back any verdict. That is the intended safety property. It is
**not** correctness: strict expectations were met in 1 of 5.

### End-to-end CLI, #429 scratch repo, 3 attempts (`e2e-429-F-a{1,2,3}-*`)

| attempt | outcome |
|---|---|
| 1 | Bundle emitted, exit 0. GLM used fidelity mode correctly — real construction, generated command executed verbatim, 3 assertions all PASSED. **But the input was a path containing a double quote (`/tmp/test"dir`), which `JSON.stringify` does escape.** Its passing assertions backed the conclusion that the quoting is safe; remedy published: "No code change needed." The Haiku-drafted allegation itself was self-negating ("JSON.stringify does escape them correctly… 'safely' is true"). **A faithful experiment on a benign input produced a substantively wrong conclusion.** |
| 2 | No bundle. GLM misused construct mode — printed several commands and labelled prose as the "command" (exit 127) — then requested a 5th `review_shell`, over the cap → fatal. |
| 3 | No bundle. One script, one construct that emitted a `node -e …` wrapper as the "command" (exit 1), then a 5th call → fatal. |

### What this establishes, and what it does not

- **Execution fidelity is now enforced.** The model cannot substitute a
  reconstructed or hand-escaped command; the harness runs the code's own
  construction and records everything. A failed assertion structurally cannot
  back a verdict, and on the unit specimens that converted every would-be wrong
  verdict into *unresolved*.
- **Input adequacy is not enforced, and cannot be by this mechanism.** e2e
  attempt 1 is the residual: the model chose a character the code handles
  (`"`) rather than the ones it does not (`$`, backtick). The harness has no
  semantic knowledge of which input matters. This is the next design question,
  not solved here.
- **Wrong-expectation assertions** are the common failure: GLM asserts success
  and observes failure. The rule handles it safely (unresolved), but a model
  that understood the observed failure *is* the defect would have asserted
  `pwd_not`/`exit != 0` and supported it — as it did once (#418/3).
- **Other limitations observed, recorded not changed:** a 5th `review_shell`
  after the cap is fatal rather than answered with an error result; the
  3-minute challenge budget is tight for 3–4 fidelity experiments (~40–60s
  each with model turns); the `go test` 10-minute limit truncates a
  `-count=3` run.

Still unmerged. The original #418 false positive remains unexplained.

## Corrections to this record (after review of the fidelity results)

Three statements above were too strong. Corrected here; the earlier text is
left in place so the correction is visible.

1. **"Zero wrong verdicts on the unit specimens" is subordinate evidence.** The
   end-to-end run is the one that mirrors production, and it produced a wrong
   "safe" verdict. That is the result that matters. The unit-specimen outcome
   shows one thing: when the model's own assertion fails, the rule stops
   publication — a safety property, not correctness.

2. **"Execution fidelity is enforced" was overstated.** What is enforced: the
   model cannot alter the generated string between its own construction and
   execution. The construction code is still model-authored, and nothing
   verifies it matches the source under review. That is a separate,
   undemonstrated risk — by its own account, the e2e run's constructor matched
   the code. **The demonstrated cause of that wrong verdict was inadequate
   input selection**, not mismatched construction: the model tested a double
   quote, which `JSON.stringify` escapes, and its passing assertions were taken
   as a general claim of safety about `$` and backticks.

3. **"All assertions passed" is not necessary for useful evidence.** The
   implemented rule discards a completed experiment whose assertion failed. But
   if the requirement is "run in the exact workspace directory," a failed
   directory-equality assertion *demonstrates* the defect. The system should
   preserve that observation and distinguish it from a **broken** experiment
   (did not run; record incomplete). Several unit-specimen attempts above were
   exactly this case — the defect reproduced, the model's success-expectation
   failed, and the observation was thrown away as "unresolved." Safe, but
   evidence lost.

## The open design question

> How do we connect the allegation, the actual code, the tested input, and the
> observed result — without letting a passing example become a general claim
> of safety?

No universal automatic answer is promised. Two practical directions, recorded
for decision rather than implemented:

- **For this case:** tests over the *actual source* covering the alleged `$` and
  backtick behavior — evaluate the real construction expression from the cited
  line with those inputs, rather than a model-written analog on an input of the
  model's choosing.
- **More broadly:** when an experiment does not address the allegation — wrong
  input class, wrong code path — the reviewer should **report that as a
  limitation**, not treat the experiment as a refutation. A passing example on
  input X establishes behavior for X; it cannot refute a claim about Y.

Implementation is paused. Still unmerged. The original #418 false positive
remains unexplained.

## Verdict contract (`942b928`): what the preserved-run validation does and does not show

### The preserved-run test's inputs, field by field

From the ORIGINAL run (extracted by script from its bundle; never retyped):
the allegation text; GLM's verdict `supported`; GLM's reason ("JSON.stringify
properly escapes…"); GLM's remedy ("No code change needed…"); GLM's evidence
`[{source 1, lines 29-30}, {source 4, lines 13-24, experiment}]` — **with no
relevance fields, because none existed**; the experiment record: mode
`construct`, setup `mkdir -p '/tmp/test"dir'`, the construct code, generated
command `cd "/tmp/test\"dir" && echo "successfully changed to: $(pwd)"`, exit
0, observed pwd `/tmp/test"dir`, stdout, and three assertions (exit 0, stdout
contains, pwd equals) all passed.

SUPPLIED BY THE TEST (not produced by the model in that run):
- `outcome=completed` on the record — the field postdates the run;
- `addresses_allegation`, `covers_alleged_inputs`, `expectation="intended"`,
  `tested="a path containing a double quote"` on the citation;
- `requires_runtime=true` on the check (the run's allegation carried it);
- the verdict under test — `supported` as GLM gave it, and `refuted` as its
  reasoning expresses — plus scope / intent / merge_ready / finding fixtures.

So the test shows what the rules do to that record **given those
classifications**. It does not show that GLM would produce them.

### Outputs, scenario by scenario (`go test -run TestWrongVerdictRun -v`)

| | verdict | covers | derived observation | result | reason (abridged) |
|---|---|---|---|---|---|
| A | supported | **honest: false** | conformance | **unresolved**; remedy withheld | supported requires a relevant experiment that observed the violation; none did; a passing example on other inputs establishes behavior for that input only |
| B | refuted | **honest: false** | conformance | **unresolved**; remedy withheld | refuted requires conformance on the alleged inputs themselves; tested: a double quote |
| C | supported | **dishonest: true** | conformance | **unresolved**; remedy withheld | no violation observed — a false coverage claim cannot manufacture support |
| D | refuted | **dishonest: true** | conformance | **refuted** | the remaining path to the wrong "safe" conclusion; the false `covers` and the `tested` text are recorded on the citation |

Honest coverage (A, B): the record can neither support nor refute; the review
is incomplete. Dishonest coverage (C, D): it still cannot support (C), but it
CAN refute (D) — the gate cannot check the coverage claim, only record it.

### Can an unrelated failed assertion support an allegation? **Yes — known gap.**

`TestKnownGapUnrelatedFailedAssertionCanSupportAllegation` passes, which
characterizes the flaw: under `expectation="intended"` the observation is
derived from ANY failed assertion. With the directory-equality check PASSING
(the alleged behavior intact) and an unrelated `stdout_contains` failing, the
gate derives "violation observed" and `supported` goes through. The converse
direction is safe by construction — `expectation="defect"` requires ALL
assertions to pass, so an unrelated failure blocks rather than manufactures
support. The observation is not tied to the specific assertion that encodes
the alleged behavior. Not fixed here; recorded for decision.

### Real GLM CLI → bundle → render, verdict-contract revision, 3 attempts

Model supplying its own relevance fields; no retries added; full output saved
(`e2e-429-V-a{1,2,3}-*`).

| attempt | experiments | outcome |
|---|---|---|
| 1 | 1 script, 2 construct (`cd "/tmp/ws-test" && echo 'hello'`; an "INJECTED" echo test) | substantive check failure ("omitted reasoning, duplicated a check, or checked an unknown issue"). **No bundle.** |
| 2 | 2 script, 2 construct (`echo hello`; `BUILT_COMMAND_START…END` labels as the command, exit 127) | 5th `review_shell` over the cap → fatal. **No bundle.** |
| 3 | 1 script | empty `scope` → fail closed. **No bundle.** |

**No attempt reached a verdict, so no relevance field was ever supplied by
the model, the render step could not run, and there is no live evidence
about whether GLM populates the contract sensibly.** The original failure is
NOT shown fixed. What is shown: the rules block the preserved record under
test-supplied classifications (A–C), one dishonest path remains (D), and the
derivation has an exploitable gap (unrelated failed assertion).

## Diagnosis of the three live failures (verdict-contract revision), from their full logs

What the logs contain: the harness's console lines only — the fast-pass model
substitution, a bounded summary of each experiment, and the final error. They
do **not** contain the rejected `submit_review` payload, the draft's ISSUES
list the challenger was given, the full tool results the model received, or
the model's reasoning. Two of the three rejections therefore lost their
rejected values; that is stated per attempt.

| attempt | error | origin | exact rejected field | what the model received | determinable? |
|---|---|---|---|---|---|
| 1 | `challenge omitted reasoning, duplicated a check, or checked an unknown issue` | **validator** (structural check on `checks`), on the model's submission | one of: an `issue` string not in the draft; a duplicated `issue`; an empty `reason` — **the message conflates them and the payload was not logged, so which one is not determinable** | the draft + the Haiku-drafted ISSUES bullets, sources, and three experiment results (one construct all-passed, one not) | **no** — rejected value lost |
| 2 | `challenge requested unavailable tool "review_shell" (call 5; maximum 4)` | **tool contract** — the four-call cap enforced by the harness; not the validator | the fifth tool call itself; no submission was made | four experiment results: two script-mode (no assertions); two construct-mode in which the model's `construct` printed `echo hello`, then label text `BUILT_COMMAND_START…END` as the "command" (exit 127) | **yes** — the model misused construct mode (did not print the command the code builds), then asked for a fifth experiment; the cap made that fatal |
| 3 | `challenge did not state what it reviewed (empty scope)` | **validator** (required-field check), on the model's submission | `scope` — empty or all-blank; **whether GLM sent `[]`, `[""]`, or omitted the key is not determinable** (payload not logged) | one script-mode experiment result (no assertions) | **partly** — the field is known; its value is lost |

Classification: attempt 2 is a model response failure against the tool
contract, fully determinable. Attempts 1 and 3 are validator rejections of the
model's submission whose rejected values were never captured — a
diagnosability failure on our side, not a protocol conclusion. No retry was
added and no validation was loosened in reaction.

**Diagnosability changes made (not protocol, not loosening):** the validator
now logs the rejected submission payload in full on any structural rejection;
and the single conflated message is split into three that name the offending
value ("checked an issue the draft does not contain: …", "checked the same
issue twice: …", "gave no reasoning for issue …"), likewise for decisions. The
next such run will show exactly what was rejected.

**Assertion-selection bug fixed** (same commit): each experiment citation
now names the recorded assertion(s) it offers; the observation is derived from
those only; the others are preserved on the record; regression added where
directory equality passes but an unrelated stdout check fails — the directory
allegation is not supported. What the model offers is recorded on the
citation, so offering an unrelated assertion is visible.

## Frozen revision `3ed4584` — three CLI → bundle → render attempts, fully captured

**Freeze verified:** binary sha256 `ee46819c25f78093…` identical before and
after all three attempts; working tree clean at `3ed4584`; no change between
attempts.

**Capture method (no code change):** a local logging reverse proxy in front of
the provider base URL, reached by running the CLI under a scratch `HOME` whose
`~/.kai/credentials.json` is a *copy* with `server_url` rewritten to the proxy
(the real file was verified untouched). Every request body — the fast-pass
prompt, the challenger's system prompt, the numbered allegations and decisions,
the numbered sources, each tool result the model received — and every response
body (verbatim; gzip on the wire, decoded cleanly) is preserved per attempt in
`capture/attempt-{1,2,3}/NNN-{request,response}.txt`, plus `stderr.log`,
`bundle.json`, and `rawdump.log`. Authorization headers redacted in the request
files only.

### CORRECTION TO THIS ENTIRE RECORD: the e2e challenger was not GLM

The captured challenge requests (`002-request.txt` onward — the calls carrying
the `submit_review`/`review_shell` tools) all say **`model=anthropic/claude-haiku-4-5`**.
Confirmed in code: `review_commit.go:346–350` computes
`fastModel := rcFastModel(model, provKind)` and passes it to
`rcRunFastReview`, whose line `review_commit_fast.go:195` hands that same
`model` to `rcChallengeReview`. On the fast path the challenge therefore runs
on the **fast** model. With `KAI_REVIEW_MODEL=z-ai/glm-5.2`, GLM is a reasoning
model, so the fast model — and thus the challenger — was substituted to
`anthropic/claude-haiku-4-5`. The stderr line only says "fast pass uses …";
I assumed the challenge used the review model. It did not.

Consequences, stated plainly:
- **Every "e2e" row in this record — including the preserved wrong-verdict run
  (`e2e-429-F-a1`) and its validation fixture — was challenged by Haiku, not
  GLM-5.2.** The wrong "safe" verdict was a Haiku verdict.
- The **unit-specimen** rows (`TestReviewChallengeLiveDesktop4xx`) called
  `rcChallengeReview` with the review model directly and **were** GLM-5.2.
- This is pre-existing fast-path behavior (since the challenge was introduced),
  not something changed in this branch; the error is in my reporting of the
  model, which the capture requirement exposed. To challenge with GLM on the
  fast path one would set `KAI_FAST_MODEL` (which also changes the draft
  author) or change what the fast path passes to the challenge — neither done
  here (code frozen).

### Exact challenger input, per attempt (from `002-request.txt`)

| attempt | ISSUES TO CHECK | DECISIONS TO ASSESS | sources |
|---|---|---|---|
| 1 | (a) `app.js:8` — `window.Panels` not checked before `Panels.setOpen(true)`; (b) `app.js:9` — JSON.stringify quoting is not safe for POSIX sh (`$`/backticks) | none | SOURCE 1 (36 lines: the diff) |
| 2 | (a) `app.js:8` — `workspace()` called unconditionally inside the ternary | one: "the change alters which directory terminal commands execute in…" | SOURCE 1 |
| 3 | (a) `app.js:8` — `workspace()` assumed to return a string | one: "the change assumes commands prefixed with `cd <path> &&` will work in the terminal's POSIX sh…" | SOURCE 1 |

Experiment sources were added per turn (SOURCE 2, 3, …) and each tool result
the model received is in the captured requests.

### Completion, per attempt

| attempt | exit | bundle | outcome |
|---|---|---|---|
| 1 | 1 | **emitted** (15,954 B), `incomplete`, readiness 2 | both allegations unresolved; 2 completed experiments + 2 not-run records preserved |
| 2 | 1 | **emitted** (13,240 B), `incomplete`, readiness 2 | one allegation unresolved; the draft decision assessed (supported) and published under "Decisions (need your call)" |
| 3 | 1 | **none** | validator rejection: `challenge did not assess every draft decision` — rejected payload captured in full |

Completion: 2 of 3 emitted a bundle (both incomplete); 1 of 3 rejected.

### Correctness, per attempt (separately)

- **Attempt 1, allegation (b) — the real defect.** The model did what the
  contract asks: fidelity mode, setup creating a literal `/tmp/test$dir`,
  construct producing `cd "/tmp/test$dir" && cat file.txt`, real execution —
  **exit 2, observed pwd `/tmp`: the defect reproduced.** It supplied the
  relevance fields itself: `addresses=true`, `covers=true`,
  `tested="Shell interpretation of JSON.stringify-quoted path with $"`,
  `assertions=[1, 3]` (#1 `stdout_contains "intended"`, #3 `pwd == /tmp/test$dir`
  — both FAILED). It declared **`expectation="defect"`**. Those offered
  assertions encode the *intended* behavior, not the defect; under "defect",
  their failure derives *conformance*, and the allegation ended **unresolved**,
  remedy withheld. No wrong verdict was published — but the reproduced defect
  was lost to a one-word misdeclaration of direction. That bit is model
  judgment; the fields made it visible and cannot check it.
  Allegation (a) cited only the source with `requires_runtime=true` → unresolved.
- **Attempt 2.** The draft did not allege the JSON.stringify defect, so it was
  not tested. Allegation (a) cited a script-mode experiment (no assertions) →
  unresolved, reason recorded. The construct experiment (uncited) had no setup
  and a nonsensical assertion (stdout contains the command text).
- **Attempt 3.** No verdict. The draft gave 1 issue + 1 decision; the model
  submitted 1 check (issue text matched exactly, `refuted`) and **no
  `decisions`**. Rejected field: `decisions`, absent. Origin: the model's
  response, against a requirement stated in the prompt ("exactly one decision
  entry per DECISIONS bullet") but not marked required in the tool schema.
  Determinable now because the payload is logged.

### Renders (real server path, throwaway harness, deleted after)

- Attempt 1: headline "This review did not finish, so treat the change as
  _not reviewed_ …⚠️", 2/5, "0 confirmed findings, 2 unresolved"; both
  allegations listed under the incomplete banner with "No remedy is published";
  scope and limitations shown. No withheld remedy text appears.
- Attempt 2: same headline shape, "0 confirmed findings, 1 unresolved"; the
  supported draft decision rendered under "## Decisions (need your call)" and
  in the coda.
- Attempt 3: no bundle to render.

### What this run establishes, and does not

Completion on the frozen revision: 2/3. Correctness: no wrong verdict was
published in 3 attempts; the one time the real defect was reproduced with the
model's own relevance fields, a misdeclared `expectation` turned it into
*unresolved*. None of this is GLM-5.2 evidence on the e2e path (see the
correction above). Still unmerged; code frozen.

## Revision `9a4a1ff` — challenger on the configured review model; three fully captured attempts

**Changes since `3ed4584` (scoped, then frozen for the run):** the fast path
passes the draft model and the challenge model separately, so the challenge
uses the configured review model; each result records per-phase configured /
requested / upstream provider (served model left empty — unknown to the
process); regression `TestFastDraftDoesNotSubstituteChallenger`; the
submission schema now requires `decisions` (matching the validator), which
was the exact cause of the earlier attempt-3 rejection. Expectation
interpretation unchanged; deadline unchanged; no retries.

**Freeze verified:** binary sha256 `eac35eb4a6d3b0da…` identical before and
after; tree clean at `9a4a1ff`; no change between attempts. Same capture
method as before (proxy via scratch `HOME`; real credentials untouched).

### Which model performed each phase — requested vs. effective

| attempt | phase | requested (request body) | effective (response `model`) | upstream `provider` |
|---|---|---|---|---|
| 1 | draft | `anthropic/claude-haiku-4-5` | `anthropic/claude-haiku-4.5` | Azure |
| 1 | challenge (2 calls) | `z-ai/glm-5.2` | **`z-ai/glm-5.2`** | DeepInfra |
| 2 | draft | `anthropic/claude-haiku-4-5` | `anthropic/claude-haiku-4.5` | Azure |
| 2 | challenge (3 calls) | `z-ai/glm-5.2` | **`z-ai/glm-5.2`** | DeepInfra |
| 3 | draft | `anthropic/claude-haiku-4-5` | `anthropic/claude-haiku-4.5` | Azure |
| 3 | challenge (1 call) | `z-ai/glm-5.2` | **`z-ai/glm-5.2`** | StreamLake |

Effective is **confirmed** from response metadata for every call. The
bundles' `models` records match (configured `z-ai/glm-5.2` for both phases;
draft requested Haiku; challenge requested GLM; providers as above; served
empty as designed). **GLM-5.2 performed every challenge.**

### Exact challenger input (from the captured challenge request)

| attempt | ISSUES TO CHECK | DECISIONS TO ASSESS |
|---|---|---|
| 1 | `app.js:8` — `typeof window.Panels.workspace === "function"` does not guard `window.Panels` being undefined | one: commands now run inside the workspace directory |
| 2 | `app.js:8` — `workspace()`'s return value is trusted directly into a shell command via string concatenation; the guard checks only that it is callable | none |
| 3 | `app.js:9` — the guard passes only if `workspace` is a function, but the code calls it as if callable | one: workspace path prefixed via JSON string quoting |

### Completion

| attempt | exit | bundle | status |
|---|---|---|---|
| 1 | 0 | emitted (8,847 B) | complete; readiness 4 |
| 2 | 1 | emitted (9,785 B) | **incomplete**; readiness 2; one allegation unresolved |
| 3 | 0 | emitted (6,886 B) | complete; readiness 4 |

**3 of 3 emitted a bundle.** No timeouts. No validator rejections — the
decisions-schema mismatch that rejected the earlier attempt 3 did not recur;
attempts 1 and 3 assessed their draft decision (both `supported`).

### Confirmed defects: **0 of 3**

No attempt published a `supported` verdict on a real defect.

### Missed defects (the known `$`/backtick JSON.stringify defect): **3 of 3**

- **Attempt 1 — draft omission.** The draft never alleged the quoting defect
  (it alleged a false guard problem). Missed defect attributed to the draft;
  challenger handling of the defect **not exercised**. Published review:
  "0 confirmed findings, 1 refuted… 4/5 — your call, then merge" on a change
  with a real shell-quoting defect.
- **Attempt 2 — challenger.** The draft alleged the defect class in a related
  form ("return value trusted directly into a shell command via string
  concatenation"). GLM tested a literal `` /tmp/ws`whoami` `` directory and a
  newline path in fidelity mode — **exit 2, observed pwd `/tmp`: the defect
  reproduced** — but supplied **none** of the relevance fields (no
  `addresses_allegation`, `covers_alleged_inputs`, `expectation`, or offered
  assertions) and asserted nonsense (that stdout would contain the command
  text). Gate: "no cited experiment addresses the allegation" → **unresolved**,
  review incomplete, remedy withheld. Challenger exercised; defect missed.
- **Attempt 3 — draft omission.** The draft alleged a false guard problem; the
  quoting appears only inside the draft's *decision* ("prefixed via JSON string
  quoting"), as a design choice, not a defect. Challenger **not exercised** on
  the defect. Published: "0 confirmed, 1 refuted… 4/5".

### Incorrect verdicts: **0 of 3**

Attempts 1 and 3 `refuted` the Panels-guard allegations with source
citations (`requires_runtime=false`). Both refutations are correct: the code
is `window.Panels && typeof window.Panels.workspace === "function"`, so
`window.Panels` is guarded by short-circuit, and the guard and the call are
consistent. Attempt 2 published no verdict. Both `supported` decisions are
genuine design choices in the change.

### Renders (real server path; harness deleted after)

Attempts 1 and 3: "4/5 — your call, then merge", "0 confirmed findings, 1
refuted", decision under "Decisions (need your call)". Attempt 2: "This review
did not finish… treat the change as _not reviewed_", 2/5, the unresolved
allegation listed with "No remedy is published". All three display their
verdicts correctly; two of those verdicts are clean reviews of a defective
change.

### What this establishes

With GLM confirmed as the challenger and the schema aligned, completion is
3/3 and no incorrect verdict was published. Review *effectiveness* on the
known defect is 0/3: twice the Haiku draft never alleged it, and the one time
the defect class was alleged and reproduced, GLM did not connect the
experiment to the allegation and the gate — correctly under the contract —
left it unresolved. The relevance fields are a contract GLM did not follow in
that attempt. Still unmerged.

## Fast vs. deep review workflows — differences beyond model selection (audited from code before reading deep results)

These are the differences a fast-vs-deep comparison must account for, so a
difference in outcome is not attributed to the model when the workflow itself
differs. Sources: `cmd/kai/review_commit.go`, `review_commit_fast.go`,
`review_commit_challenge.go` at `9a4a1ff`.

| aspect | fast path (default) | deep path (`--deep`) |
|---|---|---|
| prerequisite | none | a captured graph (`kai capture`); refused otherwise (`review_commit.go:279`) |
| draft author | ONE provider call over the diff, `max_tokens` 2000, budget `rcFastBudget()` (1m40s here); the draft model may be substituted for a reasoning review model | an **agent harness** run (`agent.Run`, `agent.ModeReview`, `ReadOnly`) with the read-only tool set plus `kai_impact`/`kai_diff`, graph context, soft budget 9 min (+ extension), hard deadline 20 min; drafts with the review model |
| intent step | none (stated intent only) | `rcInferIntent` — an additional model call reconstructing the author's intent before the review |
| coverage gate | none | a follow-up agent pass when changed files were never opened (`review_commit.go:905–933`) |
| conclusion fallback | none | `rcConcludeFromTranscript` when the run ends without a usable coda |
| challenge sources | the single fast-pass user prompt (the diff) | `rcChallengeSources(transcript)`: every successful tool result the agent gathered, plus the first user message |
| challenge model | configured review model (since `9a4a1ff`; previously the draft substitute) | configured review model (unchanged) |
| readiness | capped at 4 (`rcCapFastReadiness`); issues filtered (`rcFilterFastIssues`) | uncapped; unfiltered |
| incomplete accounting | challenge failure → error, no bundle (workflow continues to deep) | `rcIncomplete` record: finish reason, turns, files read; a run that dies still emits an incomplete bundle |
| coverage record | none | `coverage` (files read, turns, seconds) shipped in the bundle |

**Capture configuration for the deep attempts:** `kai capture` run in the
scratch repo `e2e-429` at `c478d2f` with the frozen reviewer binary
(sha256 `eac35eb4a6d3b0da…`, built from `9a4a1ff`), default flags, under the
scratch `HOME`. Result: snapshot `b8f5add6821e` (1 file). The store resolved
to `.git/kai/db.sqlite` (kaipath rule 3: a fresh init in a git repo goes under
`.git/kai`), sha256 `405812b180494d54…`, 163,840 bytes. The real `~/.kai` was
untouched. Each attempt records the store's hash before it runs so the same
captured state is verifiable.

## Deep path at `9a4a1ff` — GLM drafting *and* challenging; three fully captured attempts

Same frozen binary (sha256 `eac35eb4a6d3b0da…` before and after), same
scratch repo at `c478d2f`, same proxy capture, `--deep`, review model
`z-ai/glm-5.2` for every phase. No gate change between attempts. Captures in
`capture3/attempt-{1,2,3}/` (every request/response, stderr, raw dump, bundle).

### Graph identity — what "the same captured state" does and does not mean

| | before 1 | after 1 | after 2 | after 3 |
|---|---|---|---|---|
| snapshot (`refs.snap.latest`) | `b8f5add6821e` | same | same | same |
| store sha256 | `405812b180494d54…` | `83816f366a33cd77…` | `b5651545b368ee81…` | `277f37d88684d6fb…` |
| store bytes | 163,840 | 208,896 | 233,472 | 253,952 |

The store is **not** byte-identical between attempts. What changed is run
bookkeeping the reviewer writes into the same file: `agent_sessions` (3 rows,
one per attempt), `agent_messages` (48 rows), plus a `runs/<uuid>/` directory
per attempt. What did not change: `refs` (1 row, `snap.latest` → the same
snapshot), `ref_log` (1 row, written by `kai capture` at 13:38:23), and the
`objects/` directory (mtime 13:38:23, before attempt 1 started at 13:39:33).
Row counts for `nodes`/`edges` (6/5 now) were not recorded before attempt 1,
so "graph content unchanged" rests on the untouched objects and the single
ref entry, not on a direct before/after diff. Claim supported: every attempt
reviewed the same snapshot. Claim not supported: identical store bytes.

### Which model performed each phase — requested vs. effective

Every model call in all three attempts requested `z-ai/glm-5.2`. Effective
model is read from the response body's `model` field; upstream from
`provider`.

| attempt | phase | calls | effective | providers |
|---|---|---|---|---|
| 1 | intent | 1 | `z-ai/glm-5.2` | StreamLake |
| 1 | agent draft | 5 | `z-ai/glm-5.2` | Ambient, StreamLake |
| 1 | conclusion from transcript | 2 | request 008: **unknown**; request 009: `z-ai/glm-5.2` | — / StreamLake |
| 1 | challenge | 4 | `z-ai/glm-5.2` | StreamLake, DeepInfra |
| 2 | intent | 1 | `z-ai/glm-5.2` | Ambient |
| 2 | agent draft | 8 | `z-ai/glm-5.2` | Ambient, DeepInfra, StreamLake |
| 2 | conclusion from transcript | 1 | `z-ai/glm-5.2` | StreamLake |
| 2 | challenge | 5 | `z-ai/glm-5.2` | StreamLake, DeepInfra |
| 3 | intent | 1 | `z-ai/glm-5.2` | StreamLake |
| 3 | agent draft | 8 | `z-ai/glm-5.2` | StreamLake, DeepInfra |
| 3 | conclusion from transcript | 1 | `z-ai/glm-5.2` | StreamLake |
| 3 | challenge | 3 | `z-ai/glm-5.2` | StreamLake, DeepInfra |

Two non-model requests per attempt are excluded: one `POST /api/v1/runs/cost`
(HTTP 204, cost reporting). Attempt 1's request 008 is the conclusion call:
the client abandoned it after 2m01s (proxy: `context canceled`) and re-sent a
byte-identical body as request 009, which was answered by GLM. That is
consistent with kai-engine v0.6.73's 120-second HTTP client timeout on
non-streaming completions; it is an engine transport retry below the gate,
not a gate retry, and no response ever arrived for 008 — so its effective
model is **unknown**, as the rule requires. The bundles' `models` records
agree (configured and requested `z-ai/glm-5.2` for both phases; challenge
provider StreamLake / StreamLake / DeepInfra; served left unknown by design).
**GLM-5.2 drafted and challenged in every attempt.**

### Completion (separate from verdicts)

| attempt | exit | bundle | status | review wall time | draft turns / s | conclusion fallback | first `submit_review` | repair nudge |
|---|---|---|---|---|---|---|---|---|
| 1 | 1 | emitted (22,312 B) | **incomplete** — 2 unresolved | 6m50s | 6 / 104 | fired | unparseable (`invalid character 'R'`, prose) | fired once → accepted |
| 2 | 1 | emitted (23,734 B) | **incomplete** — 2 unresolved | 3m47s | 9 / 115 | fired | accepted | — |
| 3 | 1 | emitted (20,099 B) | **incomplete** — 1 unresolved | 2m28s | 9 / 53 | fired | accepted | — |

Bundles emitted 3/3; **completed reviews 0/3** (an incomplete bundle is not a
completed review); timeouts 0 (the 3-minute challenge deadline was never
reached); validator rejections 0. First-submission outcome: 2 of 3 accepted
as submitted; attempt 1's first payload was prose and was accepted only after
the single permitted format repair. The agent run ended without a usable coda
in all three attempts (`finish=end_turn`), so `rcConcludeFromTranscript`
produced every draft. Every exit was 1: CI would fail the run on all three.

### Draft detection — did the deep draft allege the known defect? **3 of 3**

Exact allegations as handed to the challenger:

- **Attempt 1** — #1 `app.js:8-9` "`JSON.stringify` double-quotes don't
  suppress `$`/backtick/`\` expansion in POSIX `sh`; use single-quote
  escaping" (**the known defect**); #2 the prepended `cd` mutates the
  persistent shell's cwd; #3 a failed `cd` silently skips the command. One
  decision (whether `Panels.workspace()` is attacker-influenceable).
- **Attempt 2** — #1 `app.js:9` "`JSON.stringify` quotes for JavaScript, not
  for a POSIX shell; a workspace path with `$`, `` ` ``, `"`… is not safely
  quoted" (**the known defect**); #2 the `wsPath ?` guard conflates empty and
  absent; #3 dependence on host `Panels` behavior outside the repo. No
  decisions.
- **Attempt 3** — #1 `cd <path> && <command>` silently aborts when the
  directory is missing; #2 `app.js:8` "`JSON.stringify` double-quoting is not
  fully POSIX-safe for paths containing `$`, backticks, or `\`" (**the known
  defect**); #3 the panel may not write `command` verbatim. No decisions.

### Evidence interpretation — what the challenger cited and what the gate derived

All experiments ran in fidelity mode against the actual construction
(`'cd ' + JSON.stringify(wsPath) + ' && ' + command`), and the model supplied
the relevance fields itself in every citation. Details that matter:

- **Attempt 1, #1 supported.** Two citations, both `expectation=intended`,
  `addresses=true`, `covers=true`. Source 10 offered assertions 1–2; #2
  `stdout_contains "/tmp/test"` **failed** — after `mkdir` of the literal
  `` /tmp/test$(echo pwned) ``, `` /tmp/test`echo backtick` `` and
  `/tmp/test$VAR` directories, none was entered; only `/tmp/a\b` printed. That
  is a genuine observed misdirection on the alleged inputs. Source 9 offered
  assertions 1–3, all of the form `stdout_contains 'cd "/tmp/test$(echo
  pwned)" && pwd'` — asserting the *command text* would appear in stdout,
  which is never true; their failure was recorded as a violation under the
  contract but is not evidence. **The verdict is correct; it rests on source
  10; one of the two citations is ill-formed.** #2 and #3 (`cd` mutates cwd;
  silent skip): the model answered `supported` but cited only experiments
  whose offered assertions passed under `intended` (conformance) → gate
  "supported requires a relevant experiment that observed the alleged
  violation; none did" → **unresolved**, remedies withheld. Decision assessed
  `supported`.
- **Attempt 2, #1 supported.** Three citations, all `expectation=defect`.
  Sources 16 and 17 ran `` cd "/tmp/ws$dir`whoami`" && echo hello `` and
  `` cd "/tmp/ws$HOME/injected-`echo whoami`" && pwd `` — **exit 2, pwd
  `/tmp`, the misdirection itself** — but the model's offered assertions were
  intended-behavior assertions (exit 0; pwd equals the literal path) labelled
  as the defect, so their failure was derived as *conformance* ("the alleged
  behavior was not observed"). Source 18 (`` cd "/tmp/ws/injected`echo`" && ls
  `` landed in `/tmp/ws/injected` and listed the marker file — backtick
  substitution executed) offered assertions 1–3, all passed under `defect` →
  **violation**. **The verdict is correct; it rests on source 18 alone; two of
  three citations were mislabelled in a way that turned real misdirection into
  "conformance".** A fourth experiment is preserved as `not_run` (setup
  failed: `touch /tmp/ws/injected/markerfile: No such file or directory`) and
  was not cited. #2 (`requires_runtime=false`) and #3 were the model's own
  `unresolved` verdicts (host behavior outside the repo), not gate downgrades.
- **Attempt 3, #2 supported (the known defect).** Sources 16 and 17,
  `expectation=intended`, offered assertions 1–3 each (`exit 0`,
  `stdout_contains` the literal path, `pwd` equals the literal path): every
  one failed — exit 2, pwd `/tmp` — for `/tmp/ws$dir` and `` /tmp/ws`back` ``
  after `mkdir` of the literal names. **Violation observed on exactly the
  alleged inputs; both citations well-formed.** #1 supported via source 18
  (`cd "/bad/missing/path" && echo COMMAND_RAN` → exit 2, no output; offered
  `exit 0` and `stdout_contains COMMAND_RAN` both failed under `intended`): the
  observation matches the allegation; whether "`&&` aborts on a missing
  directory" is a defect or the design is arguable, and the published
  model-authored remedy (`cd … ; <command>`) would drop the guard. Source 15
  (four commands separated by literal `---` lines, exit 127 because `---` ran
  as a command) was not cited. #3 the model's own `unresolved`.

### Final published recommendation (real server render; harness deleted after)

| attempt | headline | readiness | counts | findings published with remedy |
|---|---|---|---|---|
| 1 | "This review did not finish, so treat the change as _not reviewed_" | 3/5 — small fixes first | 1 confirmed, 2 unresolved | JSON.stringify quoting (single-quote `'\''` remedy) |
| 2 | same banner | 2/5 — needs work | 1 confirmed, 2 unresolved | JSON.stringify quoting (single-quote remedy) |
| 3 | same banner | 2/5 — needs work | 2 confirmed, 1 unresolved | `cd &&` silent abort (remedy: validate or `cd … ;`); JSON.stringify quoting (single-quote remedy) |

Each render lists its unresolved allegations under "No remedy is published
for them". Attempt 1 also renders its decision under "Decisions (need your
call)".

### Counts

| outcome | deep, GLM/GLM | fast, Haiku draft / GLM challenge (`9a4a1ff`, above) |
|---|---|---|
| bundles emitted | 3/3 | 3/3 |
| **completed reviews** | **0/3** (all incomplete) | 2/3 |
| timeouts / rejections | 0 / 0 | 0 / 0 |
| draft alleged the known defect | **3/3** | **1/3** (attempt 2; corrected — the first version of this table said 0/3) |
| confirmed defects (known defect `supported`) | **3/3** | 0/3 |
| missed defects | 0/3 | 3/3 (2 draft omission, 1 challenger) |
| incorrect verdicts | **1/3** — attempt 3 #1, adjudicated in the review section below (corrected from 0/3) | 0/3 |
| published readiness | 3, 2, 2 | 4, 2, 4 |

Qualifications on the 3/3: attempt 1's support rests on one of its two
citations; attempt 2's on one of three; only attempt 3's citations are all
well-formed. The contract's "an observed violation can support" carried the
correct verdict in all three, and no mislabelled or ill-formed citation
produced a wrong one here — but two of the three verdicts would have been
`unresolved` without the single sound citation each happened to include.

### Fast vs. deep — what the comparison can and cannot say

Differences beyond model selection are tabulated in the previous section.
The ones that plausibly acted here:

- **Draft detection (0/3 → 3/3)** cannot be attributed to the model alone. On
  the fast path the draft was Haiku over the diff in one call; on the deep
  path it was GLM inside the review harness with tool access (6–9 turns, the
  file opened each time), an intent step, and the conclusion-from-transcript
  fallback — which in fact authored every deep draft. No run drafted with GLM
  over the bare diff, so the model and the workflow are confounded for this
  outcome.
- **Challenger effectiveness** is the same model on both paths (GLM confirmed
  from metadata). The fast-path challenger, exercised once, supplied no
  relevance fields; the deep-path challenger supplied them in all nine
  citations. The deep challenge received 8–18 sources (the agent's tool
  results) versus the fast path's single diff prompt. n=1 vs n=3 — not a
  measured effect.
- **Completion** moved the other way: the deep drafts alleged host-dependent
  behavior (`Panels.workspace()`, panel write semantics) that cannot be
  resolved from this repo, and each such allegation keeps the review
  incomplete and the exit nonzero. Completed reviews were 2/3 fast and 0/3
  deep. This is a property of what the deep draft alleges, not of the gate.
- Wall time: 2m28s–6m50s deep versus a single draft call plus challenge on
  the fast path.

Still unmerged; no gate changes were made for or between these attempts.

## Review of the deep-path captures (no new runs; corrections and adjudication)

### Corrections to the section above

1. **Fast draft detection was 1/3, not 0/3.** Fast attempt 2's draft alleged
   the defect ("return value trusted directly into a shell command via string
   concatenation"); the challenger reproduced it and did not connect the
   experiment. Zero were confirmed. The comparison table is corrected.
2. **"Incorrect verdicts 0/3" was not established.** Attempt 3's #1
   (missing-directory) finding is adjudicated below as an incorrect verdict
   with a harmful remedy. Deep incorrect verdicts: **1/3**.
3. **Sound evidence for the quoting verdict does not validate the other
   citations.** Attempt 1 accepted an ill-formed citation and attempt 2
   accepted two reversed-label citations as observations. Both counted; both
   were wrong; the verdict survived because one other citation was sound.
   Two of three runs accepted incorrectly interpreted observations.

### 1. Each unresolved allegation against the available source

| attempt / # | allegation | what the source settles | class | why unresolved |
|---|---|---|---|---|
| 1 / #2 | the prepended `cd` leaves the persistent shell's cwd changed | Readable from the change itself: line 11's comment declares a *persistent* POSIX sh and line 9 emits `cd X && cmd`. No host context needed. The experiment (source 11: `cd dir && pwd ; echo --- ; pwd` → second `pwd` still in `dir`) **observed exactly this**, but the citation labelled those assertions `intended` and they passed → derived "conformance" → gate turned the model's `supported` into unresolved. | **unsupported classification** — a design consequence of "cd into the workspace" in a persistent terminal, DECISION-shaped, not a defect; and mislabelled evidence | gate downgrade (correct under the rules; the model's own label defeated its own verdict) |
| 1 / #3 | a failed `cd` **silently** skips the command "with no clear indication of why" | The cited record (source 10) has on its **stderr** `cd: line 1: can't cd to /tmp/testpwned: No such file or directory` (×3). "Silently / no indication" is contradicted by the model's own experiment; it asserted only `exit 0` and never looked at stderr. | **unsupported assumption** (contradicted by the record) | gate downgrade — offered assertion passed |
| 2 / #2 | the `wsPath ?` guard treats `""` as "no workspace"; whether the host can return `""` is unknowable here | Line 9's behavior on `""` is the pre-change behavior (bare command). Whether `Panels.workspace()` returns `""` is a host property. | **missing context**, speculative input shape; not a defect claim about this repo's code | model's own `unverified` |
| 2 / #3 | depends on `Panels.workspace()` existing and on the panel writing the string verbatim to POSIX sh | The existence half is refuted by the source (line 8 guards `typeof … === "function"`). The verbatim-write half is the change's own stated premise (line 11 comment) **and the premise of the supported quoting finding** in the same review. | **missing context** — a scope limitation, not an allegation | model's own `unverified` |
| 3 / #3 | the panel may re-parse or re-quote `command` | Same premise as above; nothing in the repo can settle it. | **missing context** — scope limitation | model's own `unverified` |

**Why each one makes the whole review incomplete.**
`rcChallengeResult.Incomplete = len(unresolved) > 0`
(`review_commit_challenge.go:800`), which the CLI turns into
`incomplete=true`, exit 1, and the "treat the change as _not reviewed_"
banner. That rule was agreed for allegations whose runtime evidence *could*
exist and was not obtained. Three of the five unresolved items here can
**never** be resolved by this reviewer: they are host-contract limitations,
and the same limitation is the premise the review's own confirmed finding
rests on. A review that says "confirmed: quoting is unsafe *if* the panel
writes verbatim; limitation: the panel is outside this repo" is complete as
far as the repo allows. Filed as allegations, the same content keeps CI red
on this change forever.

**Where they come from.** The deep drafter is instructed to produce them and
given nowhere else to put them. `review_commit.go:64`: "If the change's
correctness rests on something outside your reach, that IS a finding — say
what you could not see and what breaks if it is false." `review_commit.go:60`
and `:98` say the opposite ("a finding you would hedge … 'couldn't verify' is
not a finding — confirm it or drop it"; "a concern you could not verify is
not a defect"). The conclusion nudge (`review_commit.go:1472`) says "state
unresolved questions as limitations", but the REVIEW-DATA coda has no
limitations slot — the parser accepts `issues`/`findings`, `decisions`,
`intent_match`, `merge_ready`, `summary` (`review_commit.go:1648–1688`) —
and only ISSUES and DECISIONS reach the challenger. So an out-of-reach
dependency can survive only as an ISSUE, every ISSUE is an allegation, and
every unresolvable allegation is an incomplete review. All three deep drafts
complied with line 64. The fast prompt, by contrast, forbids exactly this
("depends on", "if X then", "assuming" → not an issue;
`review_commit_fast.go:75`), which is one reason the fast path completed 2/3
and the deep path 0/3.

### 2. The missing-directory finding (attempt 3 #1) against the requirement

- **Requirement** (commit subject, in the challenger's SOURCE 1 line 2, and
  the change's comment): "cd into the workspace before running the command …
  so the command always runs in the correct directory."
- **Allegation:** `cd <path> && <command>` "silently aborts the command when
  the workspace directory is missing or stale, where the old code ran it
  unconditionally".
- **What was observed** (source 18): `cd "/bad/missing/path" && echo
  COMMAND_RAN` → exit 2, empty stdout, stderr `can't cd to /bad/missing/path:
  No such file or directory`.
- **Adjudication.** The observed behavior is the requirement's behavior for
  that case: when the workspace cannot be entered, the command is not run in
  some other directory. "The old code ran it unconditionally" describes the
  hazard the change exists to remove, not a regression. "Silently" is
  contradicted by the recorded stderr. The challenger's `expectation=intended`
  citation declared `exit 0` and `COMMAND_RAN` — i.e. *running the command
  anyway* — as the intended behavior, which inverts the requirement. Its own
  reason concedes reachability is unknown ("depends on whether `workspace()`
  can return a non-existent path, which is outside this repo"), so it
  supported a conditional allegation on an unverified condition.
  **Verdict: incorrect.** Published as "confirmed finding" in a 2/5 review.
- **Remedy** ("switch to `cd … ; <command>` to preserve unconditional
  execution"): would run the command in whatever directory the persistent
  shell was in whenever the workspace is missing — the exact wrong-directory
  execution the change prevents, and for a destructive command the worst
  case. "Validate the path before prefixing" duplicates what `cd &&` already
  does. **Harmful remedy, published.** The gate's only remedy rule is
  "published iff supported"; remedy content is model-authored and unchecked,
  as the contract states.
- Attempt 1's #3 is the same allegation; it ended unresolved only because
  that citation's offered assertion (`exit 0` on a multi-command script whose
  last `cd` succeeded) passed. Its withheld remedy ("surface the cd failure
  to the UI") is harmless.
- **Mechanism.** The contract verifies that an alleged *behavior* was
  observed. It has no notion of whether that behavior is contrary to the
  requirement: "intended" is whatever the challenger declares in the
  `expectation` label, and nothing relates it to the stated intent that the
  same challenger had in SOURCE 1 and answered `intent_match: verified` on.

### 3. Why invalid assertions and reversed labels counted toward the quoting verdict

`rcExperimentRecord.observation` (`review_commit_sandbox.go:133–173`) reads
only three things: the offered indexes, each offered assertion's
`Passed`, and the `expectation` label. Under `intended`, any offered failure
is a violation; under `defect`, all-offered-passed is a violation and any
failure is conformance. `rcResolveCitations` (`review_commit_challenge.go:
572–588`) then sets `rel.Violation` from that, and `supported` stands when
`rel.Violation` is true (`:686`). Nothing examines *what* an assertion
asserts or whether the label matches its content — by design ("relevance,
coverage, expectation, selection are model judgment — recorded, not
checkable").

- **Attempt 1, source 9 (ill-formed):** offered assertions 1–3 were
  `stdout_contains 'cd "/tmp/test$(echo pwned)" && pwd'` etc. — the
  *command text*, which `pwd`'s output can never contain. They failed for
  that reason alone; `observation("intended", [1,2,3])` returned
  `violation`; `rel.Violation` became true on the first citation processed.
  Source 10's genuine failure (`stdout_contains "/tmp/test"`) made the same
  bit true again. **Had source 9 been the only citation, an assertion that
  cannot pass under any behavior would have supported the defect.** The same
  pattern (asserting stdout contains the command text) appeared in fast
  attempt 2 at `9a4a1ff` — twice across runs, so it is a GLM habit, not a
  one-off. The value is a verbatim line of `GeneratedCommand`; this instance
  is mechanically detectable, though the general class (assertions vacuous
  for any behavior) is not.
- **Attempt 2, sources 16/17 (reversed label):** the model wrote assertions
  encoding the *intended* outcome (`exit 0`; `pwd` equals the literal
  `` /tmp/ws$dir`whoami` ``) and labelled them `defect`, because it *expected*
  the defect to show. They failed (exit 2, pwd `/tmp` — the defect showing) →
  `observation("defect", …)` returned *conformance* "the alleged behavior was
  not observed". Its own `tested` prose says the opposite ("shell expanded
  `$dir` and ran `whoami`"). The field name `expectation` invites the reading
  "what I expect to happen"; the prompt defines it as "what the assertions
  encode" (`review_commit_challenge.go:44`). Two of three citations were
  thereby inverted. It did not cost the verdict because source 18 carried a
  violation and `supported` needs only one. **The same inversion under a
  `refuted` verdict is the R5-attempt-3 false negative mechanism** (a
  reproduced defect published as refuted): rule (2) blocks refutation only
  when *some* citation reports a violation, so a refutation whose every
  citation is reversed-labelled still passes. That exposure is unchanged.

### The smallest change these findings justify

Ranked by what each finding shows about the *system* (as opposed to the
model):

- Finding 1 is caused by the reviewer's own instructions: line 64 orders
  out-of-reach dependencies to be filed as findings, and the coda gives them
  no other home. Every deep review of a change with an external dependency is
  therefore incomplete by construction. Deterministic, system-owned, and it
  decides completion (0/3).
- Finding 2 is a model judgment the gate cannot adjudicate (what the
  requirement intends); the gate did what the contract says. What the system
  can do is stop hiding the premise: the published finding shows neither
  what was declared "intended" nor which assertions were offered.
- Finding 3 is a known, documented limit of the contract; the one
  mechanically detectable instance (an assertion whose value is a line of
  the generated command) is narrow, and the reversal is not detectable.

**Chosen: route out-of-reach dependencies into a draft `LIMITATIONS` list
instead of `ISSUES`.** Concretely: the REVIEW-DATA coda accepts
`LIMITATIONS:` bullets; `review_commit.go:64` and the conclusion nudge send
"something outside your reach that the change's correctness rests on" there
(say what could not be seen and what breaks if it is false — same content,
different slot); the parser carries them, the challenge receives them as
draft limitations to preserve (not to check), and the assembled review
publishes them under Limitations verbatim as draft-authored coverage
statements, the same class as the challenger's scope/limitations. ISSUES
remain defect allegations about this repo's code. No change to the
challenge protocol, the evidence rules, the deadline, or retries; a
limitation is not an allegation and cannot be supported, refuted, or carry a
remedy.

What it would have done to these captures: attempt 2 → complete, 1 correct
finding; attempt 3 → complete, but with the incorrect #1 published as
confirmed (finding 2 is untouched by this change and would then ship in a
*complete* review — stated plainly); attempt 1 → still incomplete on #2/#3,
which are gate downgrades of mislabelled evidence, not limitations. It does
not address findings 2 or 3. Regression: a draft coda whose LIMITATIONS entry
names a host dependency publishes it under Limitations, produces no
allegation, and the review is complete; the same sentence under ISSUES still
becomes an unresolvable allegation.

**Not chosen, and why.** Publishing each supported finding's premise
(`tested`, offered assertions, label) would make finding 2 auditable but
not prevent it, and the server drops the `challenge` record, so it would
reach only the CLI/text output until Atlas renders the structured record.
Rejecting assertions whose value is a line of the generated command fixes one
detectable instance of a class that is mostly undetectable. Renaming
`expectation` to something like `assertions_encode` is a protocol change
made on n=1 evidence of the misreading. All three are recorded here, not
made.

**Not implemented.** This is the choice; the code is unchanged at `233e874`.

**Decision (after review): not adopted.** Automatic limitations-to-complete
is not to be implemented. The missing-directory case is preserved instead as
a regression, and the question becomes how the review assesses behavior and
remedies against the requirement — below.

## The missing-directory regression (preserved; red by design)

`cmd/kai/testdata/pr429/deep-attempt3-missing-dir.json` holds the
challenger's exact inputs from deep attempt 3 — the requirement (commit
subject "play button: cd into the workspace before running the command" and
the author's comment "so the command always runs in the correct directory"),
the three ISSUES as handed over, all 18 sources it received, the four
recorded experiments (source 18: `cd "/bad/missing/path" && echo
COMMAND_RAN` → exit 2, empty stdout, stderr `cd: can't cd to
/bad/missing/path: No such file or directory`), and the raw `submit_review`
payload GLM submitted (verdict `supported`, `expectation=intended` with
offered assertions `exit 0` and `stdout_contains COMMAND_RAN`, remedy
"…switch to `cd ... ; <command>` to preserve unconditional execution").
`review_commit_missingdir_test.go`:

| test | asserts | state at `58d8658`+ |
|---|---|---|
| `TestMissingDirCaseFixtureIsTheOneWeThink` | the fixture is that run: requirement, allegation text, record 18's exit/pwd/stdout/stderr, the four experiment sources are byte-for-byte `render()` of the records (after the prompt's trailing-newline trim), the submission supported the allegation with the `cd … ;` remedy and declared "runs anyway" intended | **green** |
| `TestStoppingAfterFailedCdIsNotPublishedAsADefect` | replaying the exact submission over the exact inputs: the allegation is not `supported`; no remedy is published for it; the assembled review does not carry `cd ... ; <command>` | **red** — all three assertions fail today: published as a confirmed defect, remedy published, review carries it |
| `TestMissingDirCaseKeepsTheQuotingVerdict` | the same replay keeps allegation 2 (the quoting defect) `supported` with its remedy | **green** |

The red test is the acceptance condition. It is not skipped and not
inverted; it fails until the review assesses behavior against the
requirement. The green guard says any change that turns it green must not
lose the correct verdict in the same payload.

## Proposal: assessing behavior and remedies against the requirement

**Principle.** The gate today proves that an alleged *behavior* was observed.
A *defect* is a behavior contrary to the requirement. Today "intended" is
whatever the challenger writes in the `expectation` label, anchored to
nothing; in the preserved case it anchored "intended" to the pre-change code
(its reason cites "Source 1 line 29", the diff's `-` line) and inverted the
requirement. The proposal makes three things explicit and checks
mechanically what can be checked. Each part says what is mechanical and what
remains model judgment; nothing here is implemented.

### Part 1 — the requirement is a distinct source; a supported runtime defect cites the clause it violates

- **Sources are classed by the system.** SOURCE 1 today is one blob the
  system composes: AUTHOR CONTEXT (the commit subject/body), the resolved
  symbol locations, INTENT (the `rcInferIntent` reconstruction), then the
  DIFF. The system knows which lines are which, so it can publish them as
  separate sources with a class: **requirement** (author context), **intent
  reconstruction** (a model's paraphrase — see the caveat below),
  **author's claim** (comments inside the diff), **code** (the diff and
  files), **exploration** (tool results), **experiment**. No new submission
  field: classes are properties of the sources the system already numbers.
- **Rule.** A `supported` verdict on a runtime allegation must include a
  citation into a *requirement*-class source; the citation is the clause the
  observed violation contradicts. Citations into code, the diff's `-` lines,
  the author's comments, or the intent reconstruction do not satisfy it.
- **Mechanical:** the citation resolves; its source is requirement-class.
  **Judgment:** whether the observed behavior actually contradicts the cited
  clause. **Published:** the clause, verbatim, under the finding.
- **Caveat found while evaluating:** the reconstructed INTENT the challenger
  received (SOURCE 1 line 13) says the path is "safely quoted using
  `JSON.stringify`" — the intent step laundered the author's comment into
  the requirement. If the reconstruction counted as requirement-class, a
  challenger could cite it to *refute* the quoting defect. So the
  reconstruction must not be requirement-class, and `rcInferIntent` should
  be told to state goals, not mechanisms. That is a finding about the intent
  step independent of this proposal.

### Part 2 — a regression-shaped claim is a behavior change, not a confirmed defect

- **Shape detection, executed.** When a citation's `intended` assertions can
  be run against the *pre-change* construction — the challenger supplies
  `construct_old` from the diff's `-` side, cited, in the same fidelity call
  — and they **pass on the old construction and fail on the new one**, the
  citation is a regression claim: its "intended" is the old behavior.
- **Rule.** A regression-shaped claim is published as a **behavior change**
  under DECISIONS ("the change now does Y where the pre-change code did X, on
  inputs I — observed: old …, new …"), not as a confirmed defect, and it
  carries no remedy. It becomes a defect only when Part 1's requirement
  citation is present *and* the requirement clause is one the old behavior
  satisfied — and that second condition is judgment, so in this iteration a
  regression-shaped claim is a decision, full stop. Trade-off stated: a
  genuine regression (old behavior right per the requirement, new behavior
  wrong) is published as a decision with both observed outcomes, not as a
  confirmed finding, and does not lower readiness by itself.
- **Mechanical:** the shape (pass-on-old, fail-on-new), the published
  outcomes, the reclassification. **Judgment:** that `construct_old` is a
  faithful rendering of the `-` lines (checkable by citation into the diff,
  not semantically).

### Part 3 — a remedy is executed before it is published as a correction

- **Rule.** For a supported runtime allegation, a remedy is published as a
  correction only if it is given as an alternative construction
  (`construct_remedy`), a fidelity experiment ran it on the alleged inputs,
  the assertions offered as `intended` for the supported violation **pass**
  under it, and its observable outcome (exit, pwd, stdout) on those inputs
  is **not identical to the pre-change construction's** — a "remedy" that
  reproduces the old outcome reverts the change for those inputs and is
  published as a question, not a correction. Remedies not expressible as a
  construction (prose such as "validate the path before prefixing") are
  published as *suggestions, unverified*, never as corrections. Source-only
  allegations (`requires_runtime=false`) are unchanged.
- **Mechanical:** the remedy ran; the offered assertions' outcomes; the
  equality with the old outcome. **Judgment:** that the offered assertions
  encode the requirement (the same judgment Part 1 leaves open), and the
  fidelity of `construct_remedy` to the prose remedy.

### Part 4 — "silently" can be asserted

The record already carries stderr, but no assertion kind reads it, so an
allegation of *silence* cannot be tested even when the record refutes it.
Adding `stderr_contains` / `stderr_empty` assertion kinds is a tool-contract
change (not a submission field). **Mechanical:** the outcome. **Judgment:**
offering it. Publishing the record's stderr beside a finding is mechanical
and costs nothing.

### Evaluation on the preserved case — executed, not predicted

The mechanical checks of Parts 2 and 3 were run in the pinned sandbox image
on the case's inputs (`cmd/kai/testdata/pr429/proposal-eval.sh`, output in `proposal-eval.out`, run with `docker run --rm -v $PWD/proposal-eval.sh:/eval.sh:ro node@sha256:c610fcdf… sh /eval.sh`; constructions from the
diff and the two remedies as written):

| construction | input | outcome (stdout / exit / pwd) | stderr |
|---|---|---|---|
| **new** `cd "/bad/missing/path" && echo COMMAND_RAN` | missing dir | — / 2 / `/tmp` | `can't cd to /bad/missing/path` |
| **old** `echo COMMAND_RAN` | missing dir | `COMMAND_RAN` / 0 / `/tmp` | — |
| **remedy** `cd "/bad/missing/path" ; echo COMMAND_RAN` | missing dir | `COMMAND_RAN` / 0 / `/tmp` | `can't cd to /bad/missing/path` |
| **new** `cd "/tmp/ws$dir" && pwd` | literal `/tmp/ws$dir` | — / 2 / `/tmp` | `can't cd to /tmp/ws` |
| **old** `pwd` | same | `/tmp` / 0 / `/tmp` | — |
| **remedy** `cd '/tmp/ws$dir' && pwd` (single-quote escaper) | same | `/tmp/ws$dir` / 0 / `/tmp/ws$dir` | — |
| **new** `` cd "/tmp/ws`back`" && pwd `` | literal `` /tmp/ws`back` `` | — / 2 / `/tmp` | `back: not found`; `can't cd to /tmp/ws` |
| **remedy** `` cd '/tmp/ws`back`' && pwd `` | same | `` /tmp/ws`back` `` / 0 / `` /tmp/ws`back` `` | — |
| single-quote remedy on the missing dir | missing dir | — / 2 / `/tmp` | `can't cd to /bad/missing/path` |

Against the acceptance condition:

- **Allegation 1 (missing directory), as submitted.** Part 1: the only
  requirement anchor in the submission is the `-` line (code class) → the
  `supported` verdict lacks a requirement citation → not published as a
  defect. **Mechanical.** Part 2: the offered `intended` assertions (`exit
  0`, `COMMAND_RAN`) pass on the old construction and fail on the new →
  regression-shaped → published as a behavior change under DECISIONS with
  both outcomes and the stderr, no remedy. **Mechanical**, and it holds even
  if a challenger re-anchors on the subject line and argues contradiction —
  the shape, not the argument, decides. **Acceptance condition 1 met
  mechanically.** Part 4 would additionally let "silently" be tested
  (`stderr_empty` fails), but is not needed for the condition.
- **Its remedy.** `cd … ;` executed on the missing directory produces
  `COMMAND_RAN` / 0 / `/tmp` — **identical to the pre-change outcome** →
  Part 3 refuses it as a correction (published as "restores the pre-change
  behavior for a missing workspace: the command runs in the shell's current
  directory"). "Validate the path before prefixing" is not a construction →
  suggestion, unverified. **Acceptance condition 2 met mechanically.**
- **Allegation 2 (quoting) must survive.** Its `intended` assertions (pwd
  equals the literal path) **fail on the old construction** (old never
  changes directory: pwd `/tmp`) → not regression-shaped → defect path.
  Part 1: SOURCE 1 lines 1–2 (author context) and the author's "always runs
  in the correct directory" are available; the requirement-class clause is
  the subject line — a faithful challenger cites it; whether "misdirected
  for `$`" contradicts "cd into the workspace" is judgment, but it is the
  easy direction. Part 3: the single-quote remedy, executed, enters the
  literal `/tmp/ws$dir` and `` /tmp/ws`back` `` directories (offered
  assertions pass) and differs from the old outcome → published as a
  correction; it also preserves stop-on-failure for a missing directory.
  **The correct verdict and its correction survive** — `TestMissingDirCase
  KeepsTheQuotingVerdict` is the guard.
- **What remains judgment after the proposal, on this case:** whether a
  cited requirement clause is contradicted (Part 1); fidelity of
  `construct_old` and `construct_remedy` to the diff and to the prose remedy
  (both cite-checkable, not semantically); which assertions encode the
  requirement. None of these decided the acceptance condition here — the
  shape and the outcome equality did.
- **Costs.** Up to two more fidelity runs per supported runtime allegation
  (old construction, remedy) inside the existing four-call cap and
  three-minute deadline: in attempt 3 that is 4 + 2 (allegation 2's old
  construct and remedy; allegation 1's old construct shares a call) = 6 >
  4. Either the cap rises for these system-required runs or the challenger
  must plan calls; a run that cannot complete the requirement checks stays
  unresolved. Timeouts remain completion failures. The reconstructed intent
  must be excluded from requirement-class sources (caveat above) or Part 1
  can be turned against a true defect.

### Protocol changes this implies — listed, not made

1. Source classing (system-side; no submission field).
2. `review_shell` fidelity mode gains `construct_old` and `construct_remedy`
   (tool-contract inputs), and `stderr_contains` / `stderr_empty` assertion
   kinds.
3. The submission's existing evidence citations carry the requirement
   citation — a citation into a requirement-class source, distinguished by
   its source class, not by a new field. If a marker proves necessary to
   tell "this citation is the clause violated" from "this citation is
   context", that is the one new field, and the evaluation above is the
   justification to weigh it against.
4. A regression-shaped citation reclassifies its allegation to a decision;
   a remedy without a passing `construct_remedy` run is a suggestion.

The preserved regression is the acceptance test for whichever of these is
built. Nothing above changes the four evidence rules, the deadline, the
one-shot format repair, or the never-flip principle.

## Correction to the proposal: two counterexamples, and the requirement evaluated directly

Two rules above are withdrawn. Both tried to make a *comparison of
outcomes* decide *desirability*, and a comparison cannot do that: old/new
runs supply observations; only the requirement says which observation is
the right one, and reading the requirement is model judgment. The gate's
job is to make that judgment explicit, cited, tested, and published — not
to replace it with a rule that cannot establish correctness.

### Counterexample A — a genuine regression (passes before, fails after)

Construct one on the same change. Suppose the requirement includes "when no
workspace is available the command runs unmodified" (the author's own
description of the fallback), and the change had instead dropped the
command whenever `wsPath` is empty. Pre-change: command runs. Post-change:
nothing runs. That is a real defect against the requirement.

- **Withdrawn Part 2 ("regression-shaped ⇒ behavior change / DECISION")**:
  the `intended` assertion (`stdout_contains COMMAND_RAN` with no workspace)
  passes on the old construction and fails on the new — regression-shaped —
  so the rule would have demoted a genuine defect to a decision with no
  remedy. **Fails the counterexample.** The shape is identical to the
  missing-directory case; what differs is only whether the requirement wants
  the old behavior, which the shape cannot see.
- **Under the corrected proposal (below)**: the challenger states the
  requirement's expected behavior for the tested input — "with no
  workspace, the command runs" — cites the clause, offers
  `stdout_contains COMMAND_RAN` as the intended assertion, the new
  construction fails it → violation → `supported`. The old run may be cited
  as an observation ("the pre-change code ran it") but decides nothing.
  **Preserved.**

### Counterexample B — a valid fix that restores the previous outcome

For the regression in A, the correct remedy is to restore the fallback: run
the command unmodified when `wsPath` is empty. Its outcome on that input is
byte-identical to the pre-change outcome — because the pre-change outcome
was the right one.

- **Withdrawn Part 3 equality rule ("a remedy whose outcome equals the
  pre-change outcome is not a correction")**: would have refused the
  correct fix as "a revert". **Fails the counterexample.** Identity with the
  old outcome is neither evidence for nor against a remedy.
- **Under the corrected proposal**: the remedy is executed on the alleged
  input and the requirement-derived assertion (`COMMAND_RAN`) passes →
  correction. **Preserved.**

### The missing-directory case, evaluated directly against the decisive requirement

Decisive requirement: **the command must not execute in the wrong
directory** (author context: "cd into the workspace before running the
command"; author's comment: "so the command always runs in the correct
directory"). Evaluated on the recorded and executed outcomes
(`proposal-eval.out`), input `/bad/missing/path`, command `echo COMMAND_RAN`:

| behavior | observed | executes in the wrong directory? | against the requirement |
|---|---|---|---|
| original (the change): `cd "/bad/missing/path" && echo COMMAND_RAN` | no `COMMAND_RAN`; exit 2; pwd `/tmp`; stderr `cd: can't cd to /bad/missing/path: No such file or directory` | **no** — it does not execute at all, and says why on stderr | **conforms**. Not silent. |
| remedy `cd "/bad/missing/path" ; echo COMMAND_RAN` | `COMMAND_RAN`; exit 0; pwd `/tmp`; same stderr | **yes** — runs in the shell's current directory, not the workspace | **violates** the decisive requirement |
| remedy "validate the path before prefixing" (prose) | not executable as written | if validation fails, the only conforming action is to not run — which is what `cd &&` already does | redundant at best; unverified |
| the single-quote remedy for the quoting defect, on this input | no `COMMAND_RAN`; exit 2; pwd `/tmp` | no | conforms (does not disturb this behavior) |

The pre-change outcome (`COMMAND_RAN` in `/tmp`) is the same observation as
the `cd … ;` remedy's, and it is *also* a violation of the decisive
requirement — the change exists to remove it. That is what settles the
case, not the fact that remedy and old code agree. The allegation
"silently aborts … where the old code ran it unconditionally" describes the
requirement being met and calls it a defect; the challenger's
`expectation=intended` assertions (`exit 0`, `COMMAND_RAN`) encode the
violation and call it intended. Correct verdict: the allegation is
**refuted** for this input (conformance observed on the alleged input), the
`cd … ;` remedy is not a correction, and "silently" is contradicted by the
record.

### The corrected proposal

What is kept, what is withdrawn, and who decides what:

| element | status | mechanical | model judgment |
|---|---|---|---|
| Source classing: author context = **requirement**; intent reconstruction, author comments, diff, tool results, experiments each their own class (system-side; no field) | kept | which class a source is | — |
| A `supported` or `refuted` runtime verdict must **state the requirement's expected behavior for the tested input** and cite the requirement-class clause it derives from; its offered `intended` assertions are that statement made testable | kept (the one submission-side addition; it replaces nothing and adds one stated sentence + a citation) | the citation resolves and is requirement-class (the preserved submission's anchor, the diff's `-` line, would not qualify); the assertions ran; the observation is derived from them as today | **what the requirement expects for this input** — the decisive judgment. Explicitly the challenger's, and explicitly fallible: a challenger that writes "the command runs anyway" as the requirement's expectation is wrong, and the gate cannot know it |
| Publish, with every runtime finding and every remedy: the cited clause verbatim, the stated expected behavior, the offered assertions with expected/observed, and the record's stderr | kept | yes | — (this is how a wrong judgment becomes visible on the page) |
| A remedy is a **correction** only if executed on the alleged inputs and the *same* requirement-derived assertions pass under it; otherwise it is a **suggestion, unverified** | kept | it ran; the assertions' outcomes | the assertions (same judgment as above) |
| Old/new comparison (`construct_old`) | **withdrawn as a rule**; permitted as an *observation* the challenger may cite | — | — |
| Remedy-outcome equality with the pre-change outcome | **withdrawn** | — | — |
| `stderr_contains` / `stderr_empty` assertion kinds | kept (tool contract) | outcome | offering it |
| Experiment budget | **unchanged** (four calls, three minutes). A remedy that cannot be executed within the budget is a suggestion, not a correction. No system-required runs are added | — | how to spend the four calls |

**What this establishes for the preserved regression.** With a faithful
requirement statement, the acceptance condition is met: the expected
behavior for a missing directory is "the command does not execute", the
offered assertion (`stdout_not_contains COMMAND_RAN`) passes on the original
→ conformance on the alleged input → refuted; the `cd … ;` remedy fails the
same assertion → not a correction. With the *unfaithful* statement GLM
actually made, the gate does not catch it — and this proposal does not
claim to. What changes is that the finding would then read, on the page:
"requirement cited: *cd into the workspace so the command always runs in the
correct directory*; expected under it: *the command runs even when the
directory is missing*; observed: exit 2, `can't cd to /bad/missing/path`".
The contradiction is published, not buried. Requirement interpretation stays
model judgment, named as such; correctness is not something the gate can
establish, and the record should not say it can.

**On the counterexamples.** Both are preserved because nothing in the
corrected proposal reads the old outcome as a rule: A is supported by a
requirement-derived assertion failing on the new construction; B's fix is a
correction because the same assertion passes under it.

The gate is unchanged; nothing here is implemented; the red regression stays
red; PR #117 stays unmerged.

## The proposal as an evidence-display change: the preserved bad submission, rendered

Treating the corrected proposal as a display improvement only — the gate
frozen, verdicts and remedy publication exactly as today — this is what the
assembled review would show for deep attempt 3's allegation #1, built from
the fixture's fields (`finding`, `remedy`, `reason`, the citation's
`expectation`/`tested`/`assertions`, and the recorded experiment). The
submission predates the proposal, so it carries no requirement statement;
the display says so rather than inventing one. Text in the current format is
unchanged; the added block is marked ▶.

```
**This review did not finish, so treat the change as _not reviewed_ — not as reviewed and clean.** How far it got is below. ⚠️
**Where I'd land: 2/5 — needs work.**
2 confirmed findings, 1 unresolved. Review incomplete; see the unresolved allegations. Intent verified; readiness: needs work.

## Findings

### frontend/dist/app.js:9 — `cd <path> && <command>` silently aborts the command when the workspace directory is missing or stale, where the old code ran it unconditionally; depends on `workspace()` never returning a non-existent path, which I could not read.
When workspace() returns a path to a non-existent directory, `cd "<path>" && <command>` causes cd to fail, && short-circuits, and the command never runs — a regression from the pre-change behavior which ran the command unconditionally without a cd prefix.

▶ **Evidence (runtime; the observation is confirmed, the reading of it is the challenger's):**
▶ - Requirement cited: **none.** Citations: the diff (source 1, lines 24–31) and the code (source 3, lines 8–14). Neither is the author's stated requirement ("play button: cd into the workspace before running the command").
▶ - Expected behavior, as the challenger declared it (`expectation: intended`): exit `0`; stdout contains `COMMAND_RAN` — the command runs when the workspace directory is missing.
▶ - Tested: cd into a non-existent path (/bad/missing/path) via the exact app.js construction; asserted command would run (echo COMMAND_RAN) and exit 0
▶ - Ran (verbatim): `cd "/bad/missing/path" && echo COMMAND_RAN` → exit 2; working directory after: `/tmp`; stdout: (empty)
▶ - stderr: `/tmp/__kai_run.sh: cd: line 1: can't cd to /bad/missing/path: No such file or directory`
▶ - Offered assertions: 1. FAIL exit "0" (observed "2") · 2. FAIL stdout_contains "COMMAND_RAN" (observed "")
▶ - Observation: violation — an offered assertion of the declared intended behavior failed.

**Remedy** ▶ *(model-authored; not executed — unverified)*: Validate the path before prefixing (e.g. check existence), or switch to `cd ... ; <command>` to preserve unconditional execution (dropping the working-directory guarantee when cd fails), or confirm workspace() never returns a non-existent path and document the assumption.
```

What a reader can now see that was hidden: the "intended" behavior the
verdict rests on is *the command runs when the directory is missing*; the
requirement that would make that intended is not cited because none does;
the stderr line contradicts "silently" two lines under the word; the remedy
was never run.

### What would still be published incorrectly

| published element | as rendered | why it is wrong | does the display change it? |
|---|---|---|---|
| headline under `## Findings` | "`cd <path> && <command>` **silently** aborts the command … where the old code ran it unconditionally" | the behavior conforms to the requirement; "silently" is refuted by the record | **no** — a finding's headline is the allegation text; still a confirmed finding |
| count and summary | "**2 confirmed findings**, 1 unresolved" | one of the two is not a defect | **no** — derived from statuses, which are unchanged |
| readiness | "2/5 — needs work" | the model set `merge_ready` from its supported findings, this one included; the gate only caps | **no** |
| finding body | "a regression from the pre-change behavior which ran the command unconditionally" | frames the requirement's behavior as a regression | **no** — model-authored, published on `supported` |
| remedy | `cd ... ; <command>` under **Remedy** | executes the command in the wrong directory — the hazard the change removes | **relabelled only** ("not executed — unverified"); still under Remedy, still reads as the correction |
| intent line | "Intent verified" | correct, but sits beside a finding whose premise inverts that intent | no |
| Atlas / PR comment | the server renders from the bundle's issues and drops the `challenge` record | none of the ▶ block reaches the server surface | **no** — the display change is CLI/text-only until the server renders the structured record |

Net: an author reading the CLI output carefully could catch the false
finding from the evidence block. An author reading the headline, the count,
the readiness, or the Atlas render would not. The red regression
(`TestStoppingAfterFailedCdIsNotPublishedAsADefect`) would still be red
under this display change — by design of the exercise: it is not a fix.

### Decision: inspectable, or prevented?

The acceptance condition is "stopping after a failed cd is **not published
as a confirmed defect**, and the wrong-directory remedy is **not published
as a correction**". That is prevention. The display proposal makes the
judgment inspectable and leaves both published — it fails the acceptance
condition on its own terms. So the intended outcome is **prevention**, and
the display work is worth doing only as a component of something that
prevents, not as the deliverable.

What can prevent it without a mechanical rule that pretends to establish
correctness (the constraint that stands):

1. **Anchor requirement (gate rule, narrow).** A `supported` runtime verdict
   whose expected-behavior statement is missing or cites no requirement-class
   clause is **unresolved**, reason "the intended behavior was not derived
   from the stated requirement". Prevents *this* submission (anchored to the
   diff's `-` line), not a re-anchored one that cites the subject and still
   declares "runs anyway" intended. Mechanical; makes no correctness claim;
   the unresolved reason is true.
2. **Confirm observations, not defects (labelling rule for the class).** The
   gate can confirm that a behavior was observed; whether it is a defect is
   the requirement reading, which is judgment. Publish runtime findings as
   "**Observed:** with a missing workspace directory, the command does not
   run (exit 2; stderr `can't cd …`). **Challenger's reading:** a regression
   against the pre-change behavior, citing [clause]. **Your call.**" — under
   a heading that is not "confirmed finding", not counted as a confirmed
   defect, remedy published as the challenger's suggestion, never as the
   correction. Prevents the false *defect label* for the whole class by
   construction, including re-anchored submissions. Cost, stated: the true
   quoting defect gets the same label ("Observed: misdirected for `$` and
   backtick; challenger's reading: violates 'runs in the correct
   directory'"), and readiness can no longer be lowered by a runtime finding
   on the challenger's reading alone. Whether that trade is acceptable is
   the design decision — it redefines what "confirmed" means in this review.
3. **A second, narrow judgment** — a separate call that answers only "does
   the stated expected behavior follow from the cited clause?" — is the
   "another judging model" deferred earlier; recorded, not proposed.

**Recommendation:** the intended outcome is prevention; (1) + (2) achieve it
for the case and the class without claiming the gate can establish
correctness; the display block is then the evidence panel under (2), not a
standalone improvement. Not implemented — this is the decision to make
before implementing anything. Gate frozen; regression red; PR unmerged.

## Independent adjudication pass — does a fresh judgment reject the harmful recommendation and keep the valid ones?

A throwaway harness (`cmd/kai/testdata/pr429/adjudication/adjudicate.py`;
not reviewer code; gate untouched) gave a fresh GLM-5.2 call, per case: the
**requirement** in the author's words; the **actual diff**; **recorded
execution results** — the exact generated commands run in the pinned image,
including the pre-change code and the proposed remedy on the same inputs
where executed; the **alleged violation**; the **proposed remedy**. Withheld:
the prior verdict, readiness, the challenger's `expectation` label and
reasoning. It was asked to answer the **violation** and the **remedy**
separately, judged against the requirement, using only the records for
runtime behavior, and to assess each remedy alternative. Same model
configuration as the reviewer (`z-ai/glm-5.2`, reasoning disabled); three
attempts per case; **every request (auth stripped) and raw response is
preserved** under `adjudication/caseN/`; model and provider confirmed from
each response body (`z-ai/glm-5.2`, Ambient, all 12 calls). Two earlier
runs never reached the model (a CA-bundle failure, then Cloudflare 1010 on
the Python user agent) and are kept as logs.

Note what the adjudicator had that the original challenger did not: the
remedy's recorded execution and the pre-change code's recorded execution.

### Cases

| # | case | requirement given | expected |
|---|---|---|---|
| 1 | the preserved false finding (missing directory) | actual: "play button: cd into the workspace before running the command" + author's comment "so the command always runs in the correct directory" | conforms / reject |
| 2 | the real quoting defect (`$`, backtick) | actual | violation / accept |
| 3 | genuine regression + valid restoration: a hypothetical variant whose fallback sends `""` when no workspace is known (executed in the image: nothing runs), requirement adds "when no workspace is known, run the command unmodified, exactly as before"; the remedy restores `: command` — byte-identical to the pre-change outcome | hypothetical, stated | violation / accept |
| 4 | requirement-flipped control: same code and records as case 1, requirement "always run the command … a missing or stale workspace path must never stop the command from running" — does it read the requirement or pattern-match `cd … ;`? | hypothetical, stated | violation / accept `cd … ;` |

### Results (`adjudication/summary.tsv`)

| case | attempt 1 | attempt 2 | attempt 3 |
|---|---|---|---|
| 1 false finding | **cannot_determine** / **reject** (`cd … ;`: reject) | **violation** / **reject** (`cd … ;`: cannot_determine) | **violation** / **accept** (`cd … ;`: **accept**) |
| 2 quoting | violation / accept | violation / accept | violation / accept |
| 3 regression + restoration | violation / accept | violation / accept | violation / accept |
| 4 flipped control | violation / accept (`cd … ;`) | violation / accept (`cd … ;`, also "validate") | violation / accept (`cd … ;`) |

### Determination

- **Valid findings and fixes retained: 9/9.** The quoting defect was found
  and its single-quote remedy accepted every time; the genuine regression
  was found and the restoring fix accepted every time — including that the
  fix's outcome equals the pre-change outcome, which did not count against
  it; the flipped control accepted `cd … ;` every time, so the adjudicator
  reads the requirement rather than pattern-matching the remedy.
- **Harmful recommendation rejected: 2/3; endorsed 1/3.** Attempt 1 rejected
  `cd … ;` for the right reason ("the command runs but is not in the
  workspace"). Attempt 2 rejected the remedy overall while calling `cd … ;`
  undetermined. Attempt 3 **accepted** it, writing that the remedy
  "explicitly acknowledges and accepts dropping the working-directory
  guarantee when cd fails" — the exact hazard, named and waved through.
- **The false violation: adjudicated "conforms" 0/3.** Attempt 1 said the
  requirement does not specify the missing-path case (cannot_determine);
  attempts 2 and 3 read "cd into the workspace **before running the
  command**" as a promise that the command runs, and called not running it a
  violation. None weighted the author's comment "always runs in the correct
  directory" as decisive. The reading I recorded earlier as "the decisive
  requirement" is a reading; given only the author's words, the same model
  read the same sentence three ways.
- **Where the disagreement lives.** Not in the evidence — every attempt
  cited exit 2, empty stdout, the stderr line, and the pre-change record
  correctly — but in the requirement reading. With identical inputs the
  adjudicator's requirement-clause field was "cd into the workspace before
  running the command" three times and its conclusion differed each time.

**Answer to the question asked:** an independent adjudication pass retains
valid findings and fixes reliably in this sample, and rejects the harmful
recommendation more often than not, but it does not reliably prevent the
false finding: one attempt in three would have published it, with the
harmful remedy, on the same recorded evidence. As a gate it would be a
second model judgment with a ~1/3 miss rate on this case (n=3; same model
the reviewer uses; providers uniform). This is the empirical basis for the
decision above: requirement interpretation varied across independent passes
of the same model on the same words, so prevention that does not depend on
a single model's reading is option (2) — confirm the observation, publish
the reading as the challenger's, and leave the requirement call to the
author — or a requirement stated by the author with an explicit "must not"
clause the reviewer can test. Nothing implemented; gate frozen; regression
red; PR #117 unmerged.

### Case 5 — the same case with one explicit clarification

Everything as case 1 — code, records, allegation, remedy text, system
prompt, model, `max_tokens`, reasoning setting (verified field-by-field
against case 1's request) — with one sentence added to the requirement:
*"If the workspace cannot be entered, do not execute the command in another
directory; report the failure."* Three attempts, preserved under
`adjudication/case5/`; `z-ai/glm-5.2` confirmed from each response
(provider StreamLake).

| attempt | violation (label) | what the reasoning concludes | remedy | `cd … ;` alternative | "validate" alternative |
|---|---|---|---|---|---|
| 1 | **violation** | **conforms** — "command suppressed, failure reported via cd's stderr and exit code 2 … So it conforms on this tested input … The recorded result therefore does not show a violation" (the text even says "Wait — re-evaluating" and reverses itself; the label was not updated) | reject | **reject** — "runs the command in another directory (/tmp) when the workspace cannot be entered. This directly violates the requirement" | accept (unrecorded; "the direction can satisfy the requirement") |
| 2 | **violation** | violation on the *report* clause — non-execution is satisfied, but "the recorded result only shows the shell's own stderr diagnostic … no evidence that the application reports the failure to the user" | reject | **reject** — "runs the command unconditionally in /tmp after cd fails, which the requirement forbids" | cannot_determine |
| 3 | **violation** | **conforms** — "matches the requirement's instruction for the cannot-enter case; the unconditional pre-change execution in /tmp is the disallowed behavior … the requirement favors the new behavior" | reject | **reject** — "executing the command in /tmp after cd fails, violating the requirement" | accept (unrecorded) |

**Remedy: the harmful `cd … ;` recommendation was rejected 3/3, each time for
the right reason and citing its recorded execution.** With the clarification
present, the requirement reading that endorsed it in case 1 (attempt 3) did
not recur.

**Alleged defect: label "violation" 3/3; reasoning concludes conformance
2/3.** Attempts 1 and 3 reason their way to "the code conforms" and still
emit `violation` in the answer field — a label/reasoning contradiction in
the adjudicator's own output, the same failure shape as the reviewer's
reversed `expectation` labels: the prose is right and the structured field
is wrong. Attempt 2 reaches `violation` by a defensible reading of "report
the failure": the shell's stderr is the shell reporting, not the
application; whether that counts is a judgment the clarification did not
settle.

What the clarification did and did not do. It fixed the remedy assessment:
"do not execute the command in another directory" is directly testable
against the recorded remedy run (`COMMAND_RAN`, pwd `/tmp`), and all three
attempts tested it. It did not make the violation label reliable: two
attempts contradicted their own reasoning in the field a gate would read,
and one attempt found a new ambiguity ("report") in the clarifying sentence
itself. A gate consuming the `answer` field would still have published this
allegation as a violation 3/3 — with the remedy withheld 3/3. That is the
acceptance condition half met: the harmful correction is not published; the
false defect still is.

Two consequences for the design decision above:

1. An explicit, testable "must not" clause is worth having — it made the
   remedy verdict correct and stable on the recorded evidence. It is not
   sufficient for the violation verdict, because (a) the model's structured
   answer can contradict its own reasoning, and (b) any clarifying sentence
   carries its own readings ("report the failure").
2. The label/reasoning contradiction is the second time in this record that
   a model's structured field inverted its correct prose (reviewer:
   `expectation=defect` on intended-behavior assertions; adjudicator:
   `violation` on reasoning that concludes conformance). A gate that reads
   the label and not the reasoning inherits the inversion. This favors
   option (2) — publish the observation and the *reasoning* as the
   challenger's reading, and do not let a single structured label decide
   "defect" — over adding a second label-emitting judgment.

Nothing implemented; gate frozen; regression red; PR #117 unmerged.
