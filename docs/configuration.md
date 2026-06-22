# Configuration Reference

Kai uses a set of configuration files and environment variables to control behavior. No single config file is required — Kai ships with sensible defaults for everything.

## File Locations

| File | Location | Purpose | Commit to VCS? |
|------|----------|---------|----------------|
| `kai.modules.yaml` | Project root | Map file paths to logical modules | Yes |
| `.kai/rules/ci-policy.yaml` | `.kai/rules/` | CI thresholds, safety, and behavior | Yes |
| `.kai/rules/modules.yaml` | `.kai/rules/` | Auto-detected module metadata | Yes |
| `rules/changetypes.yaml` | Project root | Custom change type detection rules | Yes |
| `.kaiignore` | Project root | Files to exclude from analysis | Yes |
| `~/.kai/credentials.json` | Home directory | Auth tokens for remote servers | No |

## Module Configuration

### `kai.modules.yaml`

Defines logical modules for intent generation and CI impact analysis. Place this in your project root and commit it.

```yaml
modules:
  - name: Auth
    paths:
      - src/auth/**
      - lib/session.js
  - name: API
    paths:
      - src/routes/**
      - src/controllers/**
  - name: Billing
    paths:
      - billing/**
```

Auto-detect modules from your codebase:

```bash
# Preview detected modules
kai modules init

# Write to .kai/rules/modules.yaml
kai modules init --write
```

Kai checks `.kai/rules/modules.yaml` first, then falls back to `kai.modules.yaml` in the project root.

## CI Policy

### `.kai/rules/ci-policy.yaml`

Controls how Kai makes test selection decisions. If this file doesn't exist, Kai uses built-in defaults.

Legacy location `kai.ci-policy.yaml` in the project root is also supported.

```yaml
version: 1

thresholds:
  minConfidence: 0.40      # Expand to full suite if confidence below this (0.0-1.0)
  maxUncertainty: 70       # Expand if uncertainty score exceeds this (0-100)
  maxFilesChanged: 50      # Expand if more than N files changed
  maxTestsSkipped: 0.90    # Expand if >90% of tests would be skipped

paranoia:
  alwaysFullPatterns:       # Globs that trigger a full test run
    - "*.lock"
    - go.mod
    - go.sum
    - package.json
    - Dockerfile
    - ".github/workflows/*"
  expandOnPatterns:         # Globs that trigger expansion (but not full)
    - "**/config/**"
    - "**/setup.*"
    - "**/__mocks__/**"
  riskMultipliers:          # Boost risk score for certain paths
    "src/core/**": 1.5
    "lib/**": 1.3

behavior:
  onHighRisk: expand        # expand | warn | fail
  onLowConfidence: expand   # expand | warn | fail
  onNoTests: warn           # expand | warn | pass
  failOnExpansion: false     # Exit non-zero when expansion happens

dynamicImports:
  expansion: nearest_module  # nearest_module | package | owners | full_suite
  ownersFallback: true       # Widen to code owners' suites if module unknown
  maxFilesThreshold: 200     # If >N files in module, widen by owners instead
  boundedRiskThreshold: 100  # Bounded imports matching >N files are treated as risky
  allowlist: []              # Paths where dynamic imports are known-safe
  boundGlobs: {}             # Map pattern -> test globs for bounded expansion

coverage:
  enabled: true              # Use coverage data for test selection
  lookbackDays: 30           # How far back to look for coverage data
  minHits: 1                 # Minimum hit count to trust a file->test mapping
  onNoCoverage: warn         # expand | warn | ignore
  retentionDays: 90          # Prune coverage entries older than this

contracts:
  enabled: true              # Detect contract/schema changes
  onChange: add_tests         # add_tests | expand | warn
  types:                     # Which contract types to detect
    - openapi
    - protobuf
    - graphql
  retentionRevisions: 50     # Keep last N revisions per contract
  generated: []              # Schema -> generated file mappings
  # Example:
  # generated:
  #   - input: api/openapi.yaml
  #     outputs:
  #       - "gen/api/**"
```

All fields are optional. Kai merges your config with the defaults, so you only need to specify what you want to override.

## Change Type Rules

### `rules/changetypes.yaml`

Define custom rules for classifying code changes by AST node type.

```yaml
rules:
  - id: CONDITION_CHANGED
    match:
      node_types: ["binary_expression", "logical_expression", "relational_expression"]
      detector: "operator_or_boundary_changed"
  - id: CONSTANT_UPDATED
    match:
      node_types: ["number", "string"]
      detector: "literal_value_changed"
  - id: API_SURFACE_CHANGED
    match:
      node_types: ["function_declaration", "method_definition", "export_statement"]
      detector: "params_or_exports_changed"
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--verbose`, `-v` | Enable verbose debug output. Prints timestamped diagnostic messages to stderr. |

## Environment Variables

### Debug

| Variable | Description | Example |
|----------|-------------|---------|
| `KAI_VERBOSE` | Enable verbose debug output (`1` or `true`) | `KAI_VERBOSE=1 kai capture` |

### CI Control

| Variable | Description | Example |
|----------|-------------|---------|
| `KAI_FORCE_FULL` | Force full test suite (`1` or `true`) | `KAI_FORCE_FULL=1 kai ci plan ...` |
| `KAI_PANIC` | Panic switch — same as `KAI_FORCE_FULL` | `KAI_PANIC=1 kai ci plan ...` |
| `KAI_CI_STRATEGY` | Override selection strategy | `symbols`, `imports`, `coverage`, `auto` |
| `KAI_CI_SAFETY_MODE` | Override safety mode | `shadow`, `guarded`, `strict` |
| `KAI_CI_RISK_POLICY` | Override risk policy | `expand`, `warn`, `fail` |

### Telemetry

| Variable | Description | Example |
|----------|-------------|---------|
| `KAI_TELEMETRY` | `0` = hard off, `1` = on (overrides config) | `KAI_TELEMETRY=0 kai capture` |

See [telemetry.md](telemetry.md) for full details.

### Server & Auth

| Variable | Description | Default |
|----------|-------------|---------|
| `KAI_SERVER` | Remote server URL | `https://kaicontext.com` |
| `KAI_SSH_SIGN_KEY` | SSH key for signing operations | — |

## Authentication

### `~/.kai/credentials.json`

Managed by `kai auth login` and `kai auth logout`. Stores access and refresh tokens per server.

```bash
kai auth login              # Authenticate with remote server
kai auth status             # Check current auth state
kai auth logout             # Clear stored credentials
```

## `.kai/` Directory

Created by `kai init`. Contains local state that should be gitignored (except `rules/`).

```
.kai/
├── db.sqlite               # Graph database (WAL mode)
├── objects/                 # Content-addressable object store (blake3)
├── cache/                   # File digest cache for fast status
├── coverage-map.json        # Ingested coverage data
├── contracts.json           # Ingested contract/schema data
└── rules/                   # Committable config (ci-policy, modules)
    ├── ci-policy.yaml
    └── modules.yaml
```

Add to `.gitignore`:

```
.kai/*
!.kai/rules/
```
