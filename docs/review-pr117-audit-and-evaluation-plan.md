# PR #117 — diff audit, proposed split, and evaluation plan

Status: audit and plan only. No code split, no gate change, no runs, no merge,
no release. Base is `main` at `df52131` ("Diagnose review citation failures
and allow one bounded correction" — i.e. kai-cli #115 + #116, the exact-quote
challenge gate). Head is `035fdd8`. 32 commits; 62 files; production Go:
`review_commit_challenge.go` (+941/−), `review_commit_sandbox.go` (+411),
`review_commit.go` (+57), `review_commit_fast.go` (+20); 7 test files
(+1,806); testdata; two docs.

Standing rule for everything below: a resolved citation proves location, an
executed experiment proves an observation, a green test proves the code does
what the test says, and a well-formed bundle proves formatting. None of them
is a correct review judgment. "Demonstrated benefit" means a *measured*
outcome on real changes, or a failure class made structurally impossible —
and says which.

---

## 1. Diff audit against the base

### What the base does (so the deltas are read against it)

- Challenge answer = `{review: <model-rewritten full review text>, checks[]}`;
  each check cites `{source, quote}`; the quote must be an exact substring of
  the source (`strings.Contains`) or the **entire review is withheld**, with
  one citation-correction retry.
- Any `unverified` check → `"challenge could not verify an allegation"` →
  **entire review withheld**. There is no per-allegation status and no
  "incomplete" publication.
- The published review is the model's own rewritten prose; the validator
  checks its ISSUES coda matches the supported set and that readiness is
  coherent, else withholds.
- `review_shell` is script-only (free-form sh, printed output, no
  assertions).
- Fast path: the substituted draft model (Haiku) is also the challenger.
- Deep path: same challenge; a challenge failure becomes
  `ChallengeFailure` and an incomplete bundle with **no body**.

### Group A — citation handling and provenance

