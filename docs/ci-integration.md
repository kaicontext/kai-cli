# CI Integration Guide

Kai CI computes which tests need to run based on code changes. Instead of running your full test suite on every commit, run only what's affected.

## Quick Start

```bash
# Install kai
curl -fsSL https://kaicontext.com/install.sh | sh

# Generate a test plan from git changes
kai ci plan --git-range $BASE_SHA..$HEAD_SHA --out plan.json

# Use the plan in your CI
```

## GitHub Actions

### Basic Setup

```yaml
name: CI

on:
  pull_request:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0  # Need full history for change detection

      - name: Install Kai
        run: curl -fsSL https://kaicontext.com/install.sh | sh

      - name: Generate test plan
        id: plan
        run: |
          kai ci plan \
            --git-range ${{ github.event.pull_request.base.sha || github.event.before }}..${{ github.sha }} \
            --out plan.json

          # Export for later steps
          echo "mode=$(jq -r .mode plan.json)" >> $GITHUB_OUTPUT
          echo "targets=$(jq -c .targets.run plan.json)" >> $GITHUB_OUTPUT

      - name: Run tests
        if: steps.plan.outputs.mode != 'skip'
        run: |
          # Option 1: Run specific test files
          jq -r '.targets.run[]' plan.json | xargs go test

          # Option 2: Use plan.json directly with your test runner
          # your-test-runner --plan plan.json
```

### With Coverage Learning

For smarter test selection, ingest coverage data:

```yaml
      - name: Run tests with coverage
        run: go test -coverprofile=coverage.out ./...

      - name: Ingest coverage
        run: kai ci ingest-coverage --from coverage.out --format go

      - name: Upload coverage map
        uses: actions/upload-artifact@v4
        with:
          name: coverage-map
          path: .kai/coverage-map.json
```

### Monorepo Setup

```yaml
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      services: ${{ steps.plan.outputs.services }}
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Generate plan
        id: plan
        run: |
          kai ci plan \
            --git-range ${{ github.event.pull_request.base.sha }}..${{ github.sha }} \
            --out plan.json

          # Extract affected modules/services
          echo "services=$(jq -c '[.impact.modules_affected[]] | unique' plan.json)" >> $GITHUB_OUTPUT

  test:
    needs: plan
    if: needs.plan.outputs.services != '[]'
    strategy:
      matrix:
        service: ${{ fromJson(needs.plan.outputs.services) }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Test ${{ matrix.service }}
        run: go test ./${{ matrix.service }}/...
```

## GitLab CI

### Basic Setup

```yaml
stages:
  - plan
  - test

plan:
  stage: plan
  image: golang:1.22
  script:
    - curl -fsSL https://kaicontext.com/install.sh | sh
    - kai ci plan --git-range $CI_MERGE_REQUEST_DIFF_BASE_SHA..$CI_COMMIT_SHA --out plan.json
    - cat plan.json
  artifacts:
    paths:
      - plan.json
    reports:
      dotenv: plan.env

test:
  stage: test
  needs: [plan]
  script:
    - |
      MODE=$(jq -r .mode plan.json)
      if [ "$MODE" = "skip" ]; then
        echo "No tests needed"
        exit 0
      fi
      jq -r '.targets.run[]' plan.json | xargs go test
```

### With Rules

```yaml
test:
  stage: test
  needs: [plan]
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
  script:
    - |
      CONFIDENCE=$(jq -r .safety.confidence plan.json)
      if (( $(echo "$CONFIDENCE < 0.4" | bc -l) )); then
        echo "Low confidence ($CONFIDENCE), running full suite"
        go test ./...
      else
        jq -r '.targets.run[]' plan.json | xargs go test
      fi
```

## Plan JSON Schema

```json
{
  "mode": "selective|expanded|skip",
  "risk": "low|medium|high",
  "safety_mode": "shadow|guarded|strict",
  "targets": {
    "run": ["path/to/test1.go", "path/to/test2.go"],
    "skip": ["path/to/unaffected_test.go"]
  },
  "impact": {
    "files_changed": ["src/auth.go", "src/user.go"],
    "modules_affected": ["auth", "user"]
  },
  "safety": {
    "confidence": 0.85,
    "structural_risks": []
  },
  "policy": {
    "strategy": "symbols|imports|coverage|auto"
  }
}
```

## Strategies

| Strategy | Precision | Speed | Best For |
|----------|-----------|-------|----------|
| `symbols` | Highest | Slower | Large codebases with good symbol extraction |
| `imports` | High | Fast | Most projects |
| `coverage` | Highest | Fast | Projects with coverage data |
| `auto` | Adaptive | Varies | Default - tries each in order |

## Safety Modes

| Mode | Behavior |
|------|----------|
| `shadow` | Log what would be skipped, but run everything (learning mode) |
| `guarded` | Skip tests but expand to full suite on uncertainty |
| `strict` | Skip tests even when uncertain (for trusted codebases) |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `KAI_CI_STRATEGY` | Override strategy (symbols, imports, coverage, auto) |
| `KAI_CI_SAFETY_MODE` | Override safety mode |
| `KAI_CI_RISK_POLICY` | Risk policy (expand, warn, fail) |

## Troubleshooting

### "No test mapping found"

The plan shows low confidence because kai doesn't know which tests cover which files yet.

**Fix:** Ingest coverage data:
```bash
# After running tests with coverage
kai ci ingest-coverage --from coverage.json
```

### Tests are skipped but shouldn't be

Check the plan's reasoning:
```bash
kai ci plan --git-range main..feature --explain
```

Use shadow mode to validate without affecting CI:
```bash
kai ci plan --safety-mode=shadow
```

### Plan takes too long

For large repos, use import-based strategy:
```bash
kai ci plan --strategy=imports
```
