# PR #117 — diff audit, minimal shipping candidate, and staged evaluation plan (revision 2)

Status: audit and plan only. No code split, no gate change, no runs, no merge,
no release. Base is `main` at `df52131` (kai-cli #115 + #116: the exact-quote
challenge gate with one citation-correction retry). PR head is `035fdd8`.

Revision 2 changes, from review of revision 1: the routing fix is defined as
an independently extractable change with no dependency on structured
verdicts or evidence interpretation, and its model-record storage is
separated as optional; a **minimal shipping candidate** is defined and is
the arm compared against the baseline first, the full experimental gate
being a conditional third arm; expected outcomes are established by
inspection with recorded evidence, not by "merged and quiet"; the first
stage is six fixed cases run on both production paths (fast and deep);
two audit claims are corrected (below, marked **corrected**).

Standing rule: a resolved citation proves location, an executed experiment
proves an observation, a green test proves the code does what the test
says, a well-formed bundle proves formatting. None is a correct review
judgment.

---

## 1. Diff audit against the base

### What the base does

- Challenge answer = `{review: <model-rewritten full review>, checks[{source, quote}]}`.
  A quote must be an exact substring of its source or the **entire review is
  withheld**; one citation-correction resubmission is allowed (#116).
- Any `unverified` check → `"challenge could not verify an allegation"` →
  entire review withheld. No per-allegation status; no partial publication.
- The published text is the model's rewrite; the validator checks its coda
  matches the supported set and readiness is coherent, else withholds.
- `review_shell` is script-only. Fast path: the substituted draft model
  (Haiku) is also the challenger. Deep path: a challenge failure → incomplete
  bundle with no body.

### Group A — citation handling and provenance

Elements: location citations (`rcCheckEvidence{Source, LineStart, LineEnd}`),
numbered rendering and extraction from one coordinate system
(`rcSourceLines`, `rcNumberedSource`, `rcExtractCitation`), removal of the
exact-quote path and its retry, per-citation tolerance (drop + log; needs
Group B's per-allegation status), `rcCitationRef` in the bundle, per-phase
`Models` record, `Experiments` record.

Tests: `TestReviewCitationExtractsByLocation`,
`TestReviewChallengeToleratesOneBadCitation`,
`TestReviewChallengeOneInvalidCitationLeavesOnlyThatFindingUnresolved`
(the last two need Group B), fixture-identity tests.

Demonstrated: the originating failure — a byte-for-byte quote slip
withholding the whole review — cannot occur, because the system copies the
lines. In the six captured 9a4a1ff runs: 32 citations, 0 out-of-range, 0
validator rejections (protocol event counts, not judgment measures).

**Corrected claim.** Revision 1 said cite-by-location "cannot make judgments
worse; it can only stop withholding correct ones." That is wrong. The base's
exact-quote check withheld *every* review whose model fabricated or
mis-copied a quote — including reviews carrying **incorrect findings** whose
invented evidence failed to match. Under location citations the model names
a real range, the system copies it, and nothing checks that the copied
lines support the claim. So cite-by-location **can let through an incorrect
finding that the base would have withheld** (the deep run's citations of
the diff's `-` line as the "requirement" for a false finding all resolved).
It trades a fail-closed check that was wrong in both directions for a
provenance check that is right about location only. Whether that trade
improves review quality is exactly what the comparison must measure (false
findings, arm B vs arm A), not something the audit can assert.

Known limits: `Served` is never known to the process; a resolved citation
says nothing about relevance.

### Group B — structured results and system-assembled publication

Elements: `rcChallengeAnswer` without `review`/summary; `rcAllegationResult`
/ `rcDecisionResult`; `requires_runtime` mandatory; `unverified` →
`unresolved`; remedy published iff supported (else `WithheldRemedy`);
decisions from the draft only; readiness cap-only; derived SUMMARY;
`rcAssembleReview`; `Incomplete` → bundle flag, CLI `Status: INCOMPLETE`,
exit 1; sandbox-unavailable not fatal; bundle `challenge` field.

Tests: ten (`…SummaryIsDerivedNotModelAuthored`, `…RecordsFinalVerdictAndPerClaimReason`,
`…ClampsIncoherentReadinessInsteadOfWithholding`, `…AssessesDraftDecisions`,
`…UnresolvedCDCannotPublishBraceAdviceUnderDecisions`,
`…AbsentExperimentIsDowngradedAndRemedyWithheld`, `…UnavailableSandboxIsNotFatal`,
`…SkipsDraftWithoutIssuesOrDecisions`, `TestPartialResultsReachEmittedBundleAsIncomplete`,
`TestFastReviewReportsUnresolved`).

Demonstrated: 6/6 captured bundles emitted and rendered through the real
server path with correct counts and banners; downgraded prose cannot
reappear. Model-authored text still published, gated by status and
unchecked for content: headline, finding description, remedy.

Known failures: incomplete-by-construction for host-dependent allegations
(deep 0/3 complete; the unchanged **drafter** prompt orders out-of-reach
dependencies filed as ISSUES); the server drops `challenge`; a supported
allegation's remedy is unchecked (the harmful `cd … ;` remedy was
published); readiness is the model's, only capped.

### Group C — model routing, schema consistency, diagnostics

**Routing (extractable on its own — see §2 R).** The fast path handed its
substituted draft model to the challenge; every "GLM" fast challenge before
9a4a1ff was Haiku. The fix is a parameter: `rcRunFastReview` takes the
challenge model separately and the caller passes the configured review
model. It needs no structured verdicts, no evidence interpretation, and no
model-record type — a stderr line says which model was requested for which
phase. **Model-record storage** (`Models{Draft, Challenge}` on
`rcChallengeResult`, `Configured` stamping, provider capture) is optional
provenance that happens to live on Group B's result type; it is separated
from the routing change.

Schema/diagnostics: `decisions` in `Required` (Group B only); one
format-repair nudge; `rcExtractJSONObject` with two-object rejection; split
validator messages; rejected-payload log; experiment console summaries.

Tests: `TestFastDraftDoesNotSubstituteChallenger` (its requested-model
assertions are the routing test; its `Models` and schema assertions belong
to the optional record and to Group B), nudge tests, multi-object test.

Demonstrated: routing confirmed from response metadata 9/9 after, 0/9
before. The nudge fired 1/6 and was accepted. Diagnostics named three
otherwise opaque live rejections.

### Group D — fidelity experiments and the complete record

Construct mode, verbatim driver, `rcExperimentRecord` with `completed` /
`not_run`, render/summary. Tests: sandbox, 5 forensics, 7 actual-source
inputs. Demonstrated: deterministic reproduction of #429; 9/9 live
experiments executed the exact construction. Limits: construction is
model-authored and unverified against the source; vacuous assertions
undetectable; no `stderr_*` kinds.

### Group E — evidence interpretation, verdict derivation, remedy handling

Relevance fields, `observation()`, `rcRelevance`, the four rules. Tests: 5
green, **1 red by design** (`TestStoppingAfterFailedCdIsNotPublishedAsADefect`).
Demonstrated: 3/3 real-defect supports (2 resting on a single sound
citation). Known failures: label inversion live; vacuous assertion counted;
a false finding with a harmful remedy published; requirement reading varies
across attempts of the same model. Experimental.

### Not in the diff, load-bearing

Drafter prompt contradiction (`review_commit.go:60/64/98`); `rcInferIntent`
laundering the author's "safely" claim; kai-server ignoring `challenge`.

---

## 2. Proposed split (nothing split yet)

| # | change | contents | depends on | notes |
|---|---|---|---|---|
| **R** | **Fast-path challenger uses the configured review model** | `rcRunFastReview(…, model, challengeModel, …)`; caller passes `fastModel, model`; one stderr line; regression = the requested-model assertions of `TestFastDraftDoesNotSubstituteChallenger` against a recording provider fake | **none** — applies to the base as is | corrects a silent misconfiguration; changes which model judges on the fast path, so it *is* a behavior change and is measured in the comparison |
| **R-opt** | model-record storage | `Models{Draft, Challenge}` per phase, `Configured` stamping, provider capture | S2 (the record lives on `rcChallengeResult`) | provenance only; ships with S2 or not at all |
| **L** | **Cite by location, fail closed** | location fields; `rcSourceLines`/`rcNumberedSource`/`rcExtractCitation`; numbered prompt; out-of-range → `rcCitationError` ("line range out of bounds"), keeping the base's single citation-correction resubmission with adapted wording; exact-quote matching removed. Base's model-rewritten review and its unverified-withholds-all behavior **unchanged** | none | removes the originating failure; **may admit incorrect findings the quote check would have blocked** (audit correction) — the comparison's false-finding metric is the test |
| S2 | structured results, assembled publication, incomplete status, tolerance, sandbox-unavailable-not-fatal, bundle `challenge`, CLI/exit status | Group B | L | changes the publication contract |
| S3 | schema `decisions`, format nudge, JSON extraction, split messages, payload logging | | S2 | completion/diagnosability |
| S4 | fidelity experiments + record | Group D | L, S2 | tooling; value only via S5 |
| S5 | evidence interpretation | Group E | S2, S4 | experimental; behind the red test |

### The minimal shipping candidate — **C-min = base + R + L**

Built as a new branch from `df52131` with those two changes only (the PR
branch is untouched). Everything else in the base stays: the model still
writes the revised review, `unverified` still withholds the whole review,
script-only `review_shell`, base deadlines. Expected size ≈ 100 production
lines plus two tests. What C-min claims: (1) the fast challenger is the
configured model; (2) a citation slip no longer withholds a review. What it
does not claim: any change to what counts as a finding.

---

## 3. Evaluation — stage 1

### Arms

| arm | binary | what it answers |
|---|---|---|
| **A** baseline | `kai` from `main@df52131` | what production would do today with this model configuration |
| **B** C-min | `kai` from base + R + L | are the two independently justified fixes shippable — do they change completion and false findings, and in which direction |
| **C** full gate (conditional) | `kai` from `035fdd8` | run **only** on stage-1 cases that trigger Q1 or Q2 below; never as a blanket third arm |

Q1 — *partial publication*: a real-defect case where both A and B withheld
the whole review because some allegation was `unverified`. Does S2 publish
the supported finding with the rest unresolved, and is that published
finding correct? Q2 — *false finding through a location citation*: a case
where B published a finding adjudicated false whose citations all resolved.
Does S5's experiment/relevance contract block it, and at what cost to the
true findings in the same review?

Fixed configuration for every arm: `kai-engine v0.6.73`,
`KAI_REVIEW_MODEL=z-ai/glm-5.2`, sandbox image `node@sha256:c610fcdf…`,
logging reverse proxy on `127.0.0.1:8899` → atlas, scratch `HOME` with a
copied `credentials.json` (real file never edited), `KAI_RAW_DUMP`, each
arm's own shipped deadlines, sequential runs on one machine, arms
interleaved per case. Arm A's fast challenger is Haiku by its own
substitution — a measured behavior, not normalized away. Both paths run
because production runs both: the CI workflow (`review_default_workflow.go`
lines 335 and 390–408) runs `--fast` first, then `kai init` + `--deep`.

### The six cases (fixed; inspected; evidence recorded)

Each case is reviewed as one commit against its parent in a fresh scratch
clone of the local repository (never the working clones — kai-desktop is
dirty and kai-server is on a feature branch), with `kai capture` at the
parent for the deep path. Expected outcomes come from inspection and
independent records, written here before any run.

**D1 — kai-desktop #429** (real defect; JS). Open PR, head `8a0662e5`
(present locally), vs its parent. Files: `frontend/dist/app.js`,
`frontend/dist/panels.js`.
*Expected defect (required):* `'cd ' + JSON.stringify(wsPath) + ' && ' + command`
is written verbatim to a POSIX sh; `$`, `$(…)` and backticks inside double
quotes are expanded, so a workspace path containing them is misdirected.
*Evidence:* the vendored actual statement executed in the pinned image
(`review_commit_pr429_test.go`): misdirected for `$HOME`, `$(id -u)`,
backtick; correct for space, `"`, `'`. The author's body claims spaces,
backslashes and quotes are handled — true — and does not mention `$`.
*Not defects:* stopping when the directory is missing (the requirement's
own behavior; stderr carries the shell's error — `proposal-eval.out`);
the persistent-shell cwd change (a design consequence, DECISION at most);
the terminal panel's verbatim-write contract (a limitation, outside the
repo). *Expected decisions:* none required; cwd persistence acceptable as
one. *Readiness:* ≤ 3 when the defect is found. *Known false finding to
watch:* "silently aborts on a missing directory" published as a defect.

**D2 — kai-cli #115** (real defect; Go). Merge `f19c9c3f` vs parent. Files:
`review_commit.go`, `review_commit_challenge.go`, `review_commit_fast.go`,
`review_commit_sandbox.go`, tests, docs.
*Expected defect (required):* the citation validator treats any exact-text
mismatch as fatal and withholds the entire review
(`!strings.Contains(sources[i], quote)` → error → caller withholds), so a
single mis-copied quote discards every finding including correct ones.
*Evidence:* #116's body — "failed twice because the challenge validator
reported 'missing or invented evidence' and immediately withheld the entire
draft … gave the model no opportunity to correct a citation" — and the
originating incident this PR exists for. *Secondary (credited if found, not
required):* `unverified` withholds the whole review; the challenger
rewrites the published review text itself. Both are design consequences
later reversed in #117; weaker independence, recorded as such.
*Not defects:* the Docker sandbox hardening choices. *Readiness:* ≤ 3 when
the primary defect is found.

**C1 — kai-cli #100** (correct change; Go). Merge `4aab378f` vs parent.
`rcFilesRead` gains a root and drops only stat-confirmed directories.
*Expected:* no defect. *Evidence from inspection:* `rcIsDir` joins relative
paths to `root` and returns false on any stat error or empty root
(unresolvable paths kept — the asymmetric-cost argument in the code);
`primary.Path` is the workspace root (`Workspace: primary.Path` at
`review_commit.go:711` of that commit); `TestFilesReadDropsDirectories`
covers two real directories dropped, a real file kept, an unresolvable
absolute path kept; the existing dedupe test passes an empty root.
*Plausible non-defect observations a reviewer may raise:* a symlink to a
directory is dropped (stat follows it — consistent with "directory"); one
stat per path (negligible). *Expected decisions:* none. *Readiness:* ≥ 4.

**C2 — kai-cli #98** (correct change; Go). Merge `1e4c17fe` vs parent.
`rcRepoIdentity` (env → env → origin remote) and `rcRepoHeader`.
*Expected:* no defect. *Evidence from inspection:* precedence mirrors
`resolveGitHubClient`; `RepoSlugFromRemote`'s regex
`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$` accepts ssh and https forms;
non-GitHub or missing remote → `""` → header omitted (silence over a wrong
name, the stated design); four unit tests cover env precedence, fallback,
remote, and silence. *Plausible non-defect observation:* a `GITHUB_REPOSITORY`
set in a local shell overrides the checkout's remote — a documented
trade-off; if raised, adjudicate as decision/limitation, not defect.
*Expected decisions:* none. *Readiness:* ≥ 4.

**B1 — kai-desktop #425** (intentional behavior change; HTML). Merge
`ed6a53ca` vs parent `6226bd97`. Removes the "Share anonymous usage data"
toggle row.
*Expected decision (required):* a user-visible privacy control is removed
while telemetry stays on by default — data continues to be sent on the
user's behalf with no in-app opt-out. *Expected defects:* none.
*Evidence from inspection at the parent:* no file under `frontend/dist/*.js`
references `TelemetryEnabled`/`SetTelemetryEnabled`; the settings modal's
`.st-toggle` click handler is explicitly cosmetic ("no state, just the
class + aria-checked") except for "Notify when runs finish"; the Go
bindings exist (`analytics.go:355, 360`) and were never called from the UI.
So the toggle was non-functional and its removal changes no data flow — the
PR body's claim checks out. *Readiness:* 4–5 (a decision never scores below
4 under the drafter's rubric).

**B2 — kai-server #237** (intentional behavior change; Go/YAML). Merge
`5c44025c` vs parent. Removes three model IDs from the picker catalog and
two price rows.
*Expected decision (required):* three models disappear from every user's
picker (the desktop fetches the catalog at runtime). *Expected defects:*
none. *Evidence from inspection at the merge:* zero remaining references to
the three IDs anywhere in the repo; the only `nvidia/` literal left is a
different ASR model in `llm_transcription.go`; kai-desktop HEAD has no
hardcoded reference to them. *Plausible unexpected finding:* a user whose
saved selection is a removed model — pre-existing (those models were
already failing upstream with 400/429 per the PR body); if raised,
adjudicate as pre-existing condition or decision, not an introduced defect.
*Readiness:* ≥ 4.

Kept as regression context but not run in stage 1: kai-desktop #418.

### Runs

3 attempts × 6 cases × 2 arms × 2 paths = **72 runs** (36 fast, 36 deep).
First-attempt outcomes reported separately from all-attempts. Arm C adds at
most 3 × (triggering cases) × (triggering path).

### Cost and time

From captured runs: deep ≈ 30–50k prompt + 4–8k completion tokens
(≈ $0.02–0.03 at GLM-5.2's $0.42/$1.32 per Mtok); fast ≈ 6–17k + 2k
(arm A's Haiku via OpenRouter at $1.00/$5.00). Stage 1 projected ≈ **$1.5–2**
and ≈ 4 hours wall (deep dominates). Hard caps: **$5 or 2.5M tokens, 5 hours**.

### Metrics (per run; aggregated per case × arm × path)

| metric | definition |
|---|---|
| real defects found | for D1/D2: draft alleged the expected defect (a); a published supported finding matches it by file + mechanism (b); (a) and (b) reported separately |
| false findings | a published finding contradicting a written expectation (a defect on C1/C2/B1/B2; the "silently aborts" defect on D1; a defect that inspection shows absent) |
| unexpected findings | anything published that the expectations neither require nor contradict — **adjudicated, never auto-counted**; adjudication records the source lines and any executed check, classifies as true-unexpected / false / decision / limitation, and the user reviews it before counts are final; a true-unexpected finding is credited and added to the expectations for later stages |
| harmful remedies | a published remedy that would introduce a defect or reintroduce the hazard the change removes; adjudicated by reading and, where a construction exists, by executing it in the sandbox image |
| decisions | expected decision present / absent / published as a defect instead |
| completion | bundle emitted; `incomplete` flag (arm B/C) or challenge-failure-no-body (A); exit code; timeouts; withheld drafts. An incomplete bundle is not a completed review |
| latency | wall clock per run |
| cost | tokens from response `usage`; $ from the catalog |
| routing | per call: requested (request body) vs effective (response `model`/`provider`); missing metadata = **unknown**; any challenge not served by the configured model flagged (arm A fast: Haiku, by design) |
| retries | citation corrections (A/B), format nudges (C) |

### Stopping criteria and gates

Hard stops (stop, fix, rerun the affected runs from scratch, keep both
sets): cost/time cap reached; **harness** failures — identified by error
class, not by outcome — on three consecutive runs: proxy/TLS/auth errors,
Docker unavailable, capture failures, disk. **Corrected from revision 1:**
low completion is **not** a harness signal. Withheld drafts, timeouts
inside the review, validator rejections, and prose-instead-of-submission
are reviewer failures; they are recorded as completion outcomes and never
stop or exclude a run.

No code change to any arm during the stage. If an arm is rebuilt for any
reason, the stage restarts for that arm.

Decision gates after stage 1 (before any stage 2 or any arm C run):

- **G1 — C-min not independently shippable as a pair:** if arm B publishes
  more false findings than arm A on C1/C2/B1/B2 (either path), L admitted
  findings the quote check would have blocked; report, and evaluate R alone
  (R is separable from L).
- **G2 — routing effect:** compare A vs B on the fast path only; B differs
  from A there by both R and L, so attribute to R only what the deep path
  (where R is inert) does not also show.
- **G3 — proceed to stage 2** (a larger set built the same way) only if B ≥
  A on real defects found and B ≤ A on false findings, on both paths.
- **G4 — arm C:** run only on cases meeting Q1 or Q2, only on the
  triggering path, 3 attempts each; report against the same metrics plus
  the red regression's acceptance condition where D1 is involved.

### Preservation

Per run under `eval/stage1/<case>/<arm>/<path>/attempt-N/`: every
request/response (auth stripped), stderr, bundle, timings; binaries' sha256
per arm; deep snapshot ids; the scratch clone's commit; the expected-outcome
text above frozen in `eval/stage1/expected.md` before the first run.

### What stage 1 cannot say

n = 3 per cell, one model, one week's provider routing, six changes from one
organization's repos. It bounds direction and rough size of the
differences; it does not certify judgment, and a clean result on C1/C2
does not establish that those changes are defect-free — only that
inspection found none and the reviewers agreed or disagreed.

---

## 4. Pre-evaluation status (not the recommendation)

| component | status |
|---|---|
| R routing | independently justified by the confirmed misattribution; its *effect on review quality* is unmeasured and is what G2 measures |
| L cite by location | removes the originating failure; may admit incorrect findings (audit correction); G1 measures |
| R-opt, S2, S3, S4 | not in the candidate; S2 is what arm C tests under Q1 |
| S5 | experimental; behind the red test regardless |

The shipping recommendation follows stage 1.
