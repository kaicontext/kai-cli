# Validation Methodology

How we prove Kai's test selection is safe and how we measure performance claims.

## Correctness Guarantee

Kai must never introduce false negatives.

**Definition of false negative:**

> The full test suite fails, but the Kai-selected suite passes.

If Kai skips a test that would have caught a bug, that is a correctness failure.

**Target:** 0 false negatives across all validation runs.

**Fallback policy:** When Kai's confidence in its selection is low (missing dependency mappings, dynamic imports, reflection-heavy code), it triggers a full suite run instead of risking a miss. A fallback is not a failure — it means the safety model is working.

## Experimental Setup

### Repositories Tested

Each validation run must record:

| Field | Description |
|-------|-------------|
| Repository | Name or URL |
| Commit | Full SHA of HEAD |
| Base | Full SHA or branch of base |
| Files | Total file count in snapshot |
| Tests | Total test count in full suite |
| Framework | Test runner (Jest, pytest, go test, etc.) |
| CI Baseline | Full suite wall-clock time |

### Environment

Each validation run must record:

| Field | Description |
|-------|-------------|
| OS | Operating system and version |
| CPU | Processor model and core count |
| Memory | Available RAM |
| Runtime | Node/Python/Go version |
| Test Runner | Framework version (e.g., Jest 29.7, pytest 8.1) |
| Kai Version | Output of `kai --version` |

This makes results reproducible. Without environment details, timing comparisons are meaningless.

## Procedure

For each PR or simulated change:

1. **Generate plan.** Run `kai ci plan --git-range BASE..HEAD` to produce the impacted test list.
2. **Run Kai-selected tests.** Execute only the tests Kai chose.
3. **Run full test suite.** Execute the entire suite as a control.
4. **Compare results:**
   - Exit codes (zero vs non-zero)
   - Failed test identifiers (exact set comparison)
5. **Record:**
   - Duration of full run vs Kai run
   - Test count reduction percentage
   - Whether fallback was triggered (and why)
   - False negatives (must be 0)
6. **Classify verdict:**
   - **safe** — both pass, or both fail on the same tests
   - **missed** — full fails on tests Kai did not select (false negative)
   - **fallback** — Kai triggered full run due to low confidence
   - **flaky_suspect** — full suite failure is inconsistent across retries

The `kai shadow run` command automates steps 1–6.

## Metrics

Every metric has an exact definition. No ambiguity.

### Test Reduction %

```
(1 - selected_tests / total_tests) × 100
```

How many tests Kai skipped compared to the full suite.

### CI Time Reduction %

```
(1 - kai_duration / full_duration) × 100
```

Wall-clock time saved by running the selective suite.

### False Negative Rate

```
false_negatives / total_validation_runs
```

Must be 0. Any non-zero value is a correctness failure that requires investigation.

### Fallback Rate

```
fallback_runs / total_validation_runs
```

How often Kai chose to run the full suite because confidence was low. A high fallback rate means the dependency graph needs improvement, but it does not mean Kai is unsafe — the opposite.

### Accuracy

```
1.0 - (false_negatives / non_flaky_failures)
```

Of the real failures in the full suite, what fraction did Kai's selection catch? Must be 1.0.

## Flaky Test Handling

Flaky tests — tests that pass or fail non-deterministically — must not be counted as false negatives.

**Policy:**

- If the full suite fails but a rerun of the same suite passes, the inconsistent tests are classified as **flaky**.
- Flaky tests are logged separately in the shadow report under `flaky.flakyTests`.
- Flaky tests are excluded from false negative calculations.
- Use `--retries N` to re-run the full suite N times for flaky classification.

**Why this matters:** Without flaky handling, every intermittent CI failure would appear as a Kai miss. Security and engineering teams evaluating Kai need to know that flaky noise is filtered out before drawing conclusions about correctness.

## Edge Cases and Known Limitations

Kai's static dependency analysis has known blind spots. We document them rather than hide them.

### Dynamic test generation

Tests created at runtime (e.g., parameterized tests from config files, data-driven test factories) may not appear in static analysis. Kai detects common patterns (`import()`, `require(variable)`, `__import__()`) and expands the selection or falls back to full.

### Reflection-heavy code

Languages with runtime reflection (Java's `Class.forName`, Go's `reflect` package) can create dependencies invisible to import graph analysis. Kai raises uncertainty score and may recommend full run.

### Runtime dependency injection

