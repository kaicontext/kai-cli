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
