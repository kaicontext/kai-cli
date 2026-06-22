# Kai CI Quick Reference

## Commands

```bash
# Generate test plan from git changes
kai ci plan --git-range BASE..HEAD --out plan.json

# Human-readable explanation
kai ci plan --git-range BASE..HEAD --explain

# Ingest coverage for smarter selection
kai ci ingest-coverage --from coverage.json --format nyc

# Print plan for CI logs
kai ci print plan.json

# Validate plan schema
kai ci validate-plan plan.json
```

## Common Patterns

### GitHub Actions
```bash
kai ci plan --git-range ${{ github.event.pull_request.base.sha }}..${{ github.sha }}
```

### GitLab CI
```bash
kai ci plan --git-range $CI_MERGE_REQUEST_DIFF_BASE_SHA..$CI_COMMIT_SHA
```

### Local Testing
```bash
kai ci plan --git-range main..HEAD --explain
```

## Plan Modes

| Mode | Meaning |
|------|---------|
| `skip` | No tests needed (no code changes affect tests) |
| `selective` | Run specific tests only |
| `expanded` | Uncertainty detected, running more tests |

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--strategy` | `auto` | Selection strategy: auto, symbols, imports, coverage |
| `--safety-mode` | `guarded` | shadow (learn), guarded (safe), strict (trust) |
| `--risk-policy` | `expand` | On uncertainty: expand, warn, fail |
| `--out` | stdout | Write plan JSON to file |
| `--explain` | false | Show human-readable reasoning |

## Coverage Formats

```bash
# JavaScript (NYC/Istanbul)
kai ci ingest-coverage --from coverage/coverage-final.json --format nyc

# Python (coverage.py)
kai ci ingest-coverage --from .coverage.json --format coveragepy

# Java (JaCoCo)
kai ci ingest-coverage --from build/reports/jacoco.xml --format jacoco

# Go
kai ci ingest-coverage --from coverage.out --format go
```

## Using the Plan

```bash
# Get test files to run
jq -r '.targets.run[]' plan.json

# Check if should skip
jq -r '.mode' plan.json  # "skip" means no tests needed

# Get confidence score
jq -r '.safety.confidence' plan.json  # 0.0-1.0

# Check for risks
jq '.safety.structural_risks' plan.json
```