| element | where | depends on |
|---|---|---|
| Cite by location: `rcCheckEvidence{Source, LineStart, LineEnd}` replaces `{source, quote}` | `review_commit_challenge.go` types; prompt "Cite evidence BY LOCATION"; schema | — |
| Numbered source rendering and extraction from the same coordinate system: `rcSourceLines`, `rcNumberedSource`, `rcExtractCitation` | challenge.go | — |
| Removal of the exact-quote path: `rcCitationError`, `rcValidateOrRepairCitation` (the base's one citation-correction retry) | challenge.go (−) | replaces base #116 |
| Per-citation tolerance: an out-of-range citation is dropped and logged; the check falls to `unresolved` if nothing usable remains | `rcResolveCitations` first branch; `rcValidateChallenge` `len(refs)==0` | **Group B's** per-allegation status |
| Validated reference record `rcCitationRef{Source, LineStart, LineEnd, Experiment, …}` carried in the bundle | challenge.go types; bundle `challenge.allegations[].evidence` | Group B |
| Provenance of models per phase `rcChallengeResult.Models{Draft, Challenge}` (configured / requested / served=unknown / provider) | challenge.go, `review_commit.go`, `review_commit_fast.go` | Group C (routing) supplies the values |
| Provenance of experiments: `Experiments []rcExperimentSource` (complete record incl. `not_run` attempts at source 0) in the bundle | challenge.go `finish`, `rcValidateChallenge` tail | Group D |

Tests: `TestReviewCitationExtractsByLocation`,
`TestReviewChallengeToleratesOneBadCitation`,
`TestReviewChallengeOneInvalidCitationLeavesOnlyThatFindingUnresolved`
(tolerance ones need Group B). Fixture identity tests
(`TestWrongVerdictRunFixtureIsTheOneWeThink`,
`TestMissingDirCaseFixtureIsTheOneWeThink`) check provenance fields
round-trip and that experiment sources equal `render()` of the record.

Demonstrated: the originating failure — one byte-for-byte quote slip
withholding the whole review — is **structurally impossible**: the system
copies the lines. Across the six captured 9a4a1ff runs (3 fast, 3 deep) with
GLM confirmed from metadata: 32 citations in emitted bundles, **0 dropped**,
0 validator rejections. This is a failure class removed, not a judgment
improved. Model routing provenance found a real misattribution (every
"GLM" challenge before 9a4a1ff was Haiku).

Known failures / limits: a resolved citation says nothing about relevance
(the deep-run citations of the diff's `-` line as "requirement" resolved
fine). The served model is not exposed by the provider layer (`Served` is
always empty; only the captured response body confirmed GLM). None of this
is a quality claim.

### Group B — structured results and system-assembled publication (not one of the three named groups; both C and E depend on it, so it is audited on its own)

| element | where |
|---|---|
| `rcChallengeAnswer` without `review`/summary: `scope[]`, `limitations[]`, `intent_match`, `merge_ready`, `checks[]`, `decisions[]` | challenge.go types, prompt, schema |
| Per-allegation final result `rcAllegationResult{ID, Issue, Status supported/refuted/unresolved, RequiresRuntime, Evidence, Reason, Remedy, WithheldRemedy}`; per-decision `rcDecisionResult` | challenge.go |
| `requires_runtime` mandatory (`*bool`); `unverified` → `unresolved`; runtime verdict with no experiment → unresolved with reason | `rcValidateChallenge` |
| Remedy attached to its allegation; published only when supported, else `WithheldRemedy` | `rcValidateChallenge` |
| Decisions drawn from the draft, assessed with evidence; challenger cannot add one | `rcValidateChallenge` decisions block |
| Readiness only ever **capped** (SmallFixes if any supported; DecideThenMerge if decisions or unresolved); never raised | `rcValidateChallenge` |
| Derived `SUMMARY` (`rcDeriveSummary`); assembled review (`rcAssembleReview`): Scope, Findings (+Remedy), incomplete banner with reasons, Limitations, Decisions, single coda | challenge.go |
| `Incomplete = len(unresolved) > 0` → bundle `incomplete`, CLI `Status: INCOMPLETE`, exit 1; empty record omitted | `review_commit.go` |
| Sandbox unavailable → error tool result + prompt NOTE, not fatal (runtime claims stay unresolved; supported ones still publish) | `rcChallengeReview` |
| Bundle field `challenge` (the whole structured record) | `review_commit.go` |

Tests: `TestReviewChallengeSummaryIsDerivedNotModelAuthored`,
`…RecordsFinalVerdictAndPerClaimReason`, `…ClampsIncoherentReadinessInsteadOfWithholding`,
`…AssessesDraftDecisions`, `…UnresolvedCDCannotPublishBraceAdviceUnderDecisions`,
`…AbsentExperimentIsDowngradedAndRemedyWithheld`, `…UnavailableSandboxIsNotFatal`,
`…SkipsDraftWithoutIssuesOrDecisions`, `TestPartialResultsReachEmittedBundleAsIncomplete`,
`TestFastReviewReportsUnresolved`.

Demonstrated: 6/6 captured bundles were emitted and rendered through the
real server path (`buildReviewBody`) with correct counts/banners; the
downgraded finding's prose cannot reappear (assembly is from statuses).
Model-authored text still published: the allegation headline, the finding
description, the remedy (all gated by status, none checked for content).

Known failures / limits: **incomplete by construction** for any change with
a host-dependent allegation (deep 0/3 complete — 3 of 5 unresolved were
scope limitations filed as ISSUES because the *drafter* prompt says to
(`review_commit.go:64`, unchanged in this PR) and the coda has no
limitations slot); the server **drops the `challenge` field** (no Atlas
rendering of the structured record); a supported allegation's **remedy is
unchecked** (the preserved harmful `cd … ;` remedy was published under
**Remedy**); readiness is the model's from its own supported findings, only
capped.

### Group C — model routing, schema consistency, diagnostics

| element | where |
|---|---|
| Fast path: `challengeModel` separate from the draft model; challenge uses the configured review model | `review_commit_fast.go`, `review_commit.go` |
| Per-phase model record populated (fast: draft requested + provider; deep: draft = review model); `Configured` stamped by the caller | fast.go, review_commit.go |
| Schema `Required` includes `decisions`, matching the validator | `rcSubmitReviewToolInfo` |
| One **format**-repair nudge (unparseable payload or prose final answer), same deadline, tools restricted, revalidated; substantive rejections never retried; `errRCMalformedAnswer` | `rcChallengeReview` |
| `rcExtractJSONObject`: embedded JSON accepted; **two** top-level objects rejected as ambiguous | challenge.go |
| Validator messages split (unknown issue / duplicate / no reasoning, each naming the value); rejected payload logged in full (`rcIndentBounded`, 400 lines); per-experiment console summary; dropped-citation log; capped-readiness log; final verdicts logged | challenge.go |

Tests: `TestFastDraftDoesNotSubstituteChallenger`,
`TestReviewChallengeUnparseableSubmissionIsNudgedOnce`,
`TestReviewChallengeProseFinalAnswerIsNudgedOnce`,
`TestReviewChallengeUnverifiedThroughProvider`; multi-object rejection is
covered inside the citation test file.

Demonstrated: the routing fix is **confirmed from response metadata** in 9/9
captured challenge calls after 9a4a1ff; before it, 0/9 "GLM" challenges were
GLM. The `decisions` schema mismatch was the exact cause of one earlier
rejection and did not recur. The nudge fired once in six runs (deep attempt
1) and the resubmission was accepted; the two-object rejection has a unit
test and no live occurrence. Diagnostics turned three otherwise opaque live
rejections into named causes.

Known failures / limits: the nudge is a completion mechanism — it changes
how often a bundle appears, not whether its verdicts are right. `Served` is
unknown to the process by design.

### Group D — fidelity experiments and the complete experiment record

| element | where |
|---|---|
| `review_shell` two modes: `script` (exploration, no verdict) and `construct` (Node builds the exact command; `setup`; up to 8 `assertions`: `exit`, `stdout_contains`, `stdout_not_contains`, `pwd`, `pwd_not`) | `rcShellToolInfo` |
| `rcConstructDriver`: generated string written to a file and executed verbatim by a child `sh`; exit/pwd probes | sandbox.go |
| `rcExperimentRecord` (`Outcome completed|not_run`, error, mode, setup, construct, generated command, exit, observed pwd, stdout, stderr, per-assertion expected/observed) — every attempt recorded, not-run preserved | sandbox.go, challenge.go `notRun` |
| `runExperiment` always returns a record; `parseConstructOutput`; `evaluate`; `render` (what the model cites); `summary` (console) | sandbox.go |

Tests: `TestExperimentRecordDistinguishesNotRunFromFailedAssertion`,
`TestExperimentRecordParsesConstructOutputAndEvaluates`, sandbox tests,
container forensics (`TestForensics*`, 5), actual-source #429 tests
(`TestPR429ActualSourceIsVendoredVerbatim`,
`TestPR429ActualSourceWorkingDirectoryByInput` over plain / space / `"` /
`'` / `$HOME` / `$(id -u)` / backtick).

Demonstrated (deterministic, container): the real #429 statement is
misdirected for `$`, `$(…)`, backtick and correct for space/quotes; the
earlier false negative was a hand-escaped test + a misread printout
(forensics). Live: 9/9 deep-run experiments constructed the exact statement
and were executed verbatim; the record carried the stderr that later refuted
"silently".

Known failures / limits: the construction is **model-authored** and nothing
checks it matches the source under review; multi-command constructs
mis-execute (`---` ran as a command, exit 127); setup assumptions (root vs
`nobody`) produced one `not_run`; assertions can be vacuous (command text
asserted in stdout) and the harness cannot tell; no `stderr_*` assertion
kinds, so "silent" cannot be tested.

### Group E — evidence interpretation, verdict derivation, remedy handling

| element | where |
|---|---|
| Experiment-citation relevance fields: `addresses_allegation`, `covers_alleged_inputs`, `expectation` (`intended`/`defect`), `tested`, `assertions` (offered 1-based indexes) | `rcCheckEvidence`, schema, prompt |
| `observation(expectation, selected)`: violation/conformance derived from the offered assertions only | sandbox.go |
| `rcRelevance{Relevant, Violation, ConformanceCovering}`; the runtime switch: not relevant → unresolved; supported without violation → unresolved; refuted with violation → unresolved; refuted without covering conformance → unresolved; verdicts never flipped | `rcResolveCitations`, `rcValidateChallenge` |
| Remedy handling as it exists: published iff supported (Group B); content unchecked | — |

Tests: `TestReviewChallengeVerdictConnectsTestedAndObserved`,
`TestReviewChallengeRuntimeClaimPublishesWithExperiment`,
`TestUnrelatedFailedAssertionCannotSupportDirectoryAllegation`,
`TestWrongVerdictRunCannotProduceTheWrongConclusion` (preserved run, honest
vs dishonest fields), `TestMissingDirCaseKeepsTheQuotingVerdict`; **red by
design:** `TestStoppingAfterFailedCdIsNotPublishedAsADefect`.

Demonstrated: with honest fields, the preserved wrong-verdict run cannot
produce its wrong conclusion; deep GLM supported the real quoting defect 3/3
with model-supplied fields. Both are partial: in two of those three runs the
verdict rested on one sound citation while others were ill-formed or
label-inverted.

Known failures: (1) `expectation` label inversion turned real misdirection
into "conformance" (deep attempt 2, 2 of 3 citations) — the same shape as
the original false negative, still live for `refuted`; (2) an assertion that
can never pass counted as a violation (deep attempt 1); (3) the
**missing-directory false finding** with a harmful remedy published as
confirmed (deep attempt 3) — the contract checks that a behavior was
observed, not that it is contrary to the requirement, and "intended" is
whatever the model labels; (4) fast-path GLM omitted the fields once (1/1
exercised) → unresolved; (5) the adjudication passes showed requirement
reading varies across attempts of the same model (case 1: harmful remedy
endorsed 1/3; case 5: violation label contradicting the model's own
conformance reasoning 2/3). This group has the most mechanism and the least
demonstrated judgment.

### Not in the diff, but load-bearing

- The **drafter** prompt (`review_commit.go:60/64/98`) is unchanged and
  self-contradictory about out-of-reach dependencies; it is the source of
  the deep path's incompleteness.
- `rcInferIntent` (unchanged) paraphrased the author's "safely" claim into
  the INTENT the challenger received.
- The server (kai-server) ignores the `challenge` field.

---

## 2. Proposed split into independently reviewable changes (nothing split yet)

Ordered by dependency; each carries its own tests, fixtures and doc section.
Sizes are production lines, approximate.

| # | change | contents | depends on | ~size | tests / fixtures that move with it |
|---|---|---|---|---|---|
| **S1** | **Cite by location** | location fields; `rcSourceLines`/`rcNumberedSource`/`rcExtractCitation`; numbered prompt; remove exact-quote matching and its repair retry. **Out-of-range citation fails closed** (as a quote mismatch does today) — tolerance moves to S2, because tolerance needs per-allegation status | — | ~80 | `TestReviewCitationExtractsByLocation`; a citation-slip regression built from the originating incident's shape |
| **S2** | **Structured results, assembled publication, incomplete status** | Group B in full + per-citation tolerance + sandbox-unavailable-not-fatal + bundle `challenge` field + CLI/exit status; `requires_runtime` mandatory; remedy gated by status; readiness cap-only; decisions from the draft | S1 | ~350 | the Group B tests; server-render check (harness, not committed) |
| **S3** | **Routing, schema, diagnostics** | fast `challengeModel`; per-phase `Models`; `decisions` in `Required`; split validator messages; rejected-payload log; format nudge + `rcExtractJSONObject` | S2 (record lives on `rcChallengeResult`; `decisions` validation is S2's) | ~120 | `TestFastDraftDoesNotSubstituteChallenger`, nudge tests, multi-object test |
| **S4** | **Fidelity experiments and the complete record** | Group D: construct mode, driver, `rcExperimentRecord`, `runExperiment`, render/summary, `not_run` preservation, `Experiments` in the bundle | S1 (numbered rendering), S2 (bundle) — the sandbox code itself is standalone | ~400 | sandbox tests, forensics, PR429 actual-source tests, `wrong-verdict-run.json` (as a record, not a verdict test) |
| **S5** | **Evidence interpretation** — *experimental* | Group E: relevance fields, `observation()`, `rcRelevance`, the four rules; prompt paragraph | S2, S4 | ~150 + prompt | verdict-connection tests, wrong-verdict validation, **red** missing-directory regression, adjudication fixtures |

S3's nudge could also be argued into S2; it is placed in S3 because it is a
completion mechanism, not a publication rule, and should be evaluated as
one. Docs: `review-evidence.md` splits along the same lines;
`review-evidence-glm-runs.md` stays whole as the record.

What the split buys: S1 and S3(routing) are small, mechanical, and remove
demonstrated failures without changing what a "finding" is. S2 changes the
product's publication contract and needs the comparison below. S4 is tooling
whose value is only realized through S5. S5 is the part with known wrong
outputs and stays behind the red test.

---

## 3. Bounded comparison: existing reviewer vs proposed reviewer

### Arms and fixed configuration

| | arm A — existing | arm B — proposed |
|---|---|---|
| binary | `kai` built from `main@df52131` in a separate worktree; sha256 recorded | `kai` built from the frozen PR head (`035fdd8`); sha256 recorded |
| engine | `kai-engine v0.6.73` (same pin in both `go.mod`s) | same |
| review model | `KAI_REVIEW_MODEL=z-ai/glm-5.2` | same |
| fast-path challenger | Haiku (base's substitution — a routing behavior under evaluation, reported as such) | GLM-5.2 (S3) |
| sandbox | `node@sha256:c610fcdf…` via Docker, same image both arms | same |
| capture | logging reverse proxy on `127.0.0.1:8899` → atlas; scratch `HOME` with a copied `credentials.json` (`server_url` rewritten; the real file never edited); `KAI_RAW_DUMP` | same |
| deadlines | as shipped in each arm (no changes) | same |
| execution | sequential, same machine, arms interleaved per case (A then B, then next case) to spread provider drift | |

Arm A's base publishes model-rewritten prose and withholds entire reviews on
any `unverified`; those are its real behaviors and are measured, not
normalized away.

### Cases (to be fixed before any run; expected outcomes established independently of either reviewer)

Selection rule: real merged changes from kai repos, reviewable as a single
commit against its parent; expected outcome established **before** the run
from (a) a later fix PR / issue that names the defect, (b) the PR's own
stated intent for behavior changes, or (c) merged + deployed + no follow-up
fix + the user's confirmation for correct changes. The reviewer's output is
never a source of expected outcomes. Candidates (from the last 25 merged PRs
per repo; final list needs the fix-PR reading marked ✎):

| category | candidate | expected outcome | how established |
|---|---|---|---|
| defect (regression case) | kai-desktop #429 | one defect: `JSON.stringify` quoting misdirects for `$`/backtick; no "missing directory" defect; readiness ≤ 3 | preserved record + container tests |
| defect (regression case) | kai-desktop #418 | the `cd` allegation is false; expected real findings ✎ | preserved record; ✎ read the PR |
| defect | the change that introduced the blank Changes panel bug (fixed on `fix/changes-selected-workspace`) ✎ | the sheet throws on a missing `selected()` | fix PR + offline repro noted in memory |
| defect | the client change that made a fresh clone replace `snap.latest` (fixed: baseline Old + ancestry-checked yield) ✎ | ref overwrite on fresh clone | fix PR |
| defect | kai-engine anti-stall guard regex (fixed in kai-engine#101) ✎ | false-positive decision detection capitulates | fix PR #101 |
| defect | kai-desktop #387's introducing change (fixed by #388) ✎ | exit-status dump instead of usage-limit message | #388 |
| correct, trivial | kai-cli #114 (engine pin bump), kai-server #262 (image pin) | 0 defects; readiness 5; complete | mechanical |
| correct, non-trivial | kai-cli #98 (tell the reviewer which repository), kai-server #243 (concise GitHub summaries) | 0 defects; readiness ≥ 4 | merged, deployed, no follow-up ✎ user confirms |
| correct, non-trivial | kai-server #251 (prefer deliverable GitHub email; block noreply magic-link) | 0 defects ✎ | deployed; ✎ user confirms |
| behavior change | kai-desktop #425 (remove telemetry toggle) | DECISION (feature removal), 0 defects | PR intent |
| behavior change | kai-server #237 (remove three catalog models) | DECISION (user-visible removal), 0 defects | PR intent |
| behavior change | kai-server #249 (drop CI early-beta gate from runs page) | DECISION (access/limits change), 0 defects | PR intent |
| behavior change | kai-cli #110 (adopt-engine via PR, not direct push) | DECISION (process), 0 defects | PR intent |
| behavior change, money | a billing/limits change (e.g. the prepaid-packs or referral PR) ✎ | DECISION naming who is charged/limited | PR intent |

Target: **14 fast cases** (2 regression + 4 defects + 4 correct + 4 behavior
changes) and a **deep subset of 4** (#429, one other defect, one correct
non-trivial, one behavior change), each deep case captured once (`kai
capture` at the parent commit in a scratch clone; snapshot id recorded and
held for all attempts, store hash drift documented as before).

Expected outcomes are written per case as: files/mechanism of each expected
defect; expected decisions; expected "no defect"; acceptable readiness
range. Findings outside that set are **not** auto-counted false — they go to
an *unadjudicated* bucket the user adjudicates after the run.

### Run counts

3 attempts per case per arm per path: fast 14×3×2 = 84 runs; deep 4×3×2 =
24 runs. First-attempt outcomes reported separately from all-attempts.

### Cost and time limits (hard stops)

From the captured runs: deep ≈ 30–50k prompt + 4–7.5k completion tokens
(≈ $0.02–0.03 at $0.42/$1.32 per Mtok); fast ≈ 6–17k prompt + 1.5–2k
completion (Haiku via OpenRouter $1.00/$5.00 on arm A's draft). Projected
total ≈ 2.5M prompt + 0.3M completion ≈ **$2–3**. Caps: **5M tokens or $10**
(whichever first), **6 hours wall clock**, and an infrastructure stop: if
either arm emits bundles for < 50% of the trivial-correct cases in the first
pass, stop and report (that is a harness problem, not a quality signal).
Cost is read from response `usage` and priced from the server catalog.

### Metrics (per run; aggregated per case, arm, path)

| metric | definition |
|---|---|
| real defects found | for a defect case: (a) draft alleged the expected defect; (b) a published `supported` finding matches it (rubric: file + mechanism); reported as (a) and (b) separately, since arm A cannot publish partial results |
| false findings | published supported finding that contradicts the pre-established expectation (e.g. a defect on a correct change; the `cd` allegation on #418) — counted only against written expectations; everything else → unadjudicated |
| harmful remedies | a published remedy that would introduce a defect or reintroduce the hazard the change removes; adjudicated by reading and, where a construction exists, by executing it in the sandbox image; recorded with the adjudicator's reasoning |
| completion | bundle emitted? `incomplete` flag? exit code; timeouts; validator/gate rejections (arm A: "challenge failure", no body). An incomplete bundle is not a completed review |
| decisions | expected DECISION present / absent / published as a defect instead |
| latency | wall clock per run from the CLI's own `timing:` lines and the harness clock |
| cost | tokens and $ per run |
| routing | per call: requested (request body) vs effective (response `model`/`provider`); any call without response metadata = **unknown**; runs whose challenge was not served by the configured model are flagged (arm A fast will flag by design) |
| retries | format nudges fired, resubmissions accepted |

### Preservation and verification

Every request/response (auth stripped), stderr, bundle, timing, and both
binaries' sha256 preserved per run under `eval/<case>/<arm>/<path>/attempt-N/`;
deep snapshot ids recorded; the scratch `HOME` copy used, real
`~/.kai/credentials.json` untouched; no code change between any two runs.

### What the comparison cannot say

n=3 per cell; one model; one week of provider routing; expected outcomes
for "correct" changes rest on absence of a later fix. It will bound the
*direction* and *size* of differences in the metrics above; it will not
certify judgment.

---

## 4. Pre-evaluation status by component (not the shipping recommendation)

| component | evidence today | pre-evaluation status |
|---|---|---|
| S1 cite by location | failure class removed structurally; 0 dropped citations in 6 runs | ready pending the comparison's completion/false-finding numbers (it cannot make judgments worse; it can only stop withholding correct ones) |
| S3 routing + schema + diagnostics | routing confirmed from metadata 9/9; schema mismatch cause found and gone; diagnostics named three live rejections | routing fix is ready on its own evidence (it corrects a silent misconfiguration); the nudge and JSON extraction need the completion numbers |
| S2 structured publication | 6/6 bundles rendered correctly; statuses carried to CLI/bundle/exit; remedy content unchecked; incomplete-by-construction for host-dependent allegations; server ignores the record | needs the comparison (complete vs incomplete, false findings) and a decision on the drafter's limitations slot before it can be called ready |
| S4 fidelity experiments | deterministic reproduction; verbatim execution proven; construction fidelity unverified | tooling; ships only with S5 |
| S5 evidence interpretation | 3/3 real-defect supports (2 resting on single sound citations); 1 false finding with a harmful remedy; label inversion live; red regression | **experimental** — behind the red test regardless of the comparison |

The shipping recommendation follows the comparison.