DI frameworks (Spring, Guice, etc.) wire dependencies at runtime. If a changed class is injected into a test's dependency tree without a static import, Kai may miss it. Coverage-based selection (`kai ci ingest-coverage`) mitigates this by using runtime data.

### External service mocks

Changes to external service contracts may affect integration tests that mock those services. Kai's contract registry (`kai ci ingest-contracts`) tracks schema-to-test bindings explicitly.

### Monorepo cross-module changes

A change in a shared library can affect tests across multiple modules. Kai's module system (`kai.modules.yaml`) and cross-module risk detection handle this, but misconfigured module boundaries can cause misses.

**In all cases:** When Kai cannot confidently determine the blast radius, it falls back to the full suite. The safety model errs on the side of running more, not less.

## How to Reproduce

### Prerequisites

```bash
# Install Kai CLI
curl -fsSL https://kaicontext.com/install.sh | sh

# Verify
kai --version
```

### Run a shadow validation

```bash
# Clone the target repo
git clone <repo-url> && cd <repo>

# Run shadow comparison on a range of commits
kai shadow run \
  --git-range main..feature-branch \
  --full "npm test" \
  --kai "npm test -- {{tests}}" \
  --retries 2 \
  --out shadow-report.json \
  --summary shadow-summary.md

# Check verdict
cat shadow-summary.md

# Machine-readable results
cat shadow-report.json | jq '.verdict, .metrics'
```

### Run across multiple PRs

```bash
# For each merged PR in the last N commits:
for sha in $(git log --first-parent --format=%H -20 main); do
  parent=$(git rev-parse ${sha}^)
  kai shadow run \
    --git-range ${parent}..${sha} \
    --full "npm test" \
    --kai "npm test -- {{tests}}" \
    --out "results/${sha}.json" \
    --summary "results/${sha}.md"
done
```

### Go projects

```bash
kai shadow run \
  --git-range $BASE..$HEAD \
  --full "go test ./..." \
  --kai "go test {{tests}}" \
  --format go \
  --out shadow-report.json \
  --summary shadow-summary.md
```

### Python projects

```bash
kai shadow run \
  --git-range $BASE..$HEAD \
  --full "pytest --json-report" \
  --kai "pytest {{tests}} --json-report" \
  --format pytest \
  --out shadow-report.json \
  --summary shadow-summary.md
```

## Raw Data

Every shadow run produces two artifacts:

### shadow-report.json

Machine-readable. Contains:

- Plan (selected tests, confidence, risk, fallback status)
- Selective run result (exit code, duration, test count, failures)
- Full run result (same fields)
- Metrics (reduction %, time saved %, false negatives, accuracy)
- Flaky test list
- Fallback info (triggered, reason, confidence)

### shadow-summary.md

Human-readable. Contains:

- Verdict with status indicator
- Metrics table
- Execution details for both runs
- False negative list (if any)
- Flaky test list (if any)

For aggregation across multiple runs, collect the JSON reports and compute rolling statistics:

```bash
# Example: extract verdicts from all reports
for f in results/*.json; do
  echo "$(basename $f .json): $(jq -r '.verdict' $f)"
done

# Count by verdict
jq -r '.verdict' results/*.json | sort | uniq -c
```

## Validation Flow

```
Changed Files (git diff)
        ↓
  Kai Dependency Graph
  (imports, symbols, coverage, contracts)
        ↓
  Impacted Test Selection
  (with confidence score)
        ↓
  ┌─────────────────────────────────┐
  │  Confidence check               │
  │  Low confidence? → Full suite   │
  │  High confidence? → Selective   │
  └─────────────────────────────────┘
        ↓                    ↓
  Kai Suite              Full Suite
  (selective)            (control)
        ↓                    ↓
  ┌─────────────────────────────────┐
  │  Comparison                     │
  │  • Exit codes match?            │
  │  • Failed test sets match?      │
  │  • Any flaky tests?             │
  └─────────────────────────────────┘
        ↓
  Verdict: safe | missed | fallback | flaky_suspect
```

## Current Confidence Level

State honestly where validation stands.

**Template** (update with real numbers as validation progresses):

- Repositories validated: _N_
- Total shadow runs: _N_
- False negatives observed: 0
- Fallback rate: _N%_
- Average test reduction: _N%_
- Average time reduction: _N%_

These numbers must come from actual shadow run data. Do not estimate or project. If you have run 3 repos, say 3. If you have run 500 PRs, say 500.

Claims without data are not claims — they are hopes.
