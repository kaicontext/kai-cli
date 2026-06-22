# CLI Usage

Kai is a semantic version control CLI. It creates snapshots of your code, computes semantic diffs, generates intent sentences, and produces CI test plans.

## Quick Start

```bash
kai init                    # Initialize Kai in your project
kai capture                 # Snapshot + analyze your code
# ... make changes ...
kai diff                    # See what changed semantically
kai review open             # Open a code review
kai ci plan --git-range main..HEAD --out plan.json   # Generate CI test plan
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--verbose`, `-v` | Enable verbose debug output (timestamped, to stderr) |
| `--help`, `-h` | Help for any command |
| `--version` | Print version |

The `KAI_VERBOSE=1` environment variable also enables verbose mode.

---

## Getting Started

### `kai init`

Initialize Kai in the current directory. Creates the `.kai/` directory with the graph database, object store, and default config files.

```bash
kai init
kai init --explain    # Show what this command does
```

Safe to run multiple times (idempotent).

### `kai capture`

Snapshot your project and analyze it in one step. This is the recommended starting point.

Performs three steps:
1. Creates a directory snapshot
2. Extracts symbols (functions, classes, variables)
3. Builds the call/import graph

```bash
kai capture              # Capture current directory
kai capture src/         # Capture specific path
kai capture --explain    # Show what's happening
```

Equivalent to: `kai snap . && kai analyze symbols @snap:last && kai analyze calls @snap:last`

---

## Diff & Review

### `kai diff`

Show semantic differences between snapshots, or between a snapshot and the working directory.

```bash
kai diff                             # @snap:last vs working directory (default)
kai diff -p                          # Line-level diff (like git diff)
kai diff @snap:prev @snap:last       # Compare two snapshots
kai diff --name-only                 # Just file paths with A/M/D prefixes
kai diff --json                      # JSON output (implies --semantic)
kai diff --force                     # Skip stale baseline warning
```

| Flag | Description |
|------|-------------|
| `-p`, `--patch` | Line-level diff (like git diff) |
| `--semantic` | Semantic diff showing functions/classes changed (default) |
| `--name-only` | Output just paths with status prefixes |
| `--json` | JSON output |
| `--dir` | Directory to compare against (default `.`) |
| `--force` | Skip stale baseline warning |

### `kai status`

Show pending changes since the last snapshot.

```bash
kai status                           # Changes since @snap:last
kai status --against @snap:prev      # Changes since a specific snapshot
kai status --name-only               # Paths only (A/M/D prefixes)
kai status --json                    # JSON output
kai status --semantic                # Include semantic change type analysis
```

### `kai review`

Create and manage code reviews anchored to semantic entities.

#### Open a review

```bash
kai review open                                  # Auto-title from changes
kai review open -m "Fix login bug"               # Explicit title
kai review open @cs:last --title "Reduce timeout" # From specific changeset
kai review open --base @snap:prev                # Specify base snapshot
kai review open --reviewers alice --reviewers bob
```

#### Manage reviews

```bash
kai review list                       # List all reviews
kai review view <id>                  # View a review
kai review view <id> --json           # JSON output
kai review view <id> -i               # Interactive drill-down
kai review approve <id>               # Approve
kai review request-changes <id>       # Request changes
kai review close <id> --state merged  # Close as merged
kai review close <id> --state abandoned
kai review ready <id>                 # Mark draft as ready
kai review summary <id>               # Semantic summary
kai review summary <id> --ai          # AI-powered review (needs ANTHROPIC_API_KEY)
kai review export <id> --markdown     # Export as markdown
kai review export <id> --html         # Export as HTML
```

### `kai changeset`

Create and list changesets (the diff between two snapshots).

```bash
kai changeset create @snap:prev @snap:last -m "Add auth"
kai changeset create --git-base main --git-head feature   # From Git refs
kai changeset list
```

### `kai intent`

Generate human-readable intent sentences for changesets.

```bash
kai intent render @cs:last
kai intent render @cs:last --edit "Update Auth login timeout"
kai intent render @cs:last --regenerate
kai intent render @cs:last --show-alternatives
kai intent render @cs:last --explain-intent
```

---

## Query

Query the semantic graph directly from the terminal. These are CLI equivalents of the MCP tools (`kai_callers`, `kai_dependents`, `kai_impact`).

### `kai query callers`

Find all call sites of a symbol with file and line locations.

```bash
kai query callers getUser
kai query callers handleRequest --file api/v1/users.ts
```

### `kai query dependents`

Find all files that import a given file.

```bash
kai query dependents services/userService.ts
```

### `kai query impact`

Transitive downstream impact analysis — walks import and call edges, shows hop distance, separates source files from tests.

```bash
kai query impact shared/types/user.ts
kai query impact services/userService.ts --depth 5
```

---

## CI & Testing

### `kai ci plan`

Compute which tests to run based on code changes. Produces a deterministic JSON plan.

```bash
# Git-based (no .kai/ database required)
kai ci plan --git-range main..feature --out plan.json
kai ci plan --git-range $BASE_SHA..$HEAD_SHA --out plan.json

# From existing snapshots/changesets
kai ci plan                              # Uses @cs:last
kai ci plan @cs:last --out plan.json
kai ci plan @ws:feature/auth --out plan.json

# With options
kai ci plan --strategy=imports --risk-policy=expand
kai ci plan --safety-mode=shadow         # Learn mode (runs full suite)
kai ci plan --explain                    # Human-readable output
```

| Flag | Default | Description |
|------|---------|-------------|
| `--strategy` | `auto` | `auto`, `symbols`, `imports`, `coverage` |
| `--safety-mode` | `guarded` | `shadow` (learn), `guarded` (safe fallback), `strict` (no fallback) |
| `--risk-policy` | `expand` | `expand`, `warn`, `fail` |
| `--out` | stdout | Write plan JSON to file |
| `--git-range` | | Git range `BASE..HEAD` |
| `--explain` | | Human-readable explanation |

#### Using the plan output

```bash
jq -r '.mode' plan.json                  # skip, selective, expanded, shadow
jq -r '.targets.run[]' plan.json         # Test files to run
jq -r '.safety.confidence' plan.json     # 0.0-1.0
jq '.safety.structural_risks' plan.json  # Detected risks
```

### `kai ci print`

Display a plan file in human-readable format.

```bash
kai ci print --plan plan.json
kai ci print --plan plan.json --section targets
kai ci print --plan plan.json --section impact
kai ci print --plan plan.json --section causes
```

### `kai ci ingest-coverage`

Ingest test coverage reports to improve test selection accuracy.

```bash
kai ci ingest-coverage --from coverage/coverage-final.json --format nyc
kai ci ingest-coverage --from .coverage.json --format coveragepy
kai ci ingest-coverage --from build/reports/jacoco.xml --format jacoco
kai ci ingest-coverage --from coverage.json --branch main --tag nightly
```

Supported formats: `nyc` (Istanbul), `coveragepy`, `jacoco`, `auto`.

### `kai ci detect-runtime-risk`

Analyze test logs to detect if selective testing missed dependencies (runtime safety net).

```bash
kai ci detect-runtime-risk --logs ./jest-results.json
kai ci detect-runtime-risk --stderr ./test.log --plan plan.json
kai ci detect-runtime-risk --stderr ./test.log --tripwire   # Exit 75 if rerun needed
kai ci detect-runtime-risk --stderr ./test.log --tripwire --rerun-on-fail
```

Exit codes: `0` = safe, `1` = error, `75` = rerun recommended (tripwire mode).

### `kai ci` (other subcommands)

```bash
kai ci record-miss --plan plan.json --failed "test1.js,test2.js"
kai ci ingest-contracts --type openapi --path api.yaml --tests "tests/api/**"
kai ci explain-dynamic-imports <file-or-dir>
kai ci annotate-plan --fallback.used --fallback.reason runtime_tripwire
kai ci validate-plan plan.json [--strict]
```

### `kai test affected`

List test files affected by changes between two snapshots (requires `kai analyze calls` first).

```bash
kai test affected @snap:prev @snap:last
```

---

## Remote & Sync

### `kai remote`

Configure remote Kailab servers.

```bash
kai remote set origin https://kaicontext.com --tenant myorg --repo myrepo
kai remote get origin
kai remote list
kai remote del origin
```

### `kai push`

Push snapshots, changesets, and reviews to a remote server.

```bash
kai push                             # Push all snapshots, changesets, reviews
kai push origin cs:login_fix         # Push single changeset
kai push origin review:abc123        # Push a review
kai push origin --ws feature/auth    # Push a workspace
kai push --dry-run                   # Preview what would be transferred
kai push -f                          # Force push
```

### `kai fetch`

Fetch refs and objects from a remote server.

```bash
kai fetch                            # Fetch all from origin
kai fetch origin snap.main           # Fetch specific ref
kai fetch --ws feature/auth          # Fetch and recreate workspace locally
kai fetch --review abc123            # Fetch and recreate review locally
```

### `kai clone`

Clone a Kai repository from a remote server.

```bash
kai clone myorg/myrepo               # From kaicontext.com (default)
kai clone myorg/myrepo myproject     # Into specific directory
kai clone https://kaicontext.com/myorg/myrepo
kai clone http://localhost:8080/myorg/myrepo   # Local dev
```

The default server can be overridden with `KAI_SERVER`.

### `kai auth`

Manage authentication with Kailab servers.

```bash
kai auth login                       # Interactive login (uses origin remote)
kai auth login http://localhost:8080  # Login to specific server
kai auth status                      # Check auth state
kai auth logout                      # Clear credentials
```

Credentials are stored in `~/.kai/credentials.json`.

### `kai update`

Update the kai binary to the latest version.

```bash
kai update           # Download and install latest
kai update --check   # Check for updates without installing
```

---

## Advanced

### Snapshots

```bash
kai snap                             # Quick snapshot of current directory
kai snap src/                        # Snapshot specific path

kai snapshot create --dir .          # Explicit directory snapshot
kai snapshot create --git main       # Snapshot from Git ref
kai snapshot create --git abc123 -m "Before refactor"
kai snapshot list
```

### Analysis

```bash
kai analyze symbols <snapshot-id>    # Extract symbols
kai analyze calls <snapshot-id>      # Build call/import graph
kai analyze deps <snapshot-id>       # Alias for 'calls'
```

### Workspaces

Workspaces are lightweight, mutable overlays on top of immutable snapshots.

```bash
# Create
kai ws create feat/demo                      # Auto-snapshot current dir as base
kai ws create feat/demo --from-git main      # Base from Git ref
kai ws create feat/demo --base @snap:last    # Base from existing snapshot

# Work
kai ws stage feat/demo                       # Stage changes from current dir
kai ws stage -m "Add login form"             # With message
kai ws stage --sign-ssh-key ~/.ssh/id_ed25519

# Manage
kai ws list
kai ws current                               # Show current workspace
kai ws checkout feat/demo --dir .            # Checkout workspace files
kai ws log --ws feat/demo                    # Show changelog
kai ws shelve --ws feat/demo                 # Freeze (pause work)
kai ws unshelve --ws feat/demo               # Resume
kai ws close --ws feat/demo                  # Close permanently
kai ws delete --ws feat/demo                 # Delete metadata
kai ws delete --ws feat/demo --dry-run       # Preview deletion
```

### Integration & Merge

```bash
kai integrate --ws feat/demo --into <target-snapshot-id>
kai merge base.js left.js right.js           # AST-aware 3-way merge
kai merge base.py a.py b.py --lang py -o merged.py
kai merge base.js a.js b.js --json           # JSON output with conflicts
kai cherry-pick <changeset-id> <target-snapshot-id>
kai rebase <workspace> --onto <snapshot-id>
```

### Refs & Tags

```bash
kai ref list                         # List all refs
kai ref list --kind Snapshot         # Filter by kind
kai ref set myref <target-id>
kai ref del myref

kai tag create v1.0 @snap:last
kai tag list
kai tag delete v1.0
```

### Modules

```bash
kai modules init --infer             # Preview auto-detected modules
kai modules init --infer --write     # Save to .kai/rules/modules.yaml
kai modules init --infer --by dirs --tests "tests/**"
kai modules add Auth src/auth/**
kai modules list
kai modules show Auth
kai modules preview                  # Show file-to-module mapping
kai modules rm Auth
```

### Other

```bash
kai log                              # Chronological log (last 10)
kai log -n 20                        # Show more entries
kai dump <changeset-id>              # Dump changeset as JSON
kai dump <changeset-id> --json

kai pick <query>                     # Interactive node search
kai pick <query> --no-ui             # Non-interactive output
kai pick <query> --filter substr

kai prune --dry-run                  # Preview garbage collection
kai prune --yes                      # Actually delete
kai prune --since 7 --yes            # Only older than 7 days
kai prune --aggressive --yes         # Also sweep orphaned symbols/modules

kai bisect start <good> <bad>        # Binary search for regression
kai bisect good / bad / skip / next / reset

kai shadow import --git-range main..HEAD    # Import Git range into Kai
kai shadow parity --git-range main..HEAD    # Compare Git diff vs Kai diff
kai shadow drift --git-ref HEAD --snap snap.main  # Detect drift

kai remote-log                       # Show remote ref history
kai remote-log --ref snap.main -n 50

kai completion bash                  # Generate shell completions
kai completion zsh
kai completion fish
```

---

## Selectors

Many commands accept selectors to reference snapshots, changesets, and workspaces:

| Selector | Description |
|----------|-------------|
| `@snap:last` | Most recent snapshot |
| `@snap:prev` | Previous snapshot |
| `@cs:last` | Most recent changeset |
| `@ws:name` | Workspace by name |
| `abc123...` | Direct ID (hex, can be truncated) |
| `snap.latest` | Named ref |
| `cs.latest` | Named ref |

---

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `KAI_VERBOSE` | Enable debug output (`1` or `true`) | off |
| `KAI_SERVER` | Remote server URL | `https://kaicontext.com` |
| `KAI_SSH_SIGN_KEY` | SSH key for signing | -- |
| `KAI_FORCE_FULL` | Force full test suite | off |
| `KAI_PANIC` | Panic switch (same as `KAI_FORCE_FULL`) | off |
| `KAI_CI_STRATEGY` | Override CI strategy | `auto` |
| `KAI_CI_SAFETY_MODE` | Override safety mode | `guarded` |
| `KAI_CI_RISK_POLICY` | Override risk policy | `expand` |

---

## Shell Completion

```bash
# Bash
kai completion bash > /etc/bash_completion.d/kai

# Zsh
kai completion zsh > "${fpath[1]}/_kai"

# Fish
kai completion fish > ~/.config/fish/completions/kai.fish
```
