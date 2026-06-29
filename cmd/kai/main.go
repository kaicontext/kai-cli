// Package main provides the kai CLI.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/mattn/go-isatty"
	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kaicontext/kai-core/diff"
	"github.com/kaicontext/kai-core/merge"

	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/ai"
	"github.com/kaicontext/kai-engine/authorship"
	"github.com/kaicontext/kai-engine/classify"
	"kai/internal/config"
	semanticdiff "github.com/kaicontext/kai-engine/diff"
	"github.com/kaicontext/kai-engine/dirio"
	"github.com/kaicontext/kai-engine/explain"
	"github.com/kaicontext/kai-engine/filesource"
	"github.com/kaicontext/kai-engine/gitio"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/intent"
	"github.com/kaicontext/kai-engine/kaipath"
	"github.com/kaicontext/kai-engine/memstat"
	"github.com/kaicontext/kai-engine/module"
	"github.com/kaicontext/kai-engine/parse"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/ref"
	"github.com/kaicontext/kai-engine/remote"
	"github.com/kaicontext/kai-engine/review"
	"github.com/kaicontext/kai-engine/safetygate"
	"github.com/kaicontext/kai-engine/snapshot"
	"github.com/kaicontext/kai-engine/status"
	"github.com/kaicontext/kai-engine/telemetry"
	tuierrors "kai/internal/tui/errors"
	"github.com/kaicontext/kai-engine/util"
	"github.com/kaicontext/kai-engine/workspace"
	spawnpkg "kai/pkg/spawn"
)

const (
	dbFile               = "db.sqlite"
	objectsDir           = "objects"
	schemaDir            = "schema"
	modulesFile          = "kai.modules.yaml"
	ciPolicyFileFallback = "kai.ci-policy.yaml" // Legacy location for backwards compat
	workspaceFile        = "workspace"          // stores current workspace name
)

// kaiDir is the project's kai data directory, resolved against cwd at
// process start. New git repos land in .git/kai (auto-ignored by git);
// already-initialized projects keep .kai for backward compat.
// Set $KAI_DIR to override.
var kaiDir = kaipath.Resolve(".")

// ciPolicyFile is the primary CI policy location (inside kaiDir).
var ciPolicyFile = filepath.Join(kaiDir, "rules", "ci-policy.yaml")

// Version is the current kai CLI version
var Version = "0.33.3"

// verbose enables debug output when --verbose/-v flag or KAI_VERBOSE env var is set
var verbose bool
var authLoginToken string

// updateCheckFile is the path to the cached update check result.
var updateCheckFile = filepath.Join(os.Getenv("HOME"), ".kai", "update-check.json")

type updateCheck struct {
	LatestVersion string `json:"latest_version"`
	CheckedAt     string `json:"checked_at"`
}

// printUpdateNotice reads the cached update check and prints a notice if a newer version exists.
func printUpdateNotice() {
	data, err := os.ReadFile(updateCheckFile)
	if err != nil {
		return
	}
	var uc updateCheck
	if json.Unmarshal(data, &uc) != nil || uc.LatestVersion == "" {
		return
	}
	if isNewerVersion(uc.LatestVersion, Version) {
		fmt.Fprintf(os.Stderr, "Update available: %s → %s (run `kai update`)\n", Version, uc.LatestVersion)
	}
}

// isNewerVersion returns true if a is newer than b using semver comparison.
func isNewerVersion(a, b string) bool {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		ai, _ := strconv.Atoi(aParts[i])
		bi, _ := strconv.Atoi(bParts[i])
		if ai > bi {
			return true
		}
		if ai < bi {
			return false
		}
	}
	return len(aParts) > len(bParts)
}

// backgroundUpdateCheck checks GitHub for the latest release tag and caches the result.
// Runs at most once per 24 hours.
func backgroundUpdateCheck() {
	// Rate limit: check at most once per 24h
	if data, err := os.ReadFile(updateCheckFile); err == nil {
		var uc updateCheck
		if json.Unmarshal(data, &uc) == nil && uc.CheckedAt != "" {
			if t, err := time.Parse(time.RFC3339, uc.CheckedAt); err == nil {
				if time.Since(t) < 24*time.Hour {
					return
				}
			}
		}
	}

	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("https://api.github.com/repos/kaicontext/kai-cli/releases/latest")
		if err != nil || resp.StatusCode != 200 {
			return
		}
		defer resp.Body.Close()

		var release struct {
			TagName string `json:"tag_name"`
		}
		if json.NewDecoder(resp.Body).Decode(&release) != nil || release.TagName == "" {
			return
		}

		latest := strings.TrimPrefix(release.TagName, "v")
		uc := updateCheck{
			LatestVersion: latest,
			CheckedAt:     time.Now().UTC().Format(time.RFC3339),
		}
		if out, err := json.Marshal(uc); err == nil {
			os.MkdirAll(filepath.Dir(updateCheckFile), 0755)
			os.WriteFile(updateCheckFile, out, 0644)
		}
	}()
}

// TODO: consider grouping subcommands by domain
var rootCmd = &cobra.Command{
	Use:     "kai",
	Short:   "Kai - semantic, intent-based version control",
	Long:    `Kai is a local CLI that creates semantic snapshots from Git refs, computes changesets, classifies change types, and generates intent sentences.`,
	Version: Version,
	// Bare `kai` prints help. The interactive coding experience is
	// launched explicitly via `kai code`, which resolves (and
	// self-installs) the managed `kit` binary and hands off to it
	// (see cmd/kai/code.go and internal/kitlauncher; kit-in-kai Phase 1).
	SilenceUsage:  true,
	SilenceErrors: false,
}

// Command groups for organized help output
const (
	groupStart    = "start"
	groupDiff     = "diff"
	groupQuery    = "query"
	groupCI       = "ci"
	groupRemote   = "remote"
	groupAdvanced = "advanced"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize Kai in the current directory",
	Long: `Initialize Kai in the current directory: creates the semantic
graph, optionally imports git history, installs hooks and the MCP
server, signs you up for kaicontext.com, and pushes the first
capture.

Bring Your Own Model:
  By default, the kai coding experience (kai code) uses kailab as the
  LLM provider (one bearer, server holds the upstream key). To use your
  own keys instead, set KAI_PROVIDER before running kai code:

    KAI_PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... kai code
    KAI_PROVIDER=openai    OPENAI_API_KEY=sk-...        kai code

  Local OpenAI-compatible endpoints (Ollama, vLLM, LM Studio):

    KAI_PROVIDER=openai \
      KAI_OPENAI_BASE_URL=http://localhost:11434/v1 \
      KAI_OPENAI_MODEL=llama3.1:70b kai code

  See ` + "`kai auth status`" + ` for the active provider and tradeoffs,
  or the README's "Bring Your Own Model" section.`,
	RunE: runInit,
}

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Snapshot commands",
}

var snapshotCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a snapshot from a Git ref or directory",
	Long: `Create a snapshot from a Git ref or directory.

IMPORTANT: You must be explicit about the source using --git or --dir.

Git Snapshot:
  kai snapshot create --git main           # Snapshot from Git branch
  kai snapshot create --git feature/login  # Snapshot from branch
  kai snapshot create --git abc123def      # Snapshot from commit hash

Directory Snapshot:
  kai snapshot create --dir .              # Snapshot current directory
  kai snapshot create --dir ./src          # Snapshot specific path

For a quick directory snapshot, use 'kai snap' instead.
For the full workflow (snapshot + analyze), use 'kai capture'.`,
	RunE: runSnapshot,
}

var snapCmd = &cobra.Command{
	Use:   "snap [path]",
	Short: "Quick directory snapshot (no Git)",
	Long: `Create a snapshot from a directory, ignoring Git entirely.

This is the recommended shortcut for the common case of snapshotting
your current working directory.

Examples:
  kai snap                # Snapshot current directory
  kai snap src/           # Snapshot specific path
  kai snap ./build        # Snapshot build output

Equivalent to 'kai snapshot create --dir <path>'.

This command:
  - Never reads Git
  - Includes uncommitted changes
  - Works without a Git repository
  - Is ideal for workspaces, CI, and local development`,
	RunE: runSnap,
}

var captureCmd = &cobra.Command{
	Use:   "capture [path]",
	Short: "Capture your project (snapshot + analyze) in one step",
	Long: `Captures your codebase in one simple command.

This is the recommended way to get started with Kai. It performs:
  1. Creates a snapshot of your project
  2. Analyzes symbols (functions, classes, variables)
  3. Builds the call graph (imports, dependencies)
  4. Updates module mappings

Examples:
  kai capture              # Capture current directory
  kai capture src/         # Capture specific path
  kai capture --explain    # Show what's happening

This is equivalent to running:
  kai snap . && kai analyze symbols @snap:last && kai analyze calls @snap:last

The capture command is the first step in the "2-minute value path":
  kai capture → kai diff → kai review open → kai ci plan`,
	RunE: runCapture,
}

var primeCmd = &cobra.Command{
	Use:   "prime <query>",
	Short: "Output pre-injection context for a query (used by Claude Code hooks)",
	Long: `Queries the semantic graph to find files and symbols relevant to a
natural language query, then outputs structured context to stdout.

Designed to be called from a Claude Code SessionStart hook so that
Claude receives the right code context before it starts reasoning —
eliminating expensive exploration round-trips.

Examples:
  kai prime "fix the login bug"
  kai prime "database migration"
  kai prime "add rate limiting to the API"

Hook setup (in .claude/settings.local.json):
  {
    "hooks": {
      "SessionStart": [{
        "matcher": "",
        "hooks": [{"type": "command", "command": "kai prime \"$QUERY\""}]
      }]
    }
  }`,
	Args: cobra.ExactArgs(1),
	RunE: runPrime,
}

var (
	importAll bool
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import git history as semantic snapshots",
	Long: `Replays git commits as Kai snapshots, building semantic history.

By default imports the last 50 commits. Use --all for full history.

Examples:
  kai import              # Import last 50 commits
  kai import --all        # Import entire git history
  kai import --all --max 200  # Import last 200 commits`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(".git"); os.IsNotExist(err) {
			return fmt.Errorf("not a git repository")
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		if importAll {
			importMaxCommits = 999999
		}
		return runGitImport(db)
	},
}

var importMaxCommits = 50

var analyzeCmd = &cobra.Command{
	Use:   "analyze",
	Short: "Analysis commands",
}

var analyzeSymbolsCmd = &cobra.Command{
	Use:   "symbols <snapshot-id>",
	Short: "Extract symbols from a snapshot",
	Args:  cobra.ExactArgs(1),
	RunE:  runAnalyzeSymbols,
}

var analyzeCallsCmd = &cobra.Command{
	Use:   "calls <snapshot-id>",
	Short: "Extract function calls and imports from a snapshot (JS/TS)",
	Long: `Analyzes JavaScript and TypeScript files to build a call graph.

Creates the following relationships:
  - File --IMPORTS--> File (import dependencies)
  - File --CALLS--> File (function call relationships)
  - File --TESTS--> File (test file to source file mapping)

This enables features like:
  - Finding all callers of a function
  - Determining which tests cover a file
  - Running only affected tests after changes`,
	Args: cobra.ExactArgs(1),
	RunE: runAnalyzeCalls,
}

var analyzeDepsCmd = &cobra.Command{
	Use:   "deps <snapshot-id>",
	Short: "Build the import/dependency graph for a snapshot (alias for 'calls')",
	Long: `Analyzes JavaScript and TypeScript files to build the import dependency graph.

This is an alias for 'kai analyze calls'. It creates the following relationships:
  - File --IMPORTS--> File (import dependencies)
  - File --CALLS--> File (function call relationships)
  - File --TESTS--> File (test file to source file mapping)

Use this to enable selective CI testing based on which files changed.`,
	Args: cobra.ExactArgs(1),
	RunE: runAnalyzeCalls,
}

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Test-related commands",
}

var testAffectedCmd = &cobra.Command{
	Use:   "affected <base-snap> <head-snap>",
	Short: "List test files affected by changes between two snapshots",
	Long: `Analyzes the call graph to find test files that should be run based on changes.

Compares two snapshots and identifies which test files are affected by the changes.
This command requires running 'kai analyze calls' first to build the call graph.

Example:
  # Find affected tests between two snapshots
  kai test affected @snap:prev @snap:last

  # Using explicit snapshot IDs
  kai test affected abc123 def456`,
	Args: cobra.ExactArgs(2),
	RunE: runTestAffected,
}

// Query commands - inspect the semantic graph from the CLI
var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query the semantic graph",
	Long: `Query commands let you inspect the semantic graph from the terminal.

These are the same queries available via MCP tools (kai_callers, kai_dependents, etc.)
but accessible directly from the command line.`,
}

var queryCallersCmd = &cobra.Command{
	Use:   "callers <symbol> [--file <path>]",
	Short: "Find all callers of a symbol",
	Long: `Find all files and locations that call a given symbol.

Examples:
  kai query callers getUser
  kai query callers handleRequest --file api/v1/users.ts`,
	Args: cobra.ExactArgs(1),
	RunE: runQueryCallers,
}

var queryDependentsCmd = &cobra.Command{
	Use:   "dependents <file>",
	Short: "Find all files that import a given file",
	Long: `Find all files that depend on (import from) the given file.

Examples:
  kai query dependents shared/types/user.ts
  kai query dependents services/userService.ts`,
	Args: cobra.ExactArgs(1),
	RunE: runQueryDependents,
}

var queryImpactCmd = &cobra.Command{
	Use:   "impact <file> [--depth <n>]",
	Short: "Show transitive downstream impact of changing a file",
	Long: `Walks the import and call graph to find all files transitively affected
by changes to the given file. Separates source files from test files.

Examples:
  kai query impact shared/types/user.ts
  kai query impact services/userService.ts --depth 5`,
	Args: cobra.ExactArgs(1),
	RunE: runQueryImpact,
}

var queryFileFlag string
var queryDepthFlag int

// CI commands - runner-agnostic selective CI
var ciCmd = &cobra.Command{
	Use:   "ci",
	Short: "CI/CD commands for selective test/build execution",
	Long: `Runner-agnostic CI commands that produce deterministic plans.

Kai CI computes what targets (tests, builds, etc.) are affected by changes,
outputting a neutral JSON plan that any CI system can consume.

The CLI never runs tests or builds - it just determines what to run.`,
}

var ciPlanCmd = &cobra.Command{
	Use:   "plan [changeset|selector]",
	Short: "Compute a selection plan for affected targets",
	Long: `Analyzes changes and computes which targets should run.

Produces a deterministic JSON plan listing affected paths/globs that
any CI system can consume. The CLI is tool-neutral - it never runs
tests or builds directly.

Strategies:
  symbols   - Use symbol-level dependency graph (most precise)
  imports   - Use file-level import graph
  coverage  - Use learned test↔file mappings
  auto      - Try symbols → imports → coverage (default)

Risk policies:
  expand    - Widen selection when uncertain (safe default)
  warn      - Keep minimal plan but mark risk higher
  fail      - Exit non-zero on uncertainty

Examples:
  kai ci plan                  # Uses @cs:last by default
  kai ci plan @cs:last --out plan.json
  kai ci plan @cs:last --strategy=imports --risk-policy=expand
  kai ci plan @ws:feature/auth --out plan.json --json

Git-based CI (no .kai/ database required):
  kai ci plan --git-range main..feature --out plan.json
  kai ci plan --git-range $CI_MERGE_REQUEST_DIFF_BASE_SHA..$CI_COMMIT_SHA`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runCIPlan,
}

var ciPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print a selection plan for humans or CI logs",
	Long: `Displays the contents of a plan file in a human-readable format.

Use --section to show specific parts:
  targets   - What to run/skip
  impact    - What changed
  causes    - Why each test was selected (root cause analysis)
  safety    - Safety analysis details
  summary   - Overview (default)

Examples:
  kai ci print --plan plan.json
  kai ci print --plan plan.json --section targets
  kai ci print --plan plan.json --section causes
  kai ci print --plan plan.json --json`,
	RunE: runCIPrint,
}

var ciDetectRuntimeRiskCmd = &cobra.Command{
	Use:   "detect-runtime-risk",
	Short: "Analyze test logs for runtime risk signals (tripwire)",
	Long: `Analyzes test output/logs to detect runtime signals that indicate
the selective test plan may have missed dependencies. This is the RUNTIME
SAFETY NET that catches selection misses after tests run.

Detects:
  - Cannot find module / Module not found (Node.js)
  - ImportError / ModuleNotFoundError (Python)
  - Go plugin load failures
  - TypeScript type error bursts
  - Jest/Mocha/pytest setup/fixture failures
  - importlib errors (Python dynamic imports)
  - Any dependency resolution errors

Exit Codes:
  0   - No risks detected, selection was safe
  1   - Error running the command
  75  - TRIPWIRE: Rerun full suite recommended (--tripwire mode)

Tripwire Mode (--tripwire):
  In tripwire mode, outputs only RERUN or OK and exits with code 75 or 0.
  Use this in CI to conditionally trigger a full suite rerun:

    kai ci detect-runtime-risk --stderr test.log --tripwire || npm run test:full

Examples:
  # Analyze Jest output
  kai ci detect-runtime-risk --logs ./jest-results.json

  # With plan cross-reference
  kai ci detect-runtime-risk --logs ./jest-results.json --plan plan.json

  # Tripwire mode for CI
  kai ci detect-runtime-risk --stderr ./test.log --tripwire

  # Treat any failure as tripwire
  kai ci detect-runtime-risk --stderr ./test.log --tripwire --rerun-on-fail`,
	RunE: runCIDetectRuntimeRisk,
}

var ciRecordMissCmd = &cobra.Command{
	Use:   "record-miss",
	Short: "Record a test selection miss for shadow mode learning",
	Long: `Records information about tests that failed but were not selected,
allowing Kai to learn and improve its selection accuracy over time.

Used in shadow mode to compare what was predicted vs what actually failed.
This data is used to identify missing dependency edges and improve the
test selection algorithm.

Examples:
  kai ci record-miss --plan plan.json --evidence ./test-results.json
  kai ci record-miss --plan plan.json --failed "tests/auth.test.js,tests/api.test.js"`,
	RunE: runCIRecordMiss,
}

var ciExplainDynamicImportsCmd = &cobra.Command{
	Use:   "explain-dynamic-imports [path]",
	Short: "Analyze and explain dynamic imports in a file or directory",
	Long: `Scans files for dynamic imports and shows how they would affect test selection.

This helps developers understand before committing:
- What dynamic imports exist in their code
- Whether they are bounded or unbounded
- What expansion strategy would be used
- What tests would be affected

Examples:
  kai ci explain-dynamic-imports src/
  kai ci explain-dynamic-imports src/plugins/loader.js
  kai ci explain-dynamic-imports . --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCIExplainDynamicImports,
}

var ciIngestCoverageCmd = &cobra.Command{
	Use:   "ingest-coverage",
	Short: "Ingest coverage reports to build file→test mappings",
	Long: `Ingests test coverage reports to build a mapping of which tests
exercise which source files. This data is used during plan generation
to select tests that recently covered changed files.

Supported formats:
  - NYC/Istanbul JSON (coverage-final.json)
  - coverage.py JSON
  - JaCoCo XML

The coverage map is stored in .kai/coverage-map.json and used during
plan generation when coverage.enabled=true in ci-policy.yaml.

Examples:
  # Ingest NYC/Istanbul coverage
  kai ci ingest-coverage --from coverage/coverage-final.json --format nyc

  # Ingest Python coverage.py
  kai ci ingest-coverage --from .coverage.json --format coveragepy

  # Ingest JaCoCo XML
  kai ci ingest-coverage --from build/reports/jacoco.xml --format jacoco

  # Tag with branch and run ID
  kai ci ingest-coverage --from coverage.json --branch main --tag nightly-2025-12-06`,
	RunE: runCIIngestCoverage,
}

var ciIngestContractsCmd = &cobra.Command{
	Use:   "ingest-contracts",
	Short: "Register contract schemas and their associated tests",
	Long: `Registers API contracts/schemas and links them to their contract tests.
When a registered schema changes, the linked tests are automatically selected.

Supported schema types:
  - OpenAPI (YAML/JSON)
  - Protobuf (.proto)
  - GraphQL (.graphql/SDL)

The contract registry is stored in .kai/contracts.json.

Examples:
  # Register an OpenAPI schema
  kai ci ingest-contracts --type openapi --path api/openapi.yaml \
    --service billing --tests "tests/contract/billing/**"

  # Register a protobuf schema
  kai ci ingest-contracts --type protobuf --path proto/user.proto \
    --service users --tests "tests/contract/users/**"

  # Register with generated file tracking
  kai ci ingest-contracts --type openapi --path api/openapi.yaml \
    --service billing --tests "tests/contract/**" \
    --generated "src/clients/billing/**"`,
	RunE: runCIIngestContracts,
}

var ciAnnotatePlanCmd = &cobra.Command{
	Use:   "annotate-plan <plan-file>",
	Short: "Annotate a plan with fallback/tripwire information",
	Long: `Updates a plan.json file with fallback status after a CI run.
Used to record when tripwire fallback was triggered for auditability.

This creates an audit trail showing why full suite was run:
- runtime_tripwire: Tests failed with import/module errors
- planner_over_threshold: Confidence too low at planning time
- panic_switch: Manual override via KAI_FORCE_FULL

Examples:
  # Record tripwire fallback
  kai ci annotate-plan plan.json \
    --fallback.used=true \
    --fallback.reason=runtime_tripwire \
    --fallback.trigger="Cannot find module" \
    --fallback.exitCode=75

  # Record panic switch
  kai ci annotate-plan plan.json \
    --fallback.used=true \
    --fallback.reason=panic_switch`,
	Args: cobra.ExactArgs(1),
	RunE: runCIAnnotatePlan,
}

var ciValidatePlanCmd = &cobra.Command{
	Use:   "validate-plan <plan-file>",
	Short: "Validate plan JSON schema and required fields",
	Long: `Validates that a plan.json file has all required fields with correct types.

Checks:
- Required fields: mode, risk, confidence, uncertainty.score, fallback.used
- Provenance fields: kaiVersion, detectorVersion, generatedAt
- Type validation for all fields

Exit codes:
  0 - Plan is valid
  1 - Plan is invalid or error reading file

Examples:
  kai ci validate-plan plan.json
  kai ci validate-plan plan.json --strict  # Also validate optional fields`,
	Args: cobra.ExactArgs(1),
	RunE: runCIValidatePlan,
}

// CI command flags
var (
	ciStrategy   string
	ciRiskPolicy string
	ciOutFile    string
	ciSafetyMode string // "shadow", "guarded", "strict"
	ciExplain    bool   // Output human-readable explanation
	ciGitRange   string // BASE..HEAD format for git-based CI plan
	ciGitRepo    string // Git repo path for --git-range
	ciNoFast     bool   // Skip fast path, force full snapshot
	ciPlanFile   string
	ciSection    string
	// detect-runtime-risk flags
	ciLogsFile    string
	ciStderrFile  string
	ciLogFormat   string
	ciTripwire    bool // Just output tripwire status and exit code
	ciRerunOnFail bool // Recommend rerun on any failure
	// record-miss flags
	ciEvidenceFile string
	ciFailedTests  string
	// ingest-coverage flags
	ciCoverageFrom   string
	ciCoverageFormat string
	ciCoverageBranch string
	ciCoverageTag    string
	// ingest-contracts flags
	ciContractType      string
	ciContractPath      string
	ciContractService   string
	ciContractTests     string
	ciContractGenerated string
	// annotate-plan flags
	ciFallbackUsed     bool
	ciFallbackReason   string
	ciFallbackTrigger  string
	ciFallbackExitCode int
	// validate-plan flags
	ciValidateStrict bool
	// ci comment flags
	ciCommentReport string
	ciCommentToken  string
	ciCommentRepo   string
	ciCommentPR     int
	ciCommentDryRun bool
	// ci authorship flags
	ciAuthorshipToken  string
	ciAuthorshipRepo   string
	ciAuthorshipPR     int
	ciAuthorshipDryRun bool

	// Remote CI flags
	ciRunsLimit int
	ciLogsJob   string
	ciTraceJob  string
)

var changesetCmd = &cobra.Command{
	Use:   "changeset",
	Short: "ChangeSet commands",
}

var changesetCreateCmd = &cobra.Command{
	Use:   "create [base-snap] [head-snap]",
	Short: "Create a changeset between two snapshots",
	Long: `Create a changeset between two snapshots.

You can specify snapshots by ID/ref, or create them on-the-fly from Git refs:

  kai changeset create snap.main snap.feature      # Using snapshot IDs/refs
  kai changeset create --git-base main --git-head feature  # From Git refs

The --git-base and --git-head flags create ephemeral snapshots from Git refs,
useful for CI pipelines where you don't have a persistent .kai database.`,
	Args: cobra.MaximumNArgs(2),
	RunE: runChangesetCreate,
}

var intentCmd = &cobra.Command{
	Use:   "intent [statement]",
	Short: "Open a verification contract (or manage changeset intent)",
	Long: "kai intent \"<statement>\" opens a verification contract for work done " +
		"outside kit, so the working tree can be held accountable to declared intent.\n" +
		"The 'render' subcommand still renders a changeset's generated intent.",
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return runIntentOpen(cmd, args)
	},
}

var intentRenderCmd = &cobra.Command{
	Use:   "render <changeset-id>",
	Short: "Render or edit the intent for a changeset",
	Args:  cobra.ExactArgs(1),
	RunE:  runIntentRender,
}

var dumpCmd = &cobra.Command{
	Use:   "dump <changeset-id>",
	Short: "Dump a changeset as JSON",
	Args:  cobra.ExactArgs(1),
	RunE:  runDump,
}

var listCmd = &cobra.Command{
	Use:        "list",
	Short:      "List resources (deprecated: use 'kai snapshot list' or 'kai changeset list')",
	Deprecated: "use 'kai snapshot list' or 'kai changeset list' instead",
}

var listSnapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "List all snapshots (deprecated: use 'kai snapshot list')",
	RunE:  runListSnapshots,
}

var listChangesetsCmd = &cobra.Command{
	Use:   "changesets",
	Short: "List all changesets (deprecated: use 'kai changeset list')",
	RunE:  runListChangesets,
}

// snapshotListJSON is the --json flag for "kai snapshot list".
// When true, runListSnapshots emits a JSON array of snapshot
// records so machine consumers (kai-desktop sidebar, dashboards)
// can count and inspect snapshots without parsing the tabular
// text format. 2026-05-26: kai-desktop sidebar shipped a
// "Snapshots" stat card that called `kai snapshot list --json`
// expecting an array; the flag did not exist; the card sat at 0.
// Adding it brings snapshot in line with stats and spawn list,
// both of which already support --json.
var snapshotListJSON bool

var snapshotListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all snapshots",
	RunE:  runListSnapshots,
}

var changesetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all changesets",
	RunE:  runListChangesets,
}

var listSymbolsCmd = &cobra.Command{
	Use:   "symbols <snapshot-id>",
	Short: "List symbols extracted from a snapshot",
	Long: `List all symbols (functions, classes, methods, etc.) extracted from a snapshot.

Symbols are extracted by 'kai analyze symbols'. This command shows what was found,
grouped by file.

Example:
  kai list symbols @snap:last`,
	Args: cobra.ExactArgs(1),
	RunE: runListSymbols,
}

var logCmd = &cobra.Command{
	Use:   "log",
	Short: "Show chronological log of snapshots and changesets",
	Long: `Show chronological log of snapshots, like git log.

Examples:
  kai log                          # Last 10 snapshots
  kai log -n 20                    # Last 20
  kai log --oneline                # Compact one-line format
  kai log --author="Jacob"         # Filter by author
  kai log --grep="auth"            # Search commit messages
  kai log --since="2 weeks ago"    # Date range
  kai log --stat                   # Show file changes per snapshot`,
	RunE: runLog,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Kai status and pending changes",
	RunE:  runStatus,
}

var (
	blameSummary bool
	blameJSON    bool
)

var blameCmd = &cobra.Command{
	Use:   "blame <file>",
	Short: "Show AI vs human authorship per line",
	Long: `Shows line-by-line attribution for a file, indicating whether each
section was written by an AI agent or a human, and which agent/model if AI.

Examples:
  kai blame src/auth.go              # Line ranges with attribution
  kai blame src/auth.go --summary    # Just percentages
  kai blame src/auth.go --json       # Machine-readable output`,
	Args: cobra.ExactArgs(1),
	RunE: runBlame,
}

var (
	statsJSON bool
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show AI vs human authorship statistics",
	Long: `Shows project-wide statistics on AI vs human code authorship.

Examples:
  kai stats                # Overall percentages
  kai stats --json         # Machine-readable output`,
	RunE: runStats,
}

var (
	checkpointAgent string
	checkpointModel string
	checkpointFile  string
	checkpointLines string
)

var checkpointCmd = &cobra.Command{
	Use:   "checkpoint",
	Short: "Record an AI edit event (called from Claude Code PostToolUse hook)",
	Long: `Records a line-level authorship checkpoint for an edit that came through
an AI coding assistant's tool runner (Claude Code Edit/Write/MultiEdit,
Cursor compose, etc.). This is the honest replacement for the session-
presence heuristic: only edits the agent actually made are attributed,
human keystrokes stay human.

Normally invoked from a Claude Code PostToolUse hook:

  ~/.claude/settings.local.json:
  {
    "hooks": {
      "PostToolUse": [{
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [{"type": "command", "command": "kai checkpoint --agent claude-code"}]
      }]
    }
  }

The hook pipes JSON to this command's stdin containing the tool name,
input (file path + old/new strings), and response. kai parses that
payload, computes the exact changed line range in the post-edit file,
and writes a checkpoint that kai capture will consolidate on next run.

Flags override what can be inferred from stdin — useful for manual
invocation or other AI clients.`,
	RunE: runCheckpoint,
}

var diffCmd = &cobra.Command{
	Use:   "diff [base-ref] [head-ref]",
	Short: "Show semantic differences between snapshots",
	Long: `Show semantic differences between snapshots.

With no arguments, compares the last snapshot against the working directory.
This is the recommended way to see what changed after 'kai capture'.

By default shows semantic diff (functions, classes, JSON keys changed).
Use -p/--patch for git-style line-level diff.

Examples:
  kai diff                         # Semantic diff: @snap:last vs working directory
  kai diff -p                      # Line-level diff like git
  kai diff @snap:prev @snap:last   # Compare two snapshots
  kai diff --name-only             # Just file paths
  kai diff --stat                  # Summary with insertions/deletions`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runDiff,
}

// Workspace commands
var wsCmd = &cobra.Command{
	Use:   "ws",
	Short: "Workspace (branch) commands",
	Long:  `Workspaces are lightweight, isolated, mutable overlays on top of immutable snapshots.`,
}

var wsCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new workspace",
	Long: `Create a new workspace for parallel development.

Base snapshot options (must choose one or let it default):
  --from-dir <path>    Create base from directory snapshot
  --from-git <ref>     Create base from Git commit/branch/tag
  --base <selector>    Use existing snapshot as base

If no base is specified, Kai automatically snapshots the current directory.

Examples:
  kai ws create feat/demo                    # Auto-snapshot current dir as base
  kai ws create feat/demo --from-dir .       # Explicit directory snapshot
  kai ws create feat/demo --from-git main    # From Git branch
  kai ws create feat/demo --base @snap:last  # From existing snapshot

The workspace name can be provided as a positional argument or via --name.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWsCreate,
}

var wsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all workspaces",
	RunE:  runWsList,
}

var wsStageCmd = &cobra.Command{
	Use:   "stage [workspace]",
	Short: "Stage changes into a workspace",
	Long: `Stage changes from the current directory into a workspace.

The workspace can be specified as:
  1. Positional argument: kai ws stage feat/demo
  2. Flag: kai ws stage --ws feat/demo
  3. Implicit (if checked out): kai ws stage

If no workspace is specified and you're checked out on a workspace,
that workspace is used automatically.

Examples:
  kai ws stage feat/demo     # Stage into feat/demo
  kai ws stage               # Stage into current workspace (if checked out)
  kai ws stage --ws feat/demo --dir src/  # Stage specific directory`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWsStage,
}

var wsLogCmd = &cobra.Command{
	Use:   "log",
	Short: "Show workspace changelog",
	RunE:  runWsLog,
}

var wsShelveCmd = &cobra.Command{
	Use:   "shelve",
	Short: "Shelve a workspace (freeze staging)",
	RunE:  runWsShelve,
}

var wsUnshelveCmd = &cobra.Command{
	Use:   "unshelve",
	Short: "Unshelve a workspace (resume staging)",
	RunE:  runWsUnshelve,
}

var wsCloseCmd = &cobra.Command{
	Use:   "close",
	Short: "Close a workspace (permanent)",
	RunE:  runWsClose,
}

var wsDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a workspace (metadata and refs; run `kai prune` to reclaim storage)",
	Long: `Delete a workspace permanently, removing the workspace node, edges, and refs.

Content (snapshots, changesets, files) is NOT deleted - that's the GC's job.
Run 'kai prune' after deleting workspaces to reclaim storage.

Examples:
  kai ws delete --ws feature/experiment --dry-run  # Preview what would be deleted
  kai ws delete --ws feature/experiment            # Actually delete
  kai ws delete --ws old-branch --keep-refs        # Keep refs (rare)`,
	RunE: runWsDelete,
}

var wsCheckoutCmd = &cobra.Command{
	Use:   "checkout [workspace]",
	Short: "Checkout workspace and set as current",
	Long: `Checkout a workspace's head snapshot and set it as the current workspace.

This writes files from the workspace's current state to a directory and
sets .kai/workspace so subsequent commands use this workspace by default.

Examples:
  kai ws checkout feat/demo              # Checkout and set as current
  kai ws checkout feat/demo --dir ./src  # Checkout to specific directory
  kai ws checkout feat/demo --clean      # Remove files not in snapshot`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWsCheckout,
}

// wsCurrentJSON is the --json flag on `kai ws current`. When true,
// runWsCurrent emits a structured {"workspace": "<name or empty>"}
// object instead of human-readable text — useful for the kai-desktop
// preload bridge and any other tool that wants to parse the result
// without guessing whether the help-text is a valid name.
var wsCurrentJSON bool

var wsCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show current workspace",
	Long: `Show the currently checked-out workspace.

This reads from .kai/workspace which is set by 'kai ws checkout'.`,
	RunE: runWsCurrent,
}

var integrateCmd = &cobra.Command{
	Use:   "integrate",
	Short: "Integrate workspace changes into a target snapshot",
	RunE:  runIntegrate,
}

var mergeCmd = &cobra.Command{
	Use:   "merge <base-file> <left-file> <right-file>",
	Short: "Perform AST-aware 3-way merge",
	Long: `Perform an AST-aware 3-way merge at symbol granularity.

Unlike line-based merge, this understands code structure and can:
- Auto-merge changes to different functions in the same file
- Detect API signature conflicts when both sides change function params
- Classify conflicts semantically (DELETE_vs_MODIFY, CONCURRENT_CREATE, etc.)

Examples:
  kai merge base.js left.js right.js --lang js
  kai merge base.py branch1.py branch2.py --lang py --output merged.py`,
	Args: cobra.ExactArgs(3),
	RunE: runMerge,
}

var checkoutCmd = &cobra.Command{
	Use:   "checkout <snapshot-id>",
	Short: "Restore filesystem to match a snapshot",
	Long: `Restore the filesystem to match a snapshot's state.

This writes all files from the snapshot to the target directory.
Use --clean to also delete files not in the snapshot.

Examples:
  kai checkout abc123... --dir ./src
  kai checkout abc123... --dir ./src --clean`,
	Args: cobra.ExactArgs(1),
	RunE: runCheckout,
}

var cherryPickCmd = &cobra.Command{
	Use:   "cherry-pick <changeset> <target-snapshot>",
	Short: "Apply a changeset onto a target snapshot",
	Long: `Apply a changeset onto a target snapshot and create a new changeset.

Examples:
  kai cherry-pick cs.login_fix snap.main
  kai cherry-pick @cs:last @snap:last`,
	Args: cobra.ExactArgs(2),
	RunE: runCherryPick,
}

var rebaseCmd = &cobra.Command{
	Use:   "rebase <target-snapshot> <changeset> [changeset...]",
	Short: "Reapply changesets onto a new base snapshot",
	Long: `Reapply one or more changesets onto a target snapshot in order.

Examples:
  kai rebase snap.main cs.a1b2 cs.c3d4
  kai rebase @snap:last @cs:prev @cs:last`,
	Args: cobra.MinimumNArgs(2),
	RunE: runRebase,
}

var bisectCmd = &cobra.Command{
	Use:   "bisect",
	Short: "Find a regression via binary search over snapshots",
}

var bisectStartCmd = &cobra.Command{
	Use:   "start <good-snapshot> <bad-snapshot>",
	Short: "Start a bisect session",
	Args:  cobra.ExactArgs(2),
	RunE:  runBisectStart,
}

var bisectGoodCmd = &cobra.Command{
	Use:   "good",
	Short: "Mark current snapshot as good",
	RunE:  runBisectGood,
}

var bisectBadCmd = &cobra.Command{
	Use:   "bad",
	Short: "Mark current snapshot as bad",
	RunE:  runBisectBad,
}

var bisectSkipCmd = &cobra.Command{
	Use:   "skip",
	Short: "Skip current snapshot",
	RunE:  runBisectSkip,
}

var bisectNextCmd = &cobra.Command{
	Use:   "next",
	Short: "Show the current snapshot to test",
	RunE:  runBisectNext,
}

var bisectResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Clear bisect state",
	RunE:  runBisectReset,
}

var shadowGitRange string
var shadowGitRepo string
var shadowGitRef string
var shadowSnapRef string
var shadowUpdateRef string

// shadow run flags
var shadowFullCmd string
var shadowKaiCmd string
var shadowRetries int
var shadowOutJSON string
var shadowOutMD string
var shadowSkipFullOnFail bool
var shadowResultFormat string

var shadowCmd = &cobra.Command{
	Use:   "shadow",
	Short: "Shadow Git in Kai (import/parity/drift)",
}

var shadowImportCmd = &cobra.Command{
	Use:   "import --git-range <base..head>",
	Short: "Import a Git range into Kai (snapshots + changeset)",
	Args:  cobra.NoArgs,
	RunE:  runShadowImport,
}

var shadowParityCmd = &cobra.Command{
	Use:   "parity --git-range <base..head>",
	Short: "Compare Git diff vs Kai snapshot diff",
	Args:  cobra.NoArgs,
	RunE:  runShadowParity,
}

var shadowDriftCmd = &cobra.Command{
	Use:   "drift --git-ref <ref> --snap <snap-ref>",
	Short: "Detect drift between Git ref and Kai snapshot",
	Args:  cobra.NoArgs,
	RunE:  runShadowDrift,
}

var shadowRunCmd = &cobra.Command{
	Use:   "run --git-range <base..head> --full <cmd> --kai <cmd>",
	Short: "Run selective vs full test suites and compare results",
	Long: `Run both selective (Kai-planned) and full test suites, then compare.

Answers: "If we had trusted Kai's plan, would we have missed a failure?"

The --kai command can include {{tests}} which will be replaced with the
space-joined list of test targets from the CI plan.

Examples:
  kai shadow run --git-range main..feature --full "npm test" --kai "npm test -- {{tests}}"
  kai shadow run --git-range HEAD~1..HEAD --full "go test ./..." --kai "go test {{tests}}" --format go`,
	Args: cobra.NoArgs,
	RunE: runShadowRun,
}

// Reference commands
var refCmd = &cobra.Command{
	Use:   "ref",
	Short: "Manage named references",
	Long: `Create and manage named references (aliases) for snapshots and changesets.

References allow you to use human-readable names instead of 64-character hex IDs.

Examples:
  kai ref set snap.main @snap:last
  kai ref set cs.login_fix 90cd7264
  kai ref list
  kai ref del cs.login_fix`,
}

var refListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all refs",
	RunE:  runRefList,
}

var refSetCmd = &cobra.Command{
	Use:   "set <name> <id|short|selector>",
	Short: "Create or update a ref",
	Long: `Create or update a named reference pointing to a node.

The target can be:
  - A full 64-char hex ID
  - A short hex prefix (8+ chars)
  - Another ref name
  - A selector (@snap:last, @cs:prev, etc.)

Examples:
  kai ref set snap.main d9ec9902
  kai ref set cs.bugfix @cs:last
  kai ref set snap.release @snap:prev`,
	Args: cobra.ExactArgs(2),
	RunE: runRefSet,
}

var refDelCmd = &cobra.Command{
	Use:   "del <name>",
	Short: "Delete a ref",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefDel,
}

// Tag commands
var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Manage tags (refs/tags/*)",
	Long: `Create and manage tag refs that point to snapshots.

Examples:
  kai tag create v1.0 @snap:last
  kai tag list
  kai tag delete v1.0`,
}

var tagListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tag refs",
	RunE:  runTagList,
}

var tagCreateCmd = &cobra.Command{
	Use:   "create <name> <target>",
	Short: "Create or update a tag ref",
	RunE:  runTagCreate,
	Args:  cobra.ExactArgs(2),
}

var tagDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a tag ref",
	RunE:  runTagDelete,
	Args:  cobra.ExactArgs(1),
}

// Modules commands
var modulesCmd = &cobra.Command{
	Use:   "modules",
	Short: "Manage module definitions",
	Long: `Define and manage modules for your codebase.

Modules group related files together, enabling:
- Semantic diffs at module level
- Targeted test selection based on module changes
- Import graph analysis between modules

Examples:
  kai modules init --infer --write   # Auto-detect and save modules
  kai modules add App src/app.js     # Add a module
  kai modules list                   # Show all modules
  kai modules preview                # Preview file-to-module mapping`,
}

var modulesInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize module configuration",
	Long: `Initialize module configuration by auto-detecting modules from your codebase.

With --infer, Kai scans your source directories and creates sensible module definitions.
With --write, the configuration is saved to .kai/rules/modules.yaml.

Examples:
  kai modules init --infer                    # Preview inferred modules
  kai modules init --infer --write            # Save inferred modules
  kai modules init --infer --by dirs          # Group by top-level directories
  kai modules init --infer --tests "tests/**" # Also detect test modules`,
	RunE: runModulesInit,
}

var modulesAddCmd = &cobra.Command{
	Use:   "add <name> <glob> [glob...]",
	Short: "Add or update a module",
	Long: `Add a new module or update an existing module's patterns.

Examples:
  kai modules add App src/app.js
  kai modules add Utils "src/utils/**"
  kai modules add Auth "src/auth/**" "src/middleware/auth*"`,
	Args: cobra.MinimumNArgs(2),
	RunE: runModulesAdd,
}

var modulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all modules",
	RunE:  runModulesList,
}

var modulesPreviewCmd = &cobra.Command{
	Use:   "preview [module]",
	Short: "Preview which files match each module",
	Long: `Show which files are matched by module patterns.

Without arguments, shows all modules and their matched files.
With a module name, shows only that module's matches.

Examples:
  kai modules preview
  kai modules preview Utils`,
	Args: cobra.MaximumNArgs(1),
	RunE: runModulesPreview,
}

var modulesShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show a module's configuration",
	Args:  cobra.ExactArgs(1),
	RunE:  runModulesShow,
}

var modulesRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Remove a module",
	Args:  cobra.ExactArgs(1),
	RunE:  runModulesRm,
}

var pickCmd = &cobra.Command{
	Use:   "pick <Snapshot|ChangeSet|Workspace>",
	Short: "Search and select a node interactively",
	Long: `Search for nodes and display matches for selection.

Use --filter to search by substring in ID, slug, or payload.
Use --no-ui to output matches without interactive selection.

Examples:
  kai pick Snapshot --filter auth
  kai pick ChangeSet --no-ui`,
	Args: cobra.ExactArgs(1),
	RunE: runPick,
}

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion scripts",
	Long: `Generate shell completion scripts for kai.

To load completions:

Bash:
  $ source <(kai completion bash)
  # To load completions for each session, add to your ~/.bashrc:
  # source <(kai completion bash)

Zsh:
  $ source <(kai completion zsh)
  # To load completions for each session, add to your ~/.zshrc:
  # source <(kai completion zsh)

Fish:
  $ kai completion fish | source
  # To load completions for each session:
  # kai completion fish > ~/.config/fish/completions/kai.fish

PowerShell:
  PS> kai completion powershell | Out-String | Invoke-Expression
`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE:                  runCompletion,
}

// Remote commands
var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage remote servers",
	Long: `Configure remote Kailab servers for pushing and fetching.

Examples:
  kai remote set origin http://localhost:7447
  kai remote get origin
  kai remote list`,
}

var remoteSetCmd = &cobra.Command{
	Use:   "set <name> <url>",
	Short: "Set a remote URL",
	Long: `Set a remote Kailab server URL with optional tenant and repo.

Examples:
  kai remote set origin http://localhost:7447
  kai remote set origin http://localhost:7447 --tenant myorg --repo main`,
	Args: cobra.ExactArgs(2),
	RunE: runRemoteSet,
}

var remoteGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Get a remote URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runRemoteGet,
}

var remoteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all remotes",
	RunE:  runRemoteList,
}

var remoteDelCmd = &cobra.Command{
	Use:   "del <name>",
	Short: "Delete a remote",
	Args:  cobra.ExactArgs(1),
	RunE:  runRemoteDel,
}

// ── Org management ─────────────────────────────────────────────────

var orgCmd = &cobra.Command{
	Use:   "org",
	Short: "Manage organizations on the remote server",
	Long: `Inspect and administer organizations on a remote Kailab server
(kaicontext.com by default). Requires an authenticated session
(run 'kai auth login' first).`,
}

var orgListCmd = &cobra.Command{
	Use:   "list",
	Short: "List organizations you belong to",
	RunE:  runOrgList,
}

var orgDeleteYes bool

var orgDeleteCmd = &cobra.Command{
	Use:   "delete <slug>",
	Short: "Delete an organization (hard delete, destructive)",
	Long: `Hard-deletes an organization, every repo inside it, and all
dependent data (snapshots, refs, CI runs, webhooks, secrets, variables,
billing, memberships). Irreversible.

Only the org owner can delete an org. The CLI requires you to type the
slug to confirm; --yes skips the prompt (for scripts).`,
	Args: cobra.ExactArgs(1),
	RunE: runOrgDelete,
}

var pushCmd = &cobra.Command{
	Use:   "push [remote] [target...]",
	Short: "Push snapshots, changesets, and reviews to a remote server",
	Long: `Push snapshots, changesets, and reviews to a remote Kailab server.

By default (no arguments), pushes all snapshots, changesets, and reviews.
You can also push specific targets using prefixes.

Targets can use prefixes:
  cs:<ref>      Push a changeset (+ its base/head snapshots)
  review:<id>   Push a review (+ its target changeset)
  snap:<ref>    Push a snapshot (advanced/plumbing)
  <ref>         Legacy: push a ref directly

Examples:
  kai push                         # Push all snapshots, changesets, and reviews
  kai push origin cs:login_fix     # Push single changeset
  kai push origin review:abc123    # Push a specific review
  kai push origin --ws feature/auth # Push specific workspace
  kai push --all                   # Push all refs (legacy)`,
	RunE: runPush,
}

var fetchCmd = &cobra.Command{
	Use:   "fetch [remote] [refs...]",
	Short: "Fetch refs and objects from a remote server",
	Long: `Fetch refs and objects from a remote Kailab server.

By default, fetches from the 'origin' remote.

Use --ws to fetch a specific workspace and recreate it locally.
Use --review to fetch a specific review and recreate it locally.

Examples:
  kai fetch                       # Fetch all refs from origin
  kai fetch origin                # Fetch all refs
  kai fetch origin snap.main      # Fetch specific ref
  kai fetch --ws feature/auth     # Fetch and recreate workspace
  kai fetch --review abc123       # Fetch and recreate review`,
	RunE: runFetch,
}

var pullCmd = &cobra.Command{
	Use:   "pull [remote]",
	Short: "Pull latest snapshot from remote and update local state",
	Long: `Pull the latest snapshot from the remote server, fetch all file content,
and update the local snap.latest ref to match.

This is the inverse of 'kai push'. It fetches the remote's snap.latest,
downloads all file nodes and content blobs, stores them locally, and
updates your local refs.

Examples:
  kai pull              # Pull from origin
  kai pull upstream     # Pull from a named remote`,
	RunE: runPull,
}

var cloneCmd = &cobra.Command{
	Use:   "clone <org/repo | url> [directory]",
	Short: "Clone a repository from a remote server",
	Long: `Clone a Kai repository from a remote Kailab server.

Creates a new directory, initializes Kai, sets up the remote, and fetches all refs.

URL formats:
  org/repo                         Shorthand (uses default server: kaicontext.com)
  http://server/tenant/repo        Full URL with server

The default server can be overridden with the KAI_SERVER environment variable.

Examples:
  kai clone 1m/myrepo                                   # Clone from kaicontext.com
  kai clone 1m/myrepo myproject                         # Clone into 'myproject' directory
  kai clone https://kaicontext.com/myorg/myrepo             # Full URL
  kai clone http://localhost:8080/myorg/myrepo          # Local development
  kai clone http://localhost:8080 --tenant myorg --repo myrepo`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runClone,
}

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Garbage-collect unreachable snapshots/changesets/files",
	Long: `Garbage-collect unreachable content using mark-and-sweep.

Roots (kept):
  - All ref targets
  - All workspace nodes (and their base/head/changesets)

Everything not reachable from roots is swept.

Examples:
  kai prune --dry-run           # Preview what would be deleted
  kai prune                     # Actually delete unreachable content
  kai prune --since 7           # Only delete content older than 7 days
  kai prune --aggressive        # Also sweep orphaned Symbols/Modules`,
	RunE: runPrune,
}

var purgeCmd = &cobra.Command{
	Use:   "purge <path-or-glob> [...]",
	Short: "Remove a file from all snapshots in history",
	Long: `Permanently remove files from every snapshot that contains them.

This is the escape hatch from immutability — use it to scrub leaked
secrets, credentials, or large files from kai history. Like git
filter-branch / BFG, but for the semantic graph.

Immutability is preserved by default. This command explicitly breaks
it for the targeted files only. Snapshot nodes remain valid for
navigation, but the purged file content is gone.

Supports exact paths and glob patterns (including **).

Examples:
  kai purge .env                    # Preview what would be purged
  kai purge .env --yes              # Actually purge
  kai purge "**/*.pem" --yes        # Purge all .pem files from history
  kai purge src/config/secrets.ts   # Preview purging a specific file`,
	Args: cobra.MinimumNArgs(1),
	RunE: runPurge2,
}

var remoteLogCmd = &cobra.Command{
	Use:   "remote-log [remote]",
	Short: "Show remote ref history log",
	Long: `Display the append-only ref history from a remote Kailab server.

Examples:
  kai remote-log                  # Show log from origin
  kai remote-log origin -n 20    # Show 20 entries
  kai remote-log --ref snap.main # Filter by ref`,
	RunE: runRemoteLog,
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update kai to the latest version",
	Long: `Download and install the latest kai binary from GitHub releases.

Examples:
  kai update           # Download and install latest version
  kai update --check   # Check for updates without installing`,
	RunE: runUpdate,
}

// Auth commands
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
	Long: `Manage authentication with Kailab servers and inspect the
configured LLM provider.

Examples:
  kai auth login                  # Interactive login (kailab)
  kai auth logout                 # Clear kailab credentials
  kai auth status                 # Show LLM provider + kailab status

LLM provider environment variables (used by the kai TUI):
  KAI_PROVIDER          kailab (default), anthropic, openai
                        Aliases:
                          openai-compatible / oai-compat / local → openai
                          anthropic-direct / claude              → anthropic
  ANTHROPIC_API_KEY     required when KAI_PROVIDER=anthropic
  OPENAI_API_KEY        used when KAI_PROVIDER=openai (optional for local)
  KAI_OPENAI_BASE_URL   OpenAI-compatible endpoint URL
  KAI_OPENAI_MODEL      override default model on OpenAI
  KAI_ANTHROPIC_MODEL   override default model on Anthropic
  KAI_OPENAI_TOOL_FORMAT  raw (default), hermes, llama3
  KAI_MAX_SESSION_COST_USD  pause and prompt before exceeding cap`,
}

var authLoginCmd = &cobra.Command{
	Use:   "login [server-url]",
	Short: "Login to a Kailab server",
	Long: `Authenticate with a Kailab control plane server.

If no server URL is provided, uses the origin remote's URL.

Examples:
  kai auth login                              # Interactive login using origin remote
  kai auth login http://localhost:8080        # Interactive login to specific server
  kai auth login --token "$KAI_TOKEN"         # Non-interactive login for CI`,
	RunE: runAuthLogin,
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Logout and clear credentials",
	RunE:  runAuthLogout,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current authentication status",
	RunE:  runAuthStatus,
}

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show billing usage for the current period",
	Long: `Show commit usage and billing information for your organization.

Examples:
  kai usage                  # Show usage for current org`,
	RunE: runUsage,
}

var liveCmd = &cobra.Command{
	Use:   "live",
	Short: "Query the live sync state (agents pushing through sync_events)",
	Long: `Live sync commands surface the append-only sync_events log that
tracks every live-sync push from every agent. This is the state between
the last 'kai capture' and what's actually on everyone's disk right now.

Examples:
  kai live status             # How far ahead of the last snapshot are we?
  kai live status --since 42  # Only count events after seq 42`,
}

var liveOnCmd = &cobra.Command{
	Use:   "on",
	Short: "Enable live sync for this workspace",
	Long: `Mark live sync as enabled for the current repo by writing
.kai/sync-state.json. The MCP server picks this up on startup and
auto-subscribes to the sync channel; pass --files to scope the
subscription to specific files.

This is the CLI sibling of the kai_live_sync MCP tool. The CLI just
writes the intent file — for changes to take effect in an already-
running MCP server, restart the agent session.

Examples:
  kai live on
  kai live on --files src/auth.go,src/db.go`,
	RunE: runLiveOn,
}

var liveOffCmd = &cobra.Command{
	Use:   "off",
	Short: "Disable live sync for this workspace",
	Long:  `Remove .kai/sync-state.json so the next MCP server startup does not auto-subscribe.`,
	RunE:  runLiveOff,
}

// liveSyncState mirrors internal/mcp.persistedSyncState; kept private
// here so cmd/kai doesn't have to depend on internal/mcp.
type liveSyncState struct {
	Enabled bool     `json:"enabled"`
	Files   []string `json:"files,omitempty"`
	LastSeq int64    `json:"last_seq,omitempty"`
}

func runLiveOn(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(kaiDir); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}
	filesStr, _ := cmd.Flags().GetString("files")
	var files []string
	for _, f := range strings.Split(filesStr, ",") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	// Preserve LastSeq if there's already a state file (matches the
	// behavior of internal/mcp.saveSyncState).
	var lastSeq int64
	if data, err := os.ReadFile(filepath.Join(kaiDir, "sync-state.json")); err == nil {
		var prev liveSyncState
		if json.Unmarshal(data, &prev) == nil {
			lastSeq = prev.LastSeq
		}
	}
	st := liveSyncState{Enabled: true, Files: files, LastSeq: lastSeq}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(kaiDir, "sync-state.json"), data, 0644); err != nil {
		return fmt.Errorf("writing sync-state: %w", err)
	}
	// Clear the auto-sync opt-out so `kai ws checkout` resumes starting the
	// default-on daemon for this repo.
	os.Remove(autoSyncOffPath(kaiDir))
	if len(files) > 0 {
		fmt.Printf("Live sync enabled (%d files watched).\n", len(files))
	} else {
		fmt.Println("Live sync enabled.")
	}
	return nil
}

func runLiveOff(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat(kaiDir); err != nil {
		return fmt.Errorf("not in a kai repo: run `kai init` first")
	}
	path := filepath.Join(kaiDir, "sync-state.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing sync-state: %w", err)
	}
	// Stop any running auto-sync daemon and set a persistent opt-out so
	// `kai ws checkout` won't restart it (keeps this workspace private).
	stopAutoSync(kaiDir)
	if err := os.WriteFile(autoSyncOffPath(kaiDir), []byte("1\n"), 0644); err != nil {
		return fmt.Errorf("writing opt-out: %w", err)
	}
	fmt.Println("Live sync disabled (auto-sync off for this repo; `kai live on` to re-enable).")
	return nil
}

var liveStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show uncompacted sync state summary",
	Long: `Print a rollup of the live sync state for the current repo:
- tip_seq: latest sync_events sequence
- event_count / file_count / agent_count since the given seq (default 0)
- snap_latest: the current snap.latest ref target (what the last full
  snapshot was pinned to)
- files: the files touched in the uncompacted window

Reads the remote sync_events log via GET /v1/sync/live. Does not
transfer file content, only metadata — safe to run repeatedly.`,
	RunE: runLiveStatus,
}

func runLiveStatus(cmd *cobra.Command, args []string) error {
	sinceFlag, _ := cmd.Flags().GetInt64("since")
	limitFlag, _ := cmd.Flags().GetInt("limit")
	noFiles, _ := cmd.Flags().GetBool("no-files")

	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		return fmt.Errorf("loading remote: %w", err)
	}

	// Scope status to the current workspace (empty = repo-wide), matching how
	// sync events are scoped on the server.
	curWs, _ := getCurrentWorkspace()
	summary, err := client.GetSyncLive(sinceFlag, !noFiles, curWs, limitFlag)
	if err != nil {
		return err
	}

	fmt.Printf("Repo:          %s\n", client.RepoPath())
	fmt.Printf("Remote:        %s\n", client.BaseURL)
	fmt.Printf("Tip seq:       %d\n", summary.TipSeq)
	fmt.Printf("Since seq:     %d\n", summary.SinceSeq)
	if summary.SnapLatest != "" {
		fmt.Printf("Last snapshot: %s\n", summary.SnapLatest)
	} else {
		fmt.Printf("Last snapshot: (none)\n")
	}
	fmt.Printf("Events:        %d\n", summary.EventCount)
	fmt.Printf("Files:         %d\n", summary.FileCount)
	fmt.Printf("Agents:        %d\n", summary.AgentCount)
	if summary.EventCount > 0 {
		firstTs := time.UnixMilli(summary.FirstEventTs).Format(time.RFC3339)
		lastTs := time.UnixMilli(summary.LastEventTs).Format(time.RFC3339)
		fmt.Printf("First event:   %s\n", firstTs)
		fmt.Printf("Last event:    %s\n", lastTs)
	}
	if len(summary.Files) > 0 {
		fmt.Printf("\nFiles touched (most-recently-pushed first):\n")
		for _, f := range summary.Files {
			fmt.Printf("  %s\n", f)
		}
	}
	return nil
}

// Review commands
var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Manage code reviews for changesets",
	Long: `Create and manage code reviews for changesets or workspaces.

Reviews are anchored to semantic entities (changesets, symbols) not lines.

Examples:
  kai review open @cs:last --title "Add auth"    # Open a review
  kai review list                                 # List all reviews
  kai review view <id>                            # View a review
  kai review approve <id>                         # Approve a review
  kai review close <id> --state merged            # Close as merged`,
}

var reviewOpenCmd = &cobra.Command{
	Use:   "open [changeset|workspace]",
	Short: "Open a new review",
	Long: `Open a new code review for a changeset or workspace.

With no arguments, automatically creates a changeset from your last two snapshots
(@snap:prev → @snap:last) and opens a review for it.

The title is auto-generated from semantic analysis if not provided (like git commit).

Examples:
  kai review open                                      # Auto-title from changes
  kai review open -m "Fix login bug"                   # Explicit title
  kai review open @cs:last --title "Reduce timeout"    # Explicit changeset`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runReviewOpen,
}

var reviewListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all reviews",
	RunE:  runReviewList,
}

var reviewViewCmd = &cobra.Command{
	Use:   "view <review-id>",
	Short: "View a review",
	Long: `View details of a review including its changeset, status, and comments.

Examples:
  kai review view abc123
  kai review view abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewView,
}

var reviewStatusCmd = &cobra.Command{
	Use:   "status <review-id>",
	Short: "Show review status",
	Args:  cobra.ExactArgs(1),
	RunE:  runReviewStatus,
}

var reviewApproveCmd = &cobra.Command{
	Use:   "approve <review-id>",
	Short: "Approve a review",
	Args:  cobra.ExactArgs(1),
	RunE:  runReviewApprove,
}

var reviewRequestChangesCmd = &cobra.Command{
	Use:   "request-changes <review-id>",
	Short: "Request changes on a review",
	Args:  cobra.ExactArgs(1),
	RunE:  runReviewRequestChanges,
}

var reviewCloseCmd = &cobra.Command{
	Use:   "close <review-id>",
	Short: "Close a review",
	Long: `Close a review with a final state.

Examples:
  kai review close abc123 --state merged
  kai review close abc123 --state abandoned`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewClose,
}

var reviewEditCmd = &cobra.Command{
	Use:   "edit <review-id>",
	Short: "Update review title, description, or assignees",
	Long: `Update review metadata after creation.

Examples:
  kai review edit abc123 --title "New title"
  kai review edit abc123 --desc "Updated description"
  kai review edit abc123 --assignees alice --assignees bob`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewEdit,
}

var reviewReadyCmd = &cobra.Command{
	Use:   "ready <review-id>",
	Short: "Mark a draft review as ready for review",
	Args:  cobra.ExactArgs(1),
	RunE:  runReviewReady,
}

var reviewExportCmd = &cobra.Command{
	Use:   "export <review-id>",
	Short: "Export review as markdown or HTML",
	Long: `Export a review summary for posting to GitHub/GitLab PRs.

Examples:
  kai review export abc123 --markdown > review.md
  kai review export abc123 --html > review.html`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewExport,
}

var reviewCommentCmd = &cobra.Command{
	Use:   "comment <review-id>",
	Short: "Add a comment to a review",
	Long: `Add a comment to a review, optionally anchored to a file:line.

Examples:
  kai review comment abc123 -m "Looks good"
  kai review comment abc123 -m "Check nil case" --file auth.go --line 42`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewComment,
}

var reviewCommentsCmd = &cobra.Command{
	Use:   "comments <review-id>",
	Short: "List comments on a review",
	Long: `List all comments on a review.

Examples:
  kai review comments abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewComments,
}

var reviewSummaryCmd = &cobra.Command{
	Use:   "summary [changeset]",
	Short: "Show semantic summary of a changeset",
	Long: `Display a progressive disclosure summary of what changed in a changeset.

Level 1: WHAT CHANGED - grouped by category (API, internal, tests, etc.)
Level 2: WHAT IT AFFECTS - files and symbols modified
Level 3: WHAT COULD BE FIXED - AI suggestions (use --ai flag)
Level 4: INSPECT - drill down into specific changes

Examples:
  kai review summary                    # Summary of @cs:last
  kai review summary @cs:abc123         # Summary of specific changeset
  kai review summary @cs:last -i        # Interactive drill-down mode
  kai review summary --ai               # Include AI review suggestions`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runReviewSummary,
}

var (
	// Workspace flags
	wsName           string
	wsBase           string
	wsFromDir        string
	wsFromGit        string
	wsDescription    string
	wsDir            string
	wsTarget         string
	wsDeleteKeepRefs bool
	wsDeleteDryRun   bool
	wsCheckoutClean  bool
	pruneDryRun      bool
	pruneSinceDays   int
	pruneAggressive  bool
	pruneYes         bool
	pruneKeep        []string

	// Purge flags
	purgeDryRun bool
	purgeYes    bool

	// Review flags
	reviewTitle         string
	reviewDesc          string
	reviewReviewers     []string
	reviewCloseState    string
	reviewExportMD      bool
	reviewExportHTML    bool
	reviewJSON          bool
	reviewViewMode      string
	reviewExplain       bool
	reviewBase          string
	reviewSummary       bool // Show progressive disclosure summary
	reviewInteractive   bool // Interactive drill-down mode
	reviewAI            bool // Run AI review
	reviewCommentBody   string
	reviewCommentFile   string
	reviewCommentLine   int
	reviewEditTitle     string
	reviewEditDesc      string
	reviewEditAssignees []string

	statusDir        string
	statusAgainst    string
	statusNameOnly   bool
	statusJSON       bool
	statusSemantic   bool
	statusExplain    bool
	logLimit         int
	logOneline       bool
	logAuthor        string
	logGrep          string
	logSince         string
	logUntil         string
	logStat          bool
	logJSON          bool
	repoPath         string
	dirPath          string
	editText         string
	regenerateIntent bool
	showAlternatives bool
	intentConfidence float64
	explainIntent    bool
	jsonFlag         bool
	checkoutDir      string
	checkoutClean    bool

	// Ref/pick flags
	refKindFilter string
	pickFilter    string
	pickNoUI      bool

	// Changeset flags
	changesetMessage string
	changesetGitBase string // git ref for base snapshot
	changesetGitHead string // git ref for head snapshot
	changesetGitRepo string // git repo path
	wsStageMessage   string
	wsSignKey        string

	// Diff flags
	diffDir      string
	diffNameOnly bool
	diffSemantic bool
	diffJSON     bool
	diffExplain  bool
	diffPatch    bool // git-style line-level diff
	diffStat     bool // git diff --stat style summary
	diffForce    bool // skip stale baseline warning

	// Snapshot flags
	snapshotMessage string
	snapshotGitRef  string // explicit git ref for disambiguation

	// Capture flags
	captureExplain bool
	captureMessage string
	captureAll     bool

	// Global explain flag
	explainFlag bool

	// Push/fetch flags
	pushForce      bool
	pushAll        bool
	pushWorkspace  string
	pushDryRun     bool
	pushExplain    bool
	remoteLogRef   string
	remoteLogLimit int

	// Remote set flags
	remoteTenant string
	remoteRepo   string

	// Clone flags
	cloneTenant  string
	cloneRepo    string
	cloneKaiOnly bool

	// Pull flags
	pullForce bool

	// Fetch flags
	fetchWorkspace string
	fetchReview    string
	fetchExplain   bool

	// Merge flags
	mergeLang   string
	mergeOutput string
	mergeJSON   bool

	// Modules flags
	modulesInfer     bool
	modulesWrite     bool
	modulesBy        string
	modulesTestsGlob string
	modulesDryRun    bool
)

var telemetryCmd = &cobra.Command{
	Use:   "telemetry",
	Short: "Manage anonymous usage telemetry",
}

var telemetryEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable anonymous usage telemetry",
	RunE:  runTelemetryEnable,
}

var telemetryDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable anonymous usage telemetry",
	RunE:  runTelemetryDisable,
}

var telemetryStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show telemetry status",
	RunE:  runTelemetryStatus,
}

var telemetryFlushCmd = &cobra.Command{
	Use:   "flush",
	Short: "Force-upload any spooled telemetry events (bypasses the 24h rate limit)",
	RunE:  runTelemetryFlush,
}

func runTelemetryEnable(cmd *cobra.Command, args []string) error {
	if err := telemetry.Enable(); err != nil {
		return fmt.Errorf("enabling telemetry: %w", err)
	}
	cfg, _ := telemetry.LoadConfig()
	fmt.Println("Telemetry enabled.")
	fmt.Printf("  Install ID: %s\n", cfg.InstallID)
	fmt.Println("  No sensitive data is collected (no paths, code, usernames, or repo names).")
	fmt.Println("  Disable anytime with: kai telemetry disable")
	return nil
}

func runTelemetryDisable(cmd *cobra.Command, args []string) error {
	if err := telemetry.Disable(); err != nil {
		return fmt.Errorf("disabling telemetry: %w", err)
	}
	fmt.Println("Telemetry disabled.")
	return nil
}

func runTelemetryFlush(cmd *cobra.Command, args []string) error {
	if !telemetry.IsEnabled() {
		fmt.Println("Telemetry is disabled. Run 'kai telemetry enable' first.")
		return nil
	}
	fmt.Println("Flushing pending events to PostHog...")
	if err := telemetry.FlushNow(); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	fmt.Println("Done.")
	return nil
}

func runTelemetryStatus(cmd *cobra.Command, args []string) error {
	cfg, err := telemetry.LoadConfig()
	if err != nil {
		return err
	}
	// "No config file yet" == default-on new install. Show that explicitly
	// so users can tell "never decided, using default" from "explicitly enabled".
	configExists := true
	if _, statErr := os.Stat(telemetry.ConfigPath()); os.IsNotExist(statErr) {
		configExists = false
	}
	switch {
	case !configExists:
		fmt.Println("Telemetry: enabled (default — opt-out)")
	case cfg.Enabled:
		fmt.Println("Telemetry: enabled")
	default:
		fmt.Println("Telemetry: disabled")
	}
	if cfg.InstallID != "" {
		fmt.Printf("  Install ID:    %s\n", cfg.InstallID)
	}
	if cfg.Level != "" {
		fmt.Printf("  Level:         %s\n", cfg.Level)
	}
	if cfg.CreatedAt != "" {
		fmt.Printf("  Created:       %s\n", cfg.CreatedAt)
	}
	fmt.Printf("  Config:        %s\n", telemetry.ConfigPath())
	fmt.Printf("  Destination:   PostHog (us.i.posthog.com)\n")

	// Show effective state considering env overrides
	effective := telemetry.IsEnabled()
	configuredOn := configExists && cfg.Enabled
	configuredOn = configuredOn || !configExists // default-on counts as "configured on"
	if effective != configuredOn {
		if effective {
			fmt.Println("  (overridden to ENABLED by KAI_TELEMETRY=1)")
		} else {
			fmt.Println("  (overridden to DISABLED by environment)")
		}
	}
	return nil
}

var (
	versionShort bool
	versionJSON  bool
)

// versionSemverRE captures the leading semver base, optional pre-release
// identifier, and optional build metadata. Anything that fails to match
// is treated as opaque (no parsing, --short echoes input unchanged).
var versionSemverRE = regexp.MustCompile(`^(\d+\.\d+\.\d+)(?:-([^+]+))?(?:\+(.+))?$`)

func parseVersion(v string) (base, pre, build string) {
	m := versionSemverRE.FindStringSubmatch(v)
	if m == nil {
		return v, "", ""
	}
	return m[1], m[2], m[3]
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the kai version",
	Run: func(cmd *cobra.Command, args []string) {
		if versionJSON {
			base, pre, build := parseVersion(Version)
			out, _ := json.Marshal(struct {
				Version string `json:"version"`
				Build   string `json:"build"`
				Commit  string `json:"commit"`
			}{Version: base, Build: pre, Commit: build})
			fmt.Println(string(out))
			return
		}
		if versionShort {
			base, _, _ := parseVersion(Version)
			fmt.Println(base)
			return
		}
		fmt.Println("kai " + Version)
	},
}

// configShowJSON controls whether runConfigShow outputs JSON instead of YAML.
var configShowJSON bool

// configShowQuiet controls whether runConfigShow suppresses the runtime block.
var configShowQuiet bool

var configCmd = &cobra.Command{Use: "config", Short: "Config commands"}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print resolved configuration",
	Long:  "Print the resolved kai configuration including runtime environment details such as provider, model override, and API key source.",
	RunE:  runConfigShow,
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		return err
	}
	type resolved struct {
		Config  config.Config `yaml:"config" json:"config"`
		Runtime struct {
			Provider      string `yaml:"provider" json:"provider"`
			ModelOverride string `yaml:"model_override,omitempty" json:"model_override,omitempty"`
			APIKeySource  string `yaml:"api_key_source" json:"api_key_source"`
			KaiDir        string `yaml:"kai_dir" json:"kai_dir"`
		} `yaml:"runtime" json:"runtime"`
	}
	var out resolved
	out.Config = cfg
	out.Runtime.KaiDir = kaiDir
	prov := os.Getenv("KAI_PROVIDER")
	if prov == "" {
		prov = "kailab"
	}
	out.Runtime.Provider = prov
	switch prov {
	case "anthropic", "anthropic-direct", "claude":
		out.Runtime.ModelOverride = os.Getenv("KAI_ANTHROPIC_MODEL")
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			out.Runtime.APIKeySource = "ANTHROPIC_API_KEY (env)"
		} else {
			out.Runtime.APIKeySource = "(not set)"
		}
	case "openai", "openai-compatible", "oai-compat", "local":
		out.Runtime.ModelOverride = os.Getenv("KAI_OPENAI_MODEL")
		if os.Getenv("OPENAI_API_KEY") != "" {
			out.Runtime.APIKeySource = "OPENAI_API_KEY (env)"
		} else {
			out.Runtime.APIKeySource = "(not set — local endpoint assumed)"
		}
	default: // kailab
		out.Runtime.APIKeySource = "kailab token (~/.kai/token)"
	}
	if configShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if configShowQuiet {
			return enc.Encode(out.Config)
		}
		return enc.Encode(out)
	}
	if configShowQuiet {
		b, err := yaml.Marshal(map[string]interface{}{"config": out.Config})
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		return err
	}
	os.Stdout.Write(b)
	return nil
}

// bench command — run a task with and without Kai MCP and compare token usage
var benchTask string
var benchModel string

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Benchmark token savings: run a task with and without Kai",
	Long: `Run a coding task twice using Claude Code — once without Kai's semantic
graph and once with it — then compare token usage and cost.

Requires the 'claude' CLI to be installed and authenticated.

Examples:
  kai bench --task "find where authentication is handled"
  kai bench --task "what tests cover the payment module" --model sonnet
  kai bench --task "explain the data flow in the API layer"`,
	RunE: runBench,
}

// claudeResult holds the parsed JSON output from claude -p --output-format json
type claudeResult struct {
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
	DurationMS    int    `json:"duration_ms"`
	DurationAPIMS int    `json:"duration_api_ms"`
	NumTurns      int    `json:"num_turns"`
	Result        string `json:"result"`
	IsError       bool   `json:"is_error"`
}

func (r claudeResult) totalTokens() int {
	return r.Usage.InputTokens + r.Usage.CacheCreationInputTokens +
		r.Usage.CacheReadInputTokens + r.Usage.OutputTokens
}

func runBench(cmd *cobra.Command, args []string) error {
	if benchTask == "" {
		return fmt.Errorf("--task is required")
	}

	// Check that claude CLI is available
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("claude CLI not found in PATH — install it from https://claude.ai/code")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Ensure Kai is initialized so the MCP server has a graph to serve
	kaiPath := kaipath.Resolve(cwd)
	if _, err := os.Stat(kaiPath); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "No kai data directory found — running 'kai capture' first...")
		captureCmd := exec.Command("kai", "capture", ".")
		captureCmd.Dir = cwd
		captureCmd.Stdout = os.Stderr
		captureCmd.Stderr = os.Stderr
		if err := captureCmd.Run(); err != nil {
			return fmt.Errorf("kai capture failed: %w", err)
		}
		fmt.Fprintln(os.Stderr)
	}

	modelFlag := ""
	if benchModel != "" {
		modelFlag = benchModel
	}

	fmt.Fprintf(os.Stderr, "Task: %s\n", benchTask)
	fmt.Fprintf(os.Stderr, "Repo: %s\n\n", cwd)

	// --- Warm caches ---
	// The Anthropic API caches system prompts + tool definitions with a ~5 min TTL.
	// Without warming, the first run pays expensive cache-write costs while the
	// second benefits from cache reads — skewing the comparison. We send a trivial
	// prompt for each mode first so that both measured runs hit warm caches.
	fmt.Fprintf(os.Stderr, "Warming caches...\n")
	warmupPrompt := "say ok"
	if _, err := runClaude(cwd, warmupPrompt, modelFlag, benchModeNoKai); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: warm-up without Kai failed: %v\n", err)
	}
	if _, err := runClaude(cwd, warmupPrompt, modelFlag, benchModeKaiPrime); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: warm-up with Kai failed: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "  Done\n\n")

	// --- Run 1: Without Kai ---
	fmt.Fprintf(os.Stderr, "Running without Kai (--strict-mcp-config with no servers)...\n")
	withoutResult, err := runClaude(cwd, benchTask, modelFlag, benchModeNoKai)
	if err != nil {
		return fmt.Errorf("run without Kai failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Done (%s, $%.4f)\n\n",
		formatDuration(float64(withoutResult.DurationMS)/1000), withoutResult.TotalCostUSD)

	// --- Run 2: With Kai (pre-injected context, no MCP overhead) ---
	fmt.Fprintf(os.Stderr, "Running with Kai (pre-injected context, no MCP)...\n")
	withResult, err := runClaude(cwd, benchTask, modelFlag, benchModeKaiPrime)
	if err != nil {
		return fmt.Errorf("run with Kai failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Done (%s, $%.4f)\n\n",
		formatDuration(float64(withResult.DurationMS)/1000), withResult.TotalCostUSD)

	// --- Display results ---
	withoutTokens := withoutResult.totalTokens()
	withTokens := withResult.totalTokens()

	costWithout := withoutResult.TotalCostUSD
	costWith := withResult.TotalCostUSD
	costSaved := costWithout - costWith
	var costPct float64
	if costWithout > 0 {
		costPct = costSaved / costWithout * 100
	}

	// Token breakdown
	fmt.Println()
	fmt.Println("Token breakdown:")
	fmt.Printf("  %-14s %10s  %10s  %10s  %10s  %10s\n", "", "Input", "Output", "Cache Write", "Cache Read", "Total")
	fmt.Printf("  %-14s %10s  %10s  %10s  %10s  %10s\n", "Without Kai",
		formatNumber(withoutResult.Usage.InputTokens),
		formatNumber(withoutResult.Usage.OutputTokens),
		formatNumber(withoutResult.Usage.CacheCreationInputTokens),
		formatNumber(withoutResult.Usage.CacheReadInputTokens),
		formatNumber(withoutTokens))
	fmt.Printf("  %-14s %10s  %10s  %10s  %10s  %10s\n", "With Kai",
		formatNumber(withResult.Usage.InputTokens),
		formatNumber(withResult.Usage.OutputTokens),
		formatNumber(withResult.Usage.CacheCreationInputTokens),
		formatNumber(withResult.Usage.CacheReadInputTokens),
		formatNumber(withTokens))
	fmt.Println()

	// Cost comparison (the authoritative metric)
	fmt.Println("Cost:")
	fmt.Printf("  Without Kai:  $%.4f\n", costWithout)
	fmt.Printf("  With Kai:     $%.4f\n", costWith)
	if costSaved > 0 {
		fmt.Printf("  Saved:        $%.4f (%.1f%%)\n", costSaved, costPct)
	} else if costSaved < 0 {
		fmt.Printf("  Diff:        +$%.4f (%.1f%% more with Kai)\n", -costSaved, -costPct)
	} else {
		fmt.Printf("  Saved:        $0.0000 (0.0%%)\n")
	}

	fmt.Println()
	fmt.Println("Turns:")
	fmt.Printf("  Without Kai:  %d\n", withoutResult.NumTurns)
	fmt.Printf("  With Kai:     %d\n", withResult.NumTurns)

	if withoutResult.DurationMS > 0 && withResult.DurationMS > 0 {
		speedup := float64(withoutResult.DurationMS) / float64(withResult.DurationMS)
		fmt.Println()
		if speedup >= 1 {
			fmt.Printf("Speed:          %.1fx faster with Kai\n", speedup)
		} else {
			fmt.Printf("Speed:          %.1fx slower with Kai\n", 1/speedup)
		}
	}

	// Also output JSON to stdout for scripting
	resultJSON := map[string]interface{}{
		"task": benchTask,
		"without_kai": map[string]interface{}{
			"total_tokens":                withoutTokens,
			"input_tokens":                withoutResult.Usage.InputTokens,
			"output_tokens":               withoutResult.Usage.OutputTokens,
			"cache_creation_input_tokens": withoutResult.Usage.CacheCreationInputTokens,
			"cache_read_input_tokens":     withoutResult.Usage.CacheReadInputTokens,
			"cost_usd":                    costWithout,
			"duration_ms":                 withoutResult.DurationMS,
			"num_turns":                   withoutResult.NumTurns,
		},
		"with_kai": map[string]interface{}{
			"total_tokens":                withTokens,
			"input_tokens":                withResult.Usage.InputTokens,
			"output_tokens":               withResult.Usage.OutputTokens,
			"cache_creation_input_tokens": withResult.Usage.CacheCreationInputTokens,
			"cache_read_input_tokens":     withResult.Usage.CacheReadInputTokens,
			"cost_usd":                    costWith,
			"duration_ms":                 withResult.DurationMS,
			"num_turns":                   withResult.NumTurns,
		},
		"savings": map[string]interface{}{
			"cost_saved":         costSaved,
			"cost_percent_saved": costPct,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(resultJSON)
}

// runClaude executes claude -p with the given task, returns parsed result.
// If withKai is false, it disables all MCP servers via --strict-mcp-config with an empty config.
// If withKai is true, it runs normally (Kai MCP available via user's config).
// benchMode controls how Claude is invoked in the benchmark.
type benchMode int

const (
	benchModeNoKai    benchMode = iota // no MCP servers at all
	benchModeKaiMCP                    // normal Kai MCP available (reserved for direct comparison)
	benchModeKaiPrime                  // prime context prepended, no MCP servers
)

func runClaude(cwd, task, model string, mode benchMode) (claudeResult, error) {
	prompt := task

	// In prime mode, run kai prime to get context and prepend it to the prompt
	if mode == benchModeKaiPrime {
		// Use the same binary that's running (os.Args[0]) rather than relying on PATH
		self, _ := os.Executable()
		if self == "" {
			self = os.Args[0]
		}
		primeCmd := exec.Command(self, "prime", task)
		primeCmd.Dir = cwd
		primeOut, err := primeCmd.Output()
		if err != nil {
			return claudeResult{}, fmt.Errorf("kai prime failed: %w", err)
		}
		if len(primeOut) > 0 {
			prompt = string(primeOut) + "\n---\n\nTask: " + task
		}
	}

	args := []string{"-p", prompt, "--output-format", "json"}

	if model != "" {
		args = append(args, "--model", model)
	}

	if mode != benchModeKaiMCP {
		// Disable all MCP servers for both noKai and prime modes
		tmpFile, err := os.CreateTemp("", "kai-bench-mcp-*.json")
		if err != nil {
			return claudeResult{}, fmt.Errorf("creating temp MCP config: %w", err)
		}
		defer os.Remove(tmpFile.Name())
		if _, err := tmpFile.WriteString(`{"mcpServers":{}}`); err != nil {
			tmpFile.Close()
			return claudeResult{}, fmt.Errorf("writing temp MCP config: %w", err)
		}
		tmpFile.Close()
		args = append(args, "--strict-mcp-config", "--mcp-config", tmpFile.Name())
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		// claude may exit non-zero but still produce valid JSON
		if len(out) == 0 {
			return claudeResult{}, fmt.Errorf("claude exited with error: %w", err)
		}
	}

	var result claudeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return claudeResult{}, fmt.Errorf("failed to parse claude output: %w\nraw: %s", err, string(out[:min(len(out), 500)]))
	}

	return result, nil
}

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage git hooks for automatic capture",
}

var hookInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install git hooks that update the Kai graph on commit and push",
	Long: `Installs git pre-commit and pre-push hooks that update the Kai
semantic graph automatically.

The hooks are best-effort: they will NEVER block git. If kai is missing,
.kai is gone, or capture/push fails for any reason, the hook silently
no-ops and git proceeds normally.

Re-running this command upgrades existing kai-managed hooks in place.`,
	RunE: runHookInstall,
}

var hookUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the kai pre-commit hook",
	RunE:  runHookUninstall,
}

var doctorFix bool
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Audit local Kai state and offer fixes",
	Long: `Checks the health of your local Kai installation:

  - kai binary is on PATH
  - .kai directory exists in the current repo
  - git hooks (if installed) are at the current safe version
  - kaicontext.com auth is configured
  - origin remote is configured

Run 'kai doctor --fix' to apply automatic repairs (currently: upgrade
out-of-date kai-managed git hooks).`,
	RunE: runDoctor,
}

const kaiHookMarker = "# kai-managed-hook"
const kaiHookVersion = "v5"

// All hook scripts early-exit when KAI_BRIDGE_INPROGRESS=1. Set by kai itself
// when it is the one driving the git operation (e.g. milestone→commit bridge),
// so we don't re-trigger the very hook that would re-enter kai.

// preCommitHookScript is a best-effort pre-commit script.
// CRITICAL: this script MUST NEVER block git. Every failure mode (missing kai
// binary, missing .kai dir, failed capture) silently no-ops with exit 0.
// Users who deleted .kai or uninstalled kai must still be able to commit.
const preCommitHookScript = `#!/bin/sh
` + kaiHookMarker + ` ` + kaiHookVersion + `
# Auto-installed by 'kai init' / 'kai hook install'.
# Best-effort: never blocks git. Remove with: kai hook uninstall

if [ "${KAI_BRIDGE_INPROGRESS:-}" = "1" ]; then
  exit 0
fi
if ! command -v kai >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d .git/kai ] && [ ! -d .kai ]; then
  exit 0
fi
# KAI_CAPTURE_SKIP_SUMMARY: change-summary tree-sitter-parses every
# modified source file. On a 22k-line file (round-22 dogfood) this
# took 15+ minutes and made the git commit look hung. Pre-commit
# captures do not display the summary, so skipping is pure win.
KAI_CAPTURE_SKIP_SUMMARY=1 kai capture >/dev/null 2>&1 || true
exit 0
`

// prePushHookScript is the same defensive shape as preCommitHookScript.
const prePushHookScript = `#!/bin/sh
` + kaiHookMarker + ` ` + kaiHookVersion + `
# Auto-installed by 'kai init' / 'kai hook install'.
# Never blocks git push (always exits 0). But unlike older versions it
# does NOT swallow 'kai push' output: a failed CI trigger is printed so
# you find out immediately, instead of CI going silently dark for days.
# Remove with: kai hook uninstall

if [ "${KAI_BRIDGE_INPROGRESS:-}" = "1" ]; then
  exit 0
fi
if ! command -v kai >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d .git/kai ] && [ ! -d .kai ]; then
  exit 0
fi
# Quiet on success, loud on failure — never block the git push.
if out=$(kai push 2>&1); then
  :
else
  printf 'kai: CI trigger FAILED — git push succeeded, but CI was NOT triggered.\n' >&2
  printf '%s\n' "$out" | sed 's/^/     /' >&2
  printf "     run 'kai doctor' to diagnose, then 'kai push' again. (git push not blocked)\n" >&2
fi
exit 0
`

// postCommitHookScript imports each new git commit as a kai snapshot. Only
// installed when the kai↔git bridge is enabled (kai init --git-bridge).
// Idempotent via content-addressing; skips commits that carry a Kai-Snapshot:
// trailer (they came from kai's own milestone→commit path).
const postCommitHookScript = `#!/bin/sh
` + kaiHookMarker + ` ` + kaiHookVersion + `
# Auto-installed by 'kai init --git-bridge'.
# Best-effort: never blocks git. Remove with: kai hook uninstall

if [ "${KAI_BRIDGE_INPROGRESS:-}" = "1" ]; then
  exit 0
fi
if ! command -v kai >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d .git/kai ] && [ ! -d .kai ]; then
  exit 0
fi
SHA=$(git rev-parse HEAD 2>/dev/null) || exit 0
kai bridge import "$SHA" >/dev/null 2>&1 || true
exit 0
`

// postMergeHookScript imports any new commits brought in by a merge or
// pull (fast-forward or otherwise). Only installed when the bridge is
// enabled. Uses ORIG_HEAD..HEAD to walk just the new commits.
const postMergeHookScript = `#!/bin/sh
` + kaiHookMarker + ` ` + kaiHookVersion + `
# Auto-installed by 'kai init --git-bridge'.
# Best-effort: never blocks git. Remove with: kai hook uninstall

if [ "${KAI_BRIDGE_INPROGRESS:-}" = "1" ]; then
  exit 0
fi
if ! command -v kai >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d .git/kai ] && [ ! -d .kai ]; then
  exit 0
fi
# Walk new commits (oldest first) and import each. ORIG_HEAD is set by
# merge/pull to the old HEAD.
git rev-list --reverse ORIG_HEAD..HEAD 2>/dev/null | while read -r SHA; do
  kai bridge import "$SHA" >/dev/null 2>&1 || true
done
exit 0
`

// postCheckoutHookScript imports any new commits after a branch switch
// that brings in history the working copy hadn't seen. $1 = previous HEAD,
// $2 = new HEAD, $3 = 1 when switching branches (0 for file checkout).
const postCheckoutHookScript = `#!/bin/sh
` + kaiHookMarker + ` ` + kaiHookVersion + `
# Auto-installed by 'kai init --git-bridge'.
# Best-effort: never blocks git. Remove with: kai hook uninstall

if [ "${KAI_BRIDGE_INPROGRESS:-}" = "1" ]; then
  exit 0
fi
if ! command -v kai >/dev/null 2>&1; then
  exit 0
fi
if [ ! -d .git/kai ] && [ ! -d .kai ]; then
  exit 0
fi
# Only act on branch switches (arg 3 = 1), not file checkouts.
if [ "$3" != "1" ]; then
  exit 0
fi
PREV="$1"
NEW="$2"
if [ "$PREV" = "$NEW" ]; then
  exit 0
fi
git rev-list --reverse "$PREV".."$NEW" 2>/dev/null | while read -r SHA; do
  kai bridge import "$SHA" >/dev/null 2>&1 || true
done
exit 0
`

// runDoctor audits local Kai state and reports findings. With --fix, it
// applies automatic repairs (currently just hook upgrades). Doctor must
// never error: every check is independent and logged inline.
func runDoctor(cmd *cobra.Command, args []string) error {
	ok := "  ✓"
	warn := "  !"
	bad := "  ✗"

	fmt.Println()
	fmt.Println("Kai doctor")
	fmt.Println()

	// kai binary
	if path, err := exec.LookPath("kai"); err == nil {
		fmt.Printf("%s kai binary: %s\n", ok, path)
	} else {
		fmt.Printf("%s kai binary not on PATH (you're running it somehow, but child processes may not find it)\n", warn)
	}

	// kai data directory
	if _, err := os.Stat(kaiDir); err == nil {
		fmt.Printf("%s kai data directory present (%s)\n", ok, kaiDir)
		// Object-store integrity. A content-addressed node whose stored
		// payload no longer hashes to its node ID is corrupt: 'kai push'
		// can't reconstruct it, so it skips it and publishes a HEADLESS
		// snapshot that 404s CI checkout. Doctor used to be blind to this
		// (everything green while CI was dark for days) — scan for it.
		checkObjectIntegrity(ok, warn, bad)
	} else {
		fmt.Printf("%s kai data directory missing — run 'kai init'\n", bad)
	}

	// git repo + hooks
	if _, err := os.Stat(".git"); err == nil {
		fmt.Printf("%s git repository detected\n", ok)
		checkHook("pre-commit", preCommitHookScript)
		checkHook("pre-push", prePushHookScript)
		if bridgeEnabled() {
			checkHook("post-commit", postCommitHookScript)
			checkHook("post-merge", postMergeHookScript)
			checkHook("post-checkout", postCheckoutHookScript)
		}
	} else {
		fmt.Printf("%s not a git repository (hook checks skipped)\n", warn)
	}

	// auth
	if token, err := remote.GetValidAccessToken(); err == nil && token != "" {
		email, _, _ := remote.GetAuthStatus()
		fmt.Printf("%s kaicontext.com auth: logged in as %s\n", ok, email)
	} else {
		fmt.Printf("%s kaicontext.com auth: not logged in (run 'kai auth login')\n", warn)
	}

	// remote
	if entry, err := remote.GetRemote("origin"); err == nil && entry != nil {
		fmt.Printf("%s remote 'origin': %s/%s\n", ok, entry.Tenant, entry.Repo)
	} else {
		fmt.Printf("%s remote 'origin' not configured\n", warn)
	}

	fmt.Println()
	if doctorFix {
		fmt.Println("Fixes applied (if any) above. Re-run 'kai doctor' to verify.")
	} else {
		fmt.Println("Run 'kai doctor --fix' to upgrade out-of-date kai-managed git hooks.")
	}
	return nil
}

// checkObjectIntegrity scans content-addressed graph nodes and reports any
// whose stored payload no longer hashes to their node ID — the exact
// corruption that makes 'kai push' skip an object and publish a headless
// snapshot (CI then 404s at checkout). It mirrors the push pre-flight check:
// digest == blake3(kind + "\n" + rawPayload). UUID-based kinds (Workspace,
// Review) are excluded because their ID is not a hash of their payload.
func checkObjectIntegrity(ok, warn, bad string) {
	db, err := openDB()
	if err != nil {
		fmt.Printf("%s object store: couldn't open to verify (%v)\n", warn, err)
		return
	}
	defer db.Close()

	contentKinds := []graph.NodeKind{
		graph.KindFile, graph.KindModule, graph.KindSymbol,
		graph.KindSnapshot, graph.KindChangeSet, graph.KindChangeType,
		graph.KindClassification, graph.KindReviewComment,
		graph.KindIntent, graph.KindAuthorshipLog,
	}

	var corrupt [][]byte
	scanned := 0
	for _, k := range contentKinds {
		nodes, err := db.GetNodesByKind(k)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			kind, raw, err := db.GetNodeRawPayload(n.ID)
			if err != nil {
				continue
			}
			scanned++
			content := append([]byte(string(kind)+"\n"), raw...)
			if !bytes.Equal(util.Blake3Hash(content), n.ID) {
				corrupt = append(corrupt, append([]byte(nil), n.ID...))
			}
		}
	}

	if len(corrupt) == 0 {
		fmt.Printf("%s object store: %d content-addressed objects, digests intact\n", ok, scanned)
		return
	}

	short := func(ids [][]byte) string {
		shown := ids
		if len(shown) > 8 {
			shown = shown[:8]
		}
		parts := make([]string, len(shown))
		for i, id := range shown {
			h := hex.EncodeToString(id)
			if len(h) > 12 {
				h = h[:12]
			}
			parts[i] = h
		}
		s := strings.Join(parts, ", ")
		if len(ids) > len(shown) {
			s += fmt.Sprintf(" (+%d more)", len(ids)-len(shown))
		}
		return s
	}

	// --fix: most corruption is mutable status (gate verdict, dismissed, ...)
	// that was written into the hashed snapshot payload. Re-storing through
	// UpdateNodePayload splits that status into snapshot_meta and restores the
	// content digest. Snapshots whose *content* was rewritten (e.g. by an old
	// in-place purge) can't be auto-healed and are reported.
	if doctorFix {
		healed := 0
		var stuck [][]byte
		for _, id := range corrupt {
			_, raw, err := db.GetNodeRawPayload(id)
			if err != nil {
				stuck = append(stuck, id)
				continue
			}
			var p map[string]interface{}
			if err := json.Unmarshal(raw, &p); err != nil {
				stuck = append(stuck, id)
				continue
			}
			if err := db.UpdateNodePayload(id, p); err != nil {
				stuck = append(stuck, id)
			} else {
				healed++
			}
		}
		if healed > 0 {
			fmt.Printf("%s object store: healed %d snapshot(s) — moved mutable status to snapshot_meta, digests restored\n", ok, healed)
		}
		if len(stuck) > 0 {
			fmt.Printf("%s object store: %d object(s) still CORRUPT (content rewritten, not status-only): %s\n", bad, len(stuck), short(stuck))
			fmt.Println("      can't auto-heal — restore a backup or re-capture/re-mint these.")
		}
		return
	}

	fmt.Printf("%s object store: %d of %d objects CORRUPT (payload doesn't match digest): %s\n",
		bad, len(corrupt), scanned, short(corrupt))
	fmt.Println("      these break 'kai push' (headless snapshot) and CI checkout (HTTP 404).")
	fmt.Println("      run 'kai doctor --fix' to heal status-only corruption (most cases).")
}

// checkHook reports the status of one git hook and, if --fix is set:
//   - installs the hook if it's missing
//   - upgrades the hook if it's a stale kai-managed script
//
// Foreign hooks are left untouched either way.
func checkHook(hookName, currentScript string) {
	path := filepath.Join(".git", "hooks", hookName)
	data, err := os.ReadFile(path)
	if err != nil {
		// Hook file doesn't exist.
		if doctorFix {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				fmt.Printf("  ✗ %s hook: install FAILED: %v\n", hookName, err)
				return
			}
			if err := os.WriteFile(path, []byte(currentScript), 0755); err != nil {
				fmt.Printf("  ✗ %s hook: install FAILED: %v\n", hookName, err)
				return
			}
			fmt.Printf("  ✓ %s hook: installed (%s)\n", hookName, kaiHookVersion)
			return
		}
		fmt.Printf("  - %s hook: not installed (run 'kai doctor --fix' to install)\n", hookName)
		return
	}
	s := string(data)
	if !strings.Contains(s, kaiHookMarker) {
		fmt.Printf("  - %s hook: present but not managed by Kai (left untouched)\n", hookName)
		return
	}
	currentTag := kaiHookMarker + " " + kaiHookVersion
	if strings.Contains(s, currentTag) {
		fmt.Printf("  ✓ %s hook: kai-managed, %s (safe)\n", hookName, kaiHookVersion)
		return
	}
	if doctorFix {
		if err := os.WriteFile(path, []byte(currentScript), 0755); err != nil {
			fmt.Printf("  ✗ %s hook: stale kai-managed hook, upgrade FAILED: %v\n", hookName, err)
			return
		}
		fmt.Printf("  ✓ %s hook: upgraded to %s\n", hookName, kaiHookVersion)
		return
	}
	fmt.Printf("  ! %s hook: STALE kai-managed hook (could block git on failure). Run 'kai doctor --fix'\n", hookName)
}

// selfHealHooks silently upgrades any old kai-managed git hook to the
// current safe version. Called from PersistentPreRun on every kai invocation.
// Must be cheap, must never panic, must never print anything.
func selfHealHooks() {
	defer func() { _ = recover() }()
	if _, err := os.Stat(".git"); err != nil {
		return
	}
	upgradeIfOldKaiHook(filepath.Join(".git", "hooks", "pre-commit"), preCommitHookScript)
	upgradeIfOldKaiHook(filepath.Join(".git", "hooks", "pre-push"), prePushHookScript)
	if bridgeEnabled() {
		upgradeIfOldKaiHook(filepath.Join(".git", "hooks", "post-commit"), postCommitHookScript)
		upgradeIfOldKaiHook(filepath.Join(".git", "hooks", "post-merge"), postMergeHookScript)
		upgradeIfOldKaiHook(filepath.Join(".git", "hooks", "post-checkout"), postCheckoutHookScript)
	}
}

// bridgeEnabled reports whether the kai↔git bridge is turned on for this
// repo. Presence of <kaiDir>/bridge-enabled is the sentinel; the file is
// written by 'kai init --git-bridge' (or 'kai bridge enable', future).
func bridgeEnabled() bool {
	_, err := os.Stat(filepath.Join(kaiDir, "bridge-enabled"))
	return err == nil
}

// kaiSnapshotTrailerRe matches the Kai-Snapshot trailer in a git commit
// message. Its presence means the commit was created by kai's milestone→commit
// path, so importing it back into kai would be a no-op loop.
var kaiSnapshotTrailerRe = regexp.MustCompile(`(?m)^Kai-Snapshot:\s*([0-9a-fA-F]+)\s*$`)

var bridgeCmd = &cobra.Command{
	Use:   "bridge",
	Short: "kai↔git bridge management",
	Long: `Manage the kai↔git bridge.

When enabled (via 'kai init --git-bridge'), the bridge makes kai and git
mutually visible:
  • Git commits authored outside kai are imported as kai snapshots
    (via the post-commit hook running 'kai bridge import').
  • Kai milestone checkpoints become git commits with Kai-* trailers.

The bridge is re-entrancy-safe: kai-authored commits are skipped on import,
and kai's pre-commit/pre-push hooks short-circuit when driven by the bridge.`,
}

var bridgeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show bridge status for this repo",
	RunE: func(cmd *cobra.Command, args []string) error {
		if bridgeEnabled() {
			fmt.Println("kai↔git bridge: enabled")
		} else {
			fmt.Println("kai↔git bridge: disabled (run 'kai init --git-bridge')")
		}
		return nil
	},
}

var bridgeImportCmd = &cobra.Command{
	Use:   "import <commit-sha>",
	Short: "Import a git commit as a kai snapshot (called by post-commit hook)",
	Long: `Import the named git commit as a kai snapshot.

Idempotent and re-entrancy-safe:
  • If the commit message carries a Kai-Snapshot: trailer, it was created by
    kai's own milestone→commit path and we skip (no double-import).
  • If a ref 'git.<short-sha>' already exists, we skip.
  • Otherwise we run 'kai capture' and write 'git.<short-sha>' pointing at
    the resulting snap.latest, plus a 'git.HEAD' convenience ref.

Designed to be called from .git/hooks/post-commit — never errors the hook.`,
	Args: cobra.ExactArgs(1),
	RunE: runBridgeImport,
}

var (
	milestoneLabel    string
	milestoneAssert   string
	milestonePlanHash string
)

var bridgeMilestoneCmd = &cobra.Command{
	Use:   "milestone",
	Short: "Create a git commit from a kai milestone (usually invoked by the MCP handler)",
	Long: `Translate a kai milestone into a git commit on the current branch.

Intended use is automatic: when the kai↔git bridge is enabled, the MCP
handler for 'kai_checkpoint_now' invokes this command so that kai
milestones show up as meaningful git commits for git-only teammates.

The commit message is <label>, followed by structured trailers:
  Kai-Snapshot: <snap.latest hex>
  Kai-Assert:   <assert value, when set>
  Kai-Plan-Hash: <plan hash, when set>

Sets KAI_BRIDGE_INPROGRESS=1 for the child git process so kai's own
pre-commit/pre-push hooks short-circuit. The post-commit hook then sees
the Kai-Snapshot: trailer and skips re-importing (no loop).

Best-effort: returns nil without creating a commit when the bridge is
disabled, the repo isn't git-backed, or git itself fails.`,
	RunE: runBridgeMilestone,
}

func runBridgeMilestone(cmd *cobra.Command, args []string) error {
	if !bridgeEnabled() {
		return nil
	}
	if _, err := os.Stat(".git"); err != nil {
		return nil
	}
	label := strings.TrimSpace(milestoneLabel)
	if label == "" {
		return fmt.Errorf("--label is required")
	}

	// Capture the working tree so snap.latest reflects the state we're
	// about to commit. Without this, the Kai-Snapshot: trailer on the
	// commit would reference the *previous* snapshot (pre-commit hook
	// would normally capture, but we silence all hooks via
	// KAI_BRIDGE_INPROGRESS below to prevent loops).
	{
		initMode = true
		if err := runCapture(cmd, nil); err != nil {
			debugf("bridge milestone: capture failed: %v", err)
			// non-fatal — the milestone still goes through, just without a
			// fresh Kai-Snapshot trailer
		}
		initMode = false
	}

	// Read snap.latest for the trailer. Missing snap.latest is fine — the
	// commit still carries the label and the assert; the Kai-Snapshot
	// trailer is just omitted.
	var snapHex string
	if db, err := openDB(); err == nil {
		if latest, _ := ref.NewRefManager(db).Get("snap.latest"); latest != nil {
			snapHex = util.BytesToHex(latest.TargetID)
		}
		db.Close()
	}

	var msg bytes.Buffer
	fmt.Fprintln(&msg, label)
	fmt.Fprintln(&msg)
	if snapHex != "" {
		fmt.Fprintf(&msg, "Kai-Snapshot: %s\n", snapHex)
	}
	if milestoneAssert != "" {
		fmt.Fprintf(&msg, "Kai-Assert: %s\n", milestoneAssert)
	}
	if milestonePlanHash != "" {
		fmt.Fprintf(&msg, "Kai-Plan-Hash: %s\n", milestonePlanHash)
	}

	msgFile, err := os.CreateTemp("", "kai-milestone-*.txt")
	if err != nil {
		return fmt.Errorf("writing milestone message: %w", err)
	}
	defer os.Remove(msgFile.Name())
	if _, err := msgFile.Write(msg.Bytes()); err != nil {
		msgFile.Close()
		return fmt.Errorf("writing milestone message: %w", err)
	}
	msgFile.Close()

	env := append(os.Environ(), "KAI_BRIDGE_INPROGRESS=1")

	// Stage current working tree. Respects .gitignore naturally.
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Env = env
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add failed: %v\n%s", err, out)
	}

	// --allow-empty so milestones work even when nothing changed since the
	// last commit (a milestone is a trust statement, not a code change).
	commitCmd := exec.Command("git", "commit", "--allow-empty", "-F", msgFile.Name())
	commitCmd.Env = env
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit failed: %v\n%s", err, out)
	}
	return nil
}

// gitHistoryRewritten reports whether advancing git.HEAD from prevSHA to newSHA
// represents an upstream history rewrite (rebase / force-push) rather than a
// normal fast-forward — i.e. prevSHA is NOT an ancestor of newSHA. Snapshots
// carry no ancestry (F-14), but git does, so we ask git. dir is the repo working
// directory ("" = current process dir). A prevSHA that no longer resolves
// (gc'd away after the rewrite) also counts as rewritten, since its history is
// gone from the new line.
func gitHistoryRewritten(dir, prevSHA, newSHA string) bool {
	if prevSHA == "" || newSHA == "" || prevSHA == newSHA {
		return false
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", prevSHA, newSHA)
	cmd.Dir = dir
	// Exit 0 => prevSHA is an ancestor of newSHA => normal fast-forward advance.
	// Exit 1 => not an ancestor (rewrite); any other error (e.g. prevSHA gc'd) =>
	// the old history is gone, also a rewrite.
	return cmd.Run() != nil
}

func runBridgeImport(cmd *cobra.Command, args []string) error {
	if !bridgeEnabled() {
		// Hook may have been installed manually; be a no-op instead of loud.
		return nil
	}
	sha := strings.TrimSpace(args[0])
	if sha == "" {
		return nil
	}
	// Normalize — accept short or full, resolve to full via git. --verify
	// makes rev-parse reject 40-char hex strings that aren't real objects
	// (without it, rev-parse cheerfully echoes any well-formed sha). The
	// ^{commit} peel ensures we resolved to a commit, not a tree or tag.
	out, err := exec.Command("git", "rev-parse", "--verify", sha+"^{commit}").Output()
	if err != nil {
		return nil // can't resolve; best-effort no-op
	}
	fullSHA := strings.TrimSpace(string(out))
	if fullSHA == "" {
		return nil
	}
	shortSHA := fullSHA
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}
	refName := "git." + shortSHA

	// Provenance check: if the commit message has a Kai-Snapshot trailer it
	// was authored by kai itself — importing would loop.
	msg, err := exec.Command("git", "log", "-1", "--format=%B", fullSHA).Output()
	if err == nil && kaiSnapshotTrailerRe.Match(msg) {
		debugf("bridge import: %s has Kai-Snapshot trailer, skipping", shortSHA)
		return nil
	}

	// Open DB and bail early if this ref already exists (idempotent re-run).
	db, err := openDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	refMgr := ref.NewRefManager(db)
	if existing, _ := refMgr.Get(refName); existing != nil {
		debugf("bridge import: %s already mapped, skipping", refName)
		return nil
	}

	// Snapshot the previous git.HEAD before we move it, so we can detect an
	// upstream history rewrite (rebase/force-push) and link the old line to the
	// new one with a SUPERSEDES edge below (F-16). Without this, a rewrite
	// imports as a fresh, unconnected snapshot and the graph silently forks into
	// two unrelated lineages.
	prevHead, _ := refMgr.Get("git.HEAD")
	var prevHeadSHA string
	if prevHead != nil {
		prevHeadSHA = prevHead.Meta["git_commit"]
	}

	// Capture current tree, then point git.<sha> at whatever snap.latest becomes.
	// runCapture is idempotent (content-addressed) — if nothing changed since
	// the last capture, snap.latest is unchanged and the ref just points there.
	initMode = true
	defer func() { initMode = false }()
	if err := runCapture(cmd, nil); err != nil {
		debugf("bridge import: capture failed: %v", err)
		return nil
	}
	latest, _ := refMgr.Get("snap.latest")
	if latest == nil {
		debugf("bridge import: no snap.latest after capture; nothing to ref")
		return nil
	}
	meta := map[string]string{
		"source":     "bridge_import_git",
		"git_commit": fullSHA,
	}
	if err := refMgr.SetWithMeta(refName, latest.TargetID, ref.KindSnapshot, "", meta); err != nil {
		debugf("bridge import: writing %s: %v", refName, err)
		return nil
	}

	// Detect an upstream history rewrite (F-16): if git.HEAD's previous commit is
	// no longer an ancestor of the one we just imported, the shared history was
	// rebased/force-pushed. Link the superseded snapshot to the new one so the
	// two lineages are connected instead of silently forked, and signal it. The
	// edge is idempotent (INSERT OR IGNORE) and only added when the two snapshots
	// actually differ.
	if prevHead != nil && !bytes.Equal(prevHead.TargetID, latest.TargetID) &&
		gitHistoryRewritten("", prevHeadSHA, fullSHA) {
		if err := db.InsertEdgeDirect(prevHead.TargetID, graph.EdgeSupersedes, latest.TargetID, nil); err != nil {
			debugf("bridge import: writing SUPERSEDES edge: %v", err)
		} else {
			debugf("bridge import: history rewrite detected (%s no longer ancestor of %s); linked SUPERSEDES %s -> %s",
				shortPrefix(prevHeadSHA), shortSHA,
				hex.EncodeToString(prevHead.TargetID)[:12], hex.EncodeToString(latest.TargetID)[:12])
		}
	}

	_ = refMgr.SetWithMeta("git.HEAD", latest.TargetID, ref.KindSnapshot, "", meta)
	return nil
}

// shortPrefix returns the first 12 chars of s (or all of s if shorter) for
// concise log lines.
func shortPrefix(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// installOrUpgradeBridgeHook is the DRY helper for the three bridge-only
// hooks (post-commit, post-merge, post-checkout). Install semantics match
// runHookInstall's pre-commit / pre-push paths: foreign hooks left alone,
// kai-managed ones upgraded, missing ones created.
func installOrUpgradeBridgeHook(hookName, script string) {
	path := filepath.Join(".git", "hooks", hookName)
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), kaiHookMarker) {
			if !initMode {
				fmt.Printf("Note: %s hook already exists (not managed by Kai). Skipping.\n", hookName)
			}
			return
		}
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			if !initMode {
				fmt.Printf("Warning: could not upgrade %s hook: %v\n", hookName, err)
			}
			return
		}
		if !initMode {
			fmt.Printf("Upgraded %s hook: .git/hooks/%s\n", hookName, hookName)
		}
		return
	}
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		if !initMode {
			fmt.Printf("Warning: could not install %s hook: %v\n", hookName, err)
		}
		return
	}
	if !initMode {
		fmt.Printf("Installed %s hook: .git/hooks/%s\n", hookName, hookName)
	}
}

// upgradeIfOldKaiHook overwrites a kai-managed hook in place if it isn't at
// the current version. Foreign hooks are left untouched.
func upgradeIfOldKaiHook(path, newScript string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s := string(data)
	if !strings.Contains(s, kaiHookMarker) {
		return // not ours
	}
	currentTag := kaiHookMarker + " " + kaiHookVersion
	if strings.Contains(s, currentTag) {
		return // already current
	}
	_ = os.WriteFile(path, []byte(newScript), 0755)
}

func runHookInstall(cmd *cobra.Command, args []string) error {
	hookPath := filepath.Join(".git", "hooks", "pre-commit")

	// Check .git exists
	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		return fmt.Errorf("not a git repository (no .git directory)")
	}

	// Create hooks directory if needed
	hooksDir := filepath.Join(".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("creating hooks directory: %w", err)
	}

	// pre-commit
	if data, err := os.ReadFile(hookPath); err == nil {
		if !strings.Contains(string(data), kaiHookMarker) {
			// Foreign hook — don't touch it. User can compose manually.
			if !initMode {
				fmt.Println("Note: pre-commit hook already exists (not managed by Kai). Skipping.")
			}
		} else {
			// Always upgrade kai-managed hooks to the current safe version.
			if err := os.WriteFile(hookPath, []byte(preCommitHookScript), 0755); err != nil {
				if !initMode {
					fmt.Printf("Warning: could not upgrade pre-commit hook: %v\n", err)
				}
			} else if !initMode {
				fmt.Println("Upgraded pre-commit hook: .git/hooks/pre-commit")
			}
		}
	} else {
		if err := os.WriteFile(hookPath, []byte(preCommitHookScript), 0755); err != nil {
			return fmt.Errorf("writing pre-commit hook: %w", err)
		}
		if !initMode {
			fmt.Println("Installed pre-commit hook: .git/hooks/pre-commit")
		}
	}

	// pre-push
	pushHookPath := filepath.Join(".git", "hooks", "pre-push")
	if data, err := os.ReadFile(pushHookPath); err == nil {
		if !strings.Contains(string(data), kaiHookMarker) {
			if !initMode {
				fmt.Println("Note: pre-push hook already exists (not managed by Kai). Skipping.")
			}
		} else {
			if err := os.WriteFile(pushHookPath, []byte(prePushHookScript), 0755); err != nil {
				if !initMode {
					fmt.Printf("Warning: could not upgrade pre-push hook: %v\n", err)
				}
			} else if !initMode {
				fmt.Println("Upgraded pre-push hook: .git/hooks/pre-push")
			}
		}
	} else {
		if err := os.WriteFile(pushHookPath, []byte(prePushHookScript), 0755); err != nil {
			if !initMode {
				fmt.Printf("Warning: could not install pre-push hook: %v\n", err)
			}
		} else if !initMode {
			fmt.Println("Installed pre-push hook: .git/hooks/pre-push")
		}
	}

	// Bridge hooks — only when the kai↔git bridge is enabled for this repo.
	if bridgeEnabled() {
		installOrUpgradeBridgeHook("post-commit", postCommitHookScript)
		installOrUpgradeBridgeHook("post-merge", postMergeHookScript)
		installOrUpgradeBridgeHook("post-checkout", postCheckoutHookScript)
	}

	if !initMode {
		fmt.Println("Kai hooks are best-effort and will never block git.")
	}
	return nil
}

func runHookUninstall(cmd *cobra.Command, args []string) error {
	hookPath := filepath.Join(".git", "hooks", "pre-commit")

	data, err := os.ReadFile(hookPath)
	if os.IsNotExist(err) {
		fmt.Println("No pre-commit hook found.")
		return nil
	}
	if err != nil {
		return err
	}

	if !strings.Contains(string(data), kaiHookMarker) {
		return fmt.Errorf("pre-commit hook exists but is not managed by Kai")
	}

	if err := os.Remove(hookPath); err != nil {
		return err
	}

	fmt.Println("Removed Kai pre-commit hook.")
	return nil
}

func init() {
	telemetry.SetVersion(Version)
	tuierrors.SetVersion(Version)

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose debug output")
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		if !verbose {
			verbose = os.Getenv("KAI_VERBOSE") == "1" || os.Getenv("KAI_VERBOSE") == "true"
		}
		// Detect TTY on stdout/stderr once per invocation so commands that
		// print colored output (e.g. kai diff) can decide whether to emit
		// ANSI escapes. Respects NO_COLOR.
		initColors()
		// `kai code` hands off to kit (syscall.Exec) moments later, which
		// replaces this process — killing the backgroundUpdateCheck goroutine
		// mid-write and bleeding the update notice into kit's first frame.
		// Skip both for code; kit owns the terminal from here.
		if cmd != codeCmd {
			printUpdateNotice()
			backgroundUpdateCheck()
		}
		// Silently upgrade old (dangerous v1) kai-managed git hooks if present.
		// Heals users who installed hooks on an older release.
		selfHealHooks()
	}
	rootCmd.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		// Flush any queued PostHog events before the CLI exits. Best-effort:
		// the PostHog client times out its own Close.
		telemetry.Close()
	}

	snapshotCreateCmd.Flags().StringVar(&repoPath, "repo", ".", "Path to the Git repository")
	snapshotCreateCmd.Flags().StringVar(&dirPath, "dir", "", "Path to directory (creates snapshot without Git)")
	snapshotCreateCmd.Flags().StringVar(&snapshotGitRef, "git", "", "Git ref to snapshot (explicit mode)")
	snapshotCreateCmd.Flags().StringVarP(&snapshotMessage, "message", "m", "", "Description for this snapshot")
	snapshotCreateCmd.Flags().BoolVar(&explainFlag, "explain", false, "Show detailed explanation of what this command does")

	// Capture command flags
	captureCmd.Flags().BoolVar(&captureExplain, "explain", false, "Show detailed explanation of what this command does")
	captureCmd.Flags().StringVarP(&captureMessage, "message", "m", "", "Capture message (shown as CI run headline)")
	captureCmd.Flags().BoolVar(&captureAll, "all", false, "Include live-synced peer changes (default: capture only your own changes)")
	intentRenderCmd.Flags().StringVar(&editText, "edit", "", "Set the intent text directly")
	intentRenderCmd.Flags().BoolVar(&regenerateIntent, "regenerate", false, "Force regenerate intent (ignore saved)")
	intentRenderCmd.Flags().BoolVar(&showAlternatives, "show-alternatives", false, "Show alternative intent suggestions")
	intentRenderCmd.Flags().Float64Var(&intentConfidence, "intent-confidence", 0.5, "Minimum confidence threshold (default 0.5)")
	intentRenderCmd.Flags().BoolVar(&explainIntent, "explain-intent", false, "Show reasoning behind intent choice")
	dumpCmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	logCmd.Flags().IntVarP(&logLimit, "limit", "n", 10, "Number of entries to show")
	logCmd.Flags().BoolVar(&logOneline, "oneline", false, "Show each snapshot on one line")
	logCmd.Flags().StringVar(&logAuthor, "author", "", "Filter by author name or email")
	logCmd.Flags().StringVar(&logGrep, "grep", "", "Filter by commit message substring")
	logCmd.Flags().StringVar(&logSince, "since", "", "Show snapshots after date (e.g. '2 weeks ago', '2026-04-01')")
	logCmd.Flags().StringVar(&logUntil, "until", "", "Show snapshots before date (e.g. 'yesterday', '2026-04-07')")
	logCmd.Flags().BoolVar(&logStat, "stat", false, "Show diffstat for each snapshot")
	logCmd.Flags().BoolVar(&logJSON, "json", false, "Output as JSON array")
	statusCmd.Flags().StringVar(&statusDir, "dir", ".", "Directory to check for changes")
	statusCmd.Flags().StringVar(&statusAgainst, "against", "", "Baseline ref/selector to compare against (default: @snap:last)")
	statusCmd.Flags().BoolVar(&statusNameOnly, "name-only", false, "Output just paths with status prefixes (A/M/D)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
	statusCmd.Flags().BoolVar(&statusSemantic, "semantic", false, "Include semantic change type analysis for modified files")
	statusCmd.Flags().BoolVar(&statusExplain, "explain", false, "Show detailed explanation of what this command does")

	// Changeset command flags
	changesetCreateCmd.Flags().StringVarP(&changesetMessage, "message", "m", "", "Changeset message describing the intent")
	changesetCreateCmd.Flags().StringVar(&changesetGitBase, "git-base", "", "Git ref for base snapshot (instead of snapshot ID)")
	changesetCreateCmd.Flags().StringVar(&changesetGitHead, "git-head", "", "Git ref for head snapshot (instead of snapshot ID)")
	changesetCreateCmd.Flags().StringVar(&changesetGitRepo, "repo", ".", "Path to Git repository (used with --git-base/--git-head)")

	// Diff command flags
	diffCmd.Flags().StringVar(&diffDir, "dir", ".", "Directory to compare against (when comparing snapshot vs working dir)")
	diffCmd.Flags().BoolVar(&diffNameOnly, "name-only", false, "Output just paths with status prefixes (A/M/D)")
	diffCmd.Flags().BoolVar(&diffSemantic, "semantic", false, "Show semantic diff (default, use --name-only to disable)")
	diffCmd.Flags().BoolVar(&diffJSON, "json", false, "Output diff as JSON (implies --semantic)")
	diffCmd.Flags().BoolVar(&diffExplain, "explain", false, "Show detailed explanation of what this command does")
	diffCmd.Flags().BoolVarP(&diffPatch, "patch", "p", false, "Show line-level diff (like git diff)")
	diffCmd.Flags().BoolVar(&diffStat, "stat", false, "Show diffstat summary (files changed, insertions, deletions)")
	diffCmd.Flags().BoolVar(&diffForce, "force", false, "Skip stale baseline warning")

	// Workspace command flags
	wsCreateCmd.Flags().StringVar(&wsName, "name", "", "Workspace name (or pass as positional arg)")
	wsCreateCmd.Flags().StringVar(&wsBase, "base", "", "Base snapshot selector (e.g., @snap:last)")
	wsCreateCmd.Flags().StringVar(&wsFromDir, "from-dir", "", "Create base from directory snapshot")
	wsCreateCmd.Flags().StringVar(&wsFromGit, "from-git", "", "Create base from Git commit/branch/tag")
	wsCreateCmd.Flags().StringVar(&wsDescription, "desc", "", "Workspace description")

	wsStageCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name (or pass as positional arg)")
	wsStageCmd.Flags().StringVar(&wsDir, "dir", ".", "Directory to stage from")
	wsStageCmd.Flags().StringVarP(&wsStageMessage, "message", "m", "", "Message describing the staged changes")
	wsStageCmd.Flags().StringVar(&wsSignKey, "sign-ssh-key", "", "Path to SSH private key for signing changesets")

	wsLogCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	wsLogCmd.MarkFlagRequired("ws")

	wsShelveCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	wsShelveCmd.MarkFlagRequired("ws")

	wsUnshelveCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	wsUnshelveCmd.MarkFlagRequired("ws")

	wsCloseCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	wsCloseCmd.MarkFlagRequired("ws")

	wsDeleteCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	wsDeleteCmd.Flags().BoolVar(&wsDeleteKeepRefs, "keep-refs", false, "Preserve workspace refs (rare)")
	wsDeleteCmd.Flags().BoolVar(&wsDeleteDryRun, "dry-run", false, "Show what would be deleted without actually deleting")
	wsDeleteCmd.MarkFlagRequired("ws")

	wsCheckoutCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name (or pass as positional arg)")
	wsCheckoutCmd.Flags().StringVar(&wsDir, "dir", ".", "Target directory to write files to")
	wsCheckoutCmd.Flags().BoolVar(&wsCheckoutClean, "clean", false, "Delete files not in snapshot")

	integrateCmd.Flags().StringVar(&wsName, "ws", "", "Workspace name or ID (required)")
	integrateCmd.Flags().StringVar(&wsTarget, "into", "", "Target snapshot ID (required)")
	integrateCmd.MarkFlagRequired("ws")
	integrateCmd.MarkFlagRequired("into")

	// Checkout command flags
	checkoutCmd.Flags().StringVar(&checkoutDir, "dir", ".", "Target directory to write files to")
	checkoutCmd.Flags().BoolVar(&checkoutClean, "clean", false, "Delete files not in snapshot")

	// Ref command flags
	refListCmd.Flags().StringVar(&refKindFilter, "kind", "", "Filter by kind (Snapshot, ChangeSet, Workspace)")

	// Pick command flags
	pickCmd.Flags().StringVar(&pickFilter, "filter", "", "Filter by substring")
	pickCmd.Flags().BoolVar(&pickNoUI, "no-ui", false, "Output matches without interactive selection")

	// Push/fetch command flags
	pushCmd.Flags().BoolVarP(&pushForce, "force", "f", false, "Force push (allow non-fast-forward)")
	pushCmd.Flags().BoolVar(&pushAll, "all", false, "Push all refs (legacy)")
	pushCmd.Flags().StringVar(&pushWorkspace, "ws", "", "Workspace to push")
	pushCmd.Flags().BoolVar(&pushDryRun, "dry-run", false, "Show what would be transferred without pushing")
	pushCmd.Flags().BoolVar(&pushExplain, "explain", false, "Show detailed explanation of what this command does")
	remoteLogCmd.Flags().StringVar(&remoteLogRef, "ref", "", "Filter by ref name")
	remoteLogCmd.Flags().IntVarP(&remoteLogLimit, "limit", "n", 20, "Number of entries to show")

	// Remote set flags
	remoteSetCmd.Flags().StringVar(&remoteTenant, "tenant", "default", "Tenant/org name for the remote")
	remoteSetCmd.Flags().StringVar(&remoteRepo, "repo", "main", "Repository name for the remote")

	// Clone flags
	cloneCmd.Flags().StringVar(&cloneTenant, "tenant", "", "Tenant/org name (extracted from URL if not specified)")
	cloneCmd.Flags().StringVar(&cloneRepo, "repo", "", "Repository name (extracted from URL if not specified)")
	cloneCmd.Flags().BoolVar(&cloneKaiOnly, "kai-only", false, "Clone from Kai only (skip git); materializes files from the latest snapshot on the remote")

	// Fetch flags
	pullCmd.Flags().BoolVar(&pullForce, "force", false, "Pull even if local has unpushed snapshots")

	fetchCmd.Flags().StringVar(&fetchWorkspace, "ws", "", "Fetch a specific workspace by name and recreate it locally")
	fetchCmd.Flags().StringVar(&fetchReview, "review", "", "Fetch a specific review by ID and recreate it locally")
	fetchCmd.Flags().BoolVar(&fetchExplain, "explain", false, "Show detailed explanation of what this command does")

	// Prune flags
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Show what would be deleted without actually deleting (default behavior)")
	pruneCmd.Flags().IntVar(&pruneSinceDays, "since", 0, "Only delete content older than N days (0 = no limit)")
	pruneCmd.Flags().BoolVar(&pruneAggressive, "aggressive", false, "Also sweep orphaned Symbols and Modules")
	pruneCmd.Flags().BoolVar(&pruneYes, "yes", false, "Actually perform the deletion (required for non-dry-run)")
	pruneCmd.Flags().StringArrayVar(&pruneKeep, "keep", nil, "Glob patterns for paths to keep (can be repeated)")

	// Purge flags
	purgeCmd.Flags().BoolVar(&purgeDryRun, "dry-run", false, "Show what would be purged (default behavior)")
	purgeCmd.Flags().BoolVar(&purgeYes, "yes", false, "Actually perform the purge (required)")

	// Review flags
	reviewOpenCmd.Flags().StringVarP(&reviewTitle, "title", "m", "", "Review title (auto-generated from changes if not provided)")
	reviewOpenCmd.Flags().StringVar(&reviewDesc, "desc", "", "Review description")
	reviewOpenCmd.Flags().StringVar(&reviewBase, "base", "", "Base ref for changeset (default: @snap:prev)")
	reviewOpenCmd.Flags().StringArrayVar(&reviewReviewers, "reviewers", nil, "Reviewers (can be specified multiple times)")
	reviewOpenCmd.Flags().BoolVar(&reviewExplain, "explain", false, "Show detailed explanation of what this command does")

	reviewViewCmd.Flags().BoolVar(&reviewJSON, "json", false, "Output as JSON")
	reviewViewCmd.Flags().StringVar(&reviewViewMode, "view", "semantic", "View mode: semantic, text, or mixed")
	reviewViewCmd.Flags().BoolVarP(&reviewSummary, "summary", "s", true, "Show progressive disclosure summary (default)")
	reviewViewCmd.Flags().BoolVarP(&reviewInteractive, "interactive", "i", false, "Interactive mode: drill down into changes")

	reviewCloseCmd.Flags().StringVar(&reviewCloseState, "state", "", "Close state: merged or abandoned (required)")
	reviewCloseCmd.MarkFlagRequired("state")

	reviewEditCmd.Flags().StringVar(&reviewEditTitle, "title", "", "New title")
	reviewEditCmd.Flags().StringVar(&reviewEditDesc, "desc", "", "New description")
	reviewEditCmd.Flags().StringArrayVar(&reviewEditAssignees, "assignees", nil, "Assignees (can be specified multiple times)")

	reviewExportCmd.Flags().BoolVar(&reviewExportMD, "markdown", false, "Export as markdown")
	reviewExportCmd.Flags().BoolVar(&reviewExportHTML, "html", false, "Export as HTML")

	reviewCommentCmd.Flags().StringVarP(&reviewCommentBody, "message", "m", "", "Comment body (required)")
	reviewCommentCmd.Flags().StringVar(&reviewCommentFile, "file", "", "Anchor comment to a file path")
	reviewCommentCmd.Flags().IntVar(&reviewCommentLine, "line", 0, "Anchor comment to a line number (requires --file)")
	reviewCommentCmd.MarkFlagRequired("message")

	// Merge flags
	mergeCmd.Flags().StringVar(&mergeLang, "lang", "", "Language (js, ts, py) - auto-detected from extension if not specified")
	mergeCmd.Flags().StringVarP(&mergeOutput, "output", "o", "", "Output file path (defaults to stdout)")
	mergeCmd.Flags().BoolVar(&mergeJSON, "json", false, "Output result as JSON (includes conflicts)")

	// Modules init flags
	modulesInitCmd.Flags().BoolVar(&modulesInfer, "infer", false, "Auto-detect modules from source structure")
	modulesInitCmd.Flags().BoolVar(&modulesWrite, "write", false, "Write configuration to .kai/rules/modules.yaml")
	modulesInitCmd.Flags().StringVar(&modulesBy, "by", "dirs", "Grouping strategy: dirs (directories) or globs")
	modulesInitCmd.Flags().StringVar(&modulesTestsGlob, "tests", "", "Glob pattern for test files (e.g., \"tests/**\")")
	modulesInitCmd.Flags().BoolVar(&modulesDryRun, "dry-run", false, "Preview changes without writing")

	// Add remote subcommands
	remoteCmd.AddCommand(remoteSetCmd)
	remoteCmd.AddCommand(remoteGetCmd)
	remoteCmd.AddCommand(remoteListCmd)
	remoteCmd.AddCommand(remoteDelCmd)

	orgDeleteCmd.Flags().BoolVarP(&orgDeleteYes, "yes", "y", false, "Skip confirmation prompt (dangerous)")
	orgCmd.AddCommand(orgListCmd)
	orgCmd.AddCommand(orgDeleteCmd)

	// Add ref subcommands
	refCmd.AddCommand(refListCmd)
	refCmd.AddCommand(refSetCmd)
	refCmd.AddCommand(refDelCmd)
	tagCmd.AddCommand(tagListCmd)
	tagCmd.AddCommand(tagCreateCmd)
	tagCmd.AddCommand(tagDeleteCmd)
	bisectCmd.AddCommand(bisectStartCmd)
	bisectCmd.AddCommand(bisectGoodCmd)
	bisectCmd.AddCommand(bisectBadCmd)
	bisectCmd.AddCommand(bisectSkipCmd)
	bisectCmd.AddCommand(bisectNextCmd)
	bisectCmd.AddCommand(bisectResetCmd)
	shadowCmd.AddCommand(shadowImportCmd)
	shadowCmd.AddCommand(shadowParityCmd)
	shadowCmd.AddCommand(shadowDriftCmd)
	shadowCmd.AddCommand(shadowRunCmd)

	shadowRunCmd.Flags().StringVar(&shadowGitRange, "git-range", "", "Git range BASE..HEAD")
	shadowRunCmd.Flags().StringVar(&shadowGitRepo, "repo", ".", "Path to Git repository")
	shadowRunCmd.Flags().StringVar(&shadowFullCmd, "full", "", "Command to run full test suite")
	shadowRunCmd.Flags().StringVar(&shadowKaiCmd, "kai", "", "Command to run selective tests (use {{tests}} for test list)")
	shadowRunCmd.Flags().IntVar(&shadowRetries, "retries", 0, "Number of retries for flaky detection")
	shadowRunCmd.Flags().StringVar(&shadowOutJSON, "out", "", "Path to write JSON report")
	shadowRunCmd.Flags().StringVar(&shadowOutMD, "summary", "", "Path to write Markdown summary")
	shadowRunCmd.Flags().BoolVar(&shadowSkipFullOnFail, "skip-full-on-fail", false, "Skip full run if selective fails")
	shadowRunCmd.Flags().StringVar(&shadowResultFormat, "format", "auto", "Test result format: auto, junit, jest, pytest, go")
	shadowRunCmd.MarkFlagRequired("git-range")
	shadowRunCmd.MarkFlagRequired("full")
	shadowRunCmd.MarkFlagRequired("kai")

	shadowImportCmd.Flags().StringVar(&shadowGitRange, "git-range", "", "Git range BASE..HEAD")
	shadowImportCmd.Flags().StringVar(&shadowGitRepo, "repo", ".", "Path to Git repository")
	shadowImportCmd.Flags().StringVar(&shadowUpdateRef, "update-ref", "snap.main", "Ref to update to HEAD snapshot")
	shadowImportCmd.MarkFlagRequired("git-range")

	shadowParityCmd.Flags().StringVar(&shadowGitRange, "git-range", "", "Git range BASE..HEAD")
	shadowParityCmd.Flags().StringVar(&shadowGitRepo, "repo", ".", "Path to Git repository")
	shadowParityCmd.MarkFlagRequired("git-range")

	shadowDriftCmd.Flags().StringVar(&shadowGitRef, "git-ref", "HEAD", "Git ref to compare")
	shadowDriftCmd.Flags().StringVar(&shadowGitRepo, "repo", ".", "Path to Git repository")
	shadowDriftCmd.Flags().StringVar(&shadowSnapRef, "snap", "snap.main", "Snapshot ref to compare against")

	// Add modules subcommands
	modulesCmd.AddCommand(modulesInitCmd)
	modulesCmd.AddCommand(modulesAddCmd)
	modulesCmd.AddCommand(modulesListCmd)
	modulesCmd.AddCommand(modulesPreviewCmd)
	modulesCmd.AddCommand(modulesShowCmd)
	modulesCmd.AddCommand(modulesRmCmd)

	// Set up dynamic completions for commands that accept IDs
	analyzeSymbolsCmd.ValidArgsFunction = completeSnapshotID
	analyzeCallsCmd.ValidArgsFunction = completeSnapshotID
	listSymbolsCmd.ValidArgsFunction = completeSnapshotID
	changesetCreateCmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Both args are snapshot IDs
		return completeSnapshotID(cmd, args, toComplete)
	}
	intentRenderCmd.ValidArgsFunction = completeChangeSetID
	dumpCmd.ValidArgsFunction = completeChangeSetID
	checkoutCmd.ValidArgsFunction = completeSnapshotID
	refDelCmd.ValidArgsFunction = completeRefName

	// Add workspace subcommands
	wsCmd.AddCommand(wsCreateCmd)
	wsCmd.AddCommand(wsListCmd)
	wsCmd.AddCommand(wsStageCmd)
	wsCmd.AddCommand(wsLogCmd)
	wsCmd.AddCommand(wsShelveCmd)
	wsCmd.AddCommand(wsUnshelveCmd)
	wsCmd.AddCommand(wsCloseCmd)
	wsCmd.AddCommand(wsDeleteCmd)
	wsCmd.AddCommand(wsCheckoutCmd)
	wsCmd.AddCommand(wsCurrentCmd)
	wsCurrentCmd.Flags().BoolVar(&wsCurrentJSON, "json", false, "Output workspace name as JSON object")

	analyzeCmd.AddCommand(analyzeSymbolsCmd)
	analyzeCmd.AddCommand(analyzeCallsCmd)
	analyzeCmd.AddCommand(analyzeDepsCmd)

	// Snapshot subcommands
	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	snapshotListCmd.Flags().BoolVar(&snapshotListJSON, "json", false, "Output as JSON array")
	gateListCmd.Flags().BoolVar(&gateListJSON, "json", false, "Output as JSON")

	// Changeset subcommands
	changesetCmd.AddCommand(changesetCreateCmd)
	changesetCmd.AddCommand(changesetListCmd)

	intentCmd.AddCommand(intentRenderCmd)

	// Deprecated list commands (kept for backwards compatibility)
	listCmd.AddCommand(listSnapshotsCmd)
	listCmd.AddCommand(listChangesetsCmd)
	listCmd.AddCommand(listSymbolsCmd)

	testCmd.AddCommand(testAffectedCmd)

	// Query commands
	queryCmd.AddCommand(queryCallersCmd)
	queryCmd.AddCommand(queryDependentsCmd)
	queryCmd.AddCommand(queryImpactCmd)
	queryCallersCmd.Flags().StringVar(&queryFileFlag, "file", "", "File where the symbol is defined (narrows search)")
	queryImpactCmd.Flags().IntVar(&queryDepthFlag, "depth", 3, "Maximum graph traversal depth")

	// CI commands
	ciCmd.AddCommand(ciPlanCmd)
	ciCmd.AddCommand(ciPrintCmd)
	ciCmd.AddCommand(ciDetectRuntimeRiskCmd)
	ciCmd.AddCommand(ciRecordMissCmd)
	ciCmd.AddCommand(ciExplainDynamicImportsCmd)
	ciCmd.AddCommand(ciIngestCoverageCmd)
	ciCmd.AddCommand(ciIngestContractsCmd)
	ciCmd.AddCommand(ciAnnotatePlanCmd)
	ciCmd.AddCommand(ciValidatePlanCmd)
	ciValidatePlanCmd.Flags().BoolVar(&ciValidateStrict, "strict", false, "Validate optional fields as well")
	ciCmd.AddCommand(ciCommentCmd)
	ciCommentCmd.Flags().StringVar(&ciCommentReport, "report", "", "Path to shadow report JSON")
	ciCommentCmd.Flags().StringVar(&ciCommentToken, "token", "", "GitHub API token (default: $GITHUB_TOKEN)")

	ciCmd.AddCommand(ciAuthorshipCmd)
	ciAuthorshipCmd.Flags().StringVar(&ciAuthorshipToken, "token", "", "GitHub API token (default: $GITHUB_TOKEN)")
	ciAuthorshipCmd.Flags().StringVar(&ciAuthorshipRepo, "repo", "", "GitHub repo owner/name (default: $GITHUB_REPOSITORY)")
	ciAuthorshipCmd.Flags().IntVar(&ciAuthorshipPR, "pr", 0, "PR number (default: auto-detect)")
	ciAuthorshipCmd.Flags().BoolVar(&ciAuthorshipDryRun, "dry-run", false, "Print comment to stdout instead of posting")

	// Remote CI commands (per protocol spec docs/protocol.md section 3)
	ciCmd.AddCommand(ciStatusCmd)
	ciCmd.AddCommand(ciRunsCmd)
	ciCmd.AddCommand(ciRunCmd)
	ciCmd.AddCommand(ciLogsCmd)
	ciCmd.AddCommand(ciTraceCmd)
	ciCmd.AddCommand(ciCancelCmd)
	ciCmd.AddCommand(ciRerunCmd)
	ciCmd.AddCommand(ciSecretsCmd)
	ciCmd.AddCommand(ciSecretSetCmd)
	ciRunsCmd.Flags().IntVar(&ciRunsLimit, "limit", 10, "Number of runs to show")
	ciLogsCmd.Flags().StringVar(&ciLogsJob, "job", "", "Job name (default: first failed or first job)")
	ciTraceCmd.Flags().StringVar(&ciTraceJob, "job", "", "Only trace a specific job")
	ciCommentCmd.Flags().StringVar(&ciCommentRepo, "repo", "", "GitHub repo owner/name (default: $GITHUB_REPOSITORY)")
	ciCommentCmd.Flags().IntVar(&ciCommentPR, "pr", 0, "PR number (default: auto-detect from $GITHUB_EVENT_PATH)")
	ciCommentCmd.Flags().BoolVar(&ciCommentDryRun, "dry-run", false, "Print comment to stdout instead of posting")
	ciCommentCmd.MarkFlagRequired("report")
	ciPlanCmd.Flags().StringVar(&ciStrategy, "strategy", "auto", "Selection strategy: auto, symbols, imports, coverage")
	ciPlanCmd.Flags().StringVar(&ciRiskPolicy, "risk-policy", "expand", "Risk policy: expand, warn, fail")
	ciPlanCmd.Flags().StringVar(&ciOutFile, "out", "", "Output file for plan JSON")
	ciPlanCmd.Flags().StringVar(&ciSafetyMode, "safety-mode", "guarded", "Safety mode: shadow (learn-only), guarded (safe fallback), strict (no fallback)")
	ciPlanCmd.Flags().BoolVar(&ciExplain, "explain", false, "Output human-readable explanation table instead of JSON")
	ciPlanCmd.Flags().StringVar(&ciGitRange, "git-range", "", "Git range BASE..HEAD to create changeset from (e.g., main..feature)")
	ciPlanCmd.Flags().StringVar(&ciGitRepo, "repo", ".", "Path to Git repository (used with --git-range)")
	ciPlanCmd.Flags().BoolVar(&ciNoFast, "no-fast", false, "Skip fast diff-only path and force full snapshot")
	ciPrintCmd.Flags().StringVar(&ciPlanFile, "plan", "plan.json", "Path to plan file")
	ciPrintCmd.Flags().StringVar(&ciSection, "section", "summary", "Section to display: targets, impact, summary")
	// detect-runtime-risk flags
	ciDetectRuntimeRiskCmd.Flags().StringVar(&ciLogsFile, "logs", "", "Path to test output JSON (Jest, Mocha, pytest, etc.)")
	ciDetectRuntimeRiskCmd.Flags().StringVar(&ciStderrFile, "stderr", "", "Path to stderr/text log file")
	ciDetectRuntimeRiskCmd.Flags().StringVar(&ciLogFormat, "format", "auto", "Log format: auto, jest, mocha, pytest, go, text")
	ciDetectRuntimeRiskCmd.Flags().StringVar(&ciPlanFile, "plan", "", "Path to plan file (for cross-reference)")
	ciDetectRuntimeRiskCmd.Flags().BoolVar(&ciTripwire, "tripwire", false, "Tripwire mode: exit 75 if rerun needed, 0 otherwise")
	ciDetectRuntimeRiskCmd.Flags().BoolVar(&ciRerunOnFail, "rerun-on-fail", false, "Treat any test failure as a tripwire trigger")
	// record-miss flags
	ciRecordMissCmd.Flags().StringVar(&ciPlanFile, "plan", "", "Path to plan file (required)")
	ciRecordMissCmd.Flags().StringVar(&ciEvidenceFile, "evidence", "", "Path to test results JSON")
	ciRecordMissCmd.Flags().StringVar(&ciFailedTests, "failed", "", "Comma-separated list of failed test files")
	// ingest-coverage flags
	ciIngestCoverageCmd.Flags().StringVar(&ciCoverageFrom, "from", "", "Path to coverage report file(s)")
	ciIngestCoverageCmd.Flags().StringVar(&ciCoverageFormat, "format", "auto", "Coverage format: auto, nyc, coveragepy, jacoco")
	ciIngestCoverageCmd.Flags().StringVar(&ciCoverageBranch, "branch", "", "Branch name for tagging")
	ciIngestCoverageCmd.Flags().StringVar(&ciCoverageTag, "tag", "", "Tag/identifier for this coverage run")
	ciIngestCoverageCmd.MarkFlagRequired("from")
	// ingest-contracts flags
	ciIngestContractsCmd.Flags().StringVar(&ciContractType, "type", "", "Contract type: openapi, protobuf, graphql")
	ciIngestContractsCmd.Flags().StringVar(&ciContractPath, "path", "", "Path to schema file")
	ciIngestContractsCmd.Flags().StringVar(&ciContractService, "service", "", "Service/module name")
	ciIngestContractsCmd.Flags().StringVar(&ciContractTests, "tests", "", "Glob pattern for contract tests")
	ciIngestContractsCmd.Flags().StringVar(&ciContractGenerated, "generated", "", "Glob pattern for generated files")
	ciIngestContractsCmd.MarkFlagRequired("type")
	ciIngestContractsCmd.MarkFlagRequired("path")
	ciIngestContractsCmd.MarkFlagRequired("tests")
	// annotate-plan flags
	ciAnnotatePlanCmd.Flags().BoolVar(&ciFallbackUsed, "fallback.used", false, "Whether fallback was triggered")
	ciAnnotatePlanCmd.Flags().StringVar(&ciFallbackReason, "fallback.reason", "", "Reason: runtime_tripwire, planner_over_threshold, panic_switch")
	ciAnnotatePlanCmd.Flags().StringVar(&ciFallbackTrigger, "fallback.trigger", "", "What triggered fallback (e.g., 'Cannot find module')")
	ciAnnotatePlanCmd.Flags().IntVar(&ciFallbackExitCode, "fallback.exitCode", 0, "Exit code that triggered fallback")

	// Define command groups for organized help output
	// Note: Workspaces are intentionally in Advanced to reduce cognitive load for new users
	rootCmd.AddGroup(
		&cobra.Group{ID: groupStart, Title: "Getting Started:"},
		&cobra.Group{ID: groupDiff, Title: "Diff & Review:"},
		&cobra.Group{ID: groupQuery, Title: "Query:"},
		&cobra.Group{ID: groupCI, Title: "CI & Testing:"},
		&cobra.Group{ID: groupRemote, Title: "Remote & Sync:"},
		&cobra.Group{ID: groupAdvanced, Title: "Advanced:"},
	)

	// Getting Started
	initCmd.GroupID = groupStart
	captureCmd.GroupID = groupStart
	initCmd.Flags().BoolVar(&initExplain, "explain", false, "Show detailed explanation of what this command does")
	initCmd.Flags().BoolVar(&initGitBridge, "git-bridge", false, "Enable the kai↔git bridge (installs post-commit hook; kai milestones become git commits)")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Re-run initialization even if kai is already set up in this directory")
	initCmd.Flags().BoolVar(&initAssumeYes, "yes", false, "Non-interactive: auto-select the personal org and link an existing repo (also implied when stdin isn't a TTY)")
	initCmd.Flags().StringVar(&initOrg, "org", "", "Org slug to initialize under (default: your personal org; also via KAI_ORG)")
	initCmd.Flags().StringVar(&initEmail, "email", "", "Email to sign up / log in with non-interactively (also via KAI_INIT_EMAIL)")
	initCmd.Flags().BoolVar(&initNoRemote, "no-remote", false, "Build the local semantic graph only: skip kaicontext.com signup/login and the automatic push")
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(captureCmd)

	// `kai code` — unified coding entrypoint (kit-in-kai Phase 1). Resolves
	// and self-installs the managed kit binary, then hands off to it.
	// groupStart so it surfaces alongside init/capture as a primary action.
	// codeCmd is declared in tui.go (repurposed from the old orphaned
	// native-TUI command); it must be registered exactly once.
	codeCmd.GroupID = groupStart
	rootCmd.AddCommand(codeCmd)

	rootCmd.AddCommand(importCmd)
	importCmd.GroupID = groupStart
	importCmd.Flags().BoolVar(&importAll, "all", false, "Import entire git history")
	importCmd.Flags().IntVar(&importMaxCommits, "max", 50, "Maximum number of commits to import")

	// Diff & Review
	diffCmd.GroupID = groupDiff
	statusCmd.GroupID = groupDiff
	reviewCmd.GroupID = groupDiff
	changesetCmd.GroupID = groupDiff
	intentCmd.GroupID = groupDiff
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(changesetCmd)
	rootCmd.AddCommand(intentCmd)

	// Workspaces (in Advanced group to reduce PLG cognitive load)
	wsCmd.GroupID = groupAdvanced
	integrateCmd.GroupID = groupAdvanced
	mergeCmd.GroupID = groupAdvanced
	checkoutCmd.GroupID = groupAdvanced
	rootCmd.AddCommand(wsCmd)
	rootCmd.AddCommand(integrateCmd)
	rootCmd.AddCommand(spawnCmd)
	rootCmd.AddCommand(despawnCmd)
	rootCmd.AddCommand(uiCmd)

	resolveCmd.Flags().BoolVar(&resolveContinue, "continue", false, "Apply user-edited resolutions from .kai/conflicts/<workspace>/")
	resolveCmd.Flags().BoolVar(&resolveAbort, "abort", false, "Discard pending conflict state for the workspace")
	rootCmd.AddCommand(resolveCmd)

	gateCmd.AddCommand(gateListCmd, gateShowCmd, gateApproveCmd, gateRejectCmd, gateDiffCmd, gateReviewCmd, gateFixCmd)
	gateFixCmd.Flags().BoolVar(&gateFixAutoApprove, "auto-approve", false,
		"if the post-fix verdict is Auto, fail loudly when refs were not advanced")
	gateFixCmd.Flags().IntVar(&gateFixMaxTurns, "max-turns", 3,
		"max planner turns for the fix agent (capped at 5 by gatereview.Fix)")

	runLastCmd.Flags().StringVar(&runSessionFlag, "session", "", "Session id (default: most recent)")
	runDiffCmd.Flags().StringVar(&runSessionFlag, "session", "", "Session id (default: most recent)")
	runSummaryCmd.Flags().StringVar(&runSessionFlag, "session", "", "Session id (default: most recent)")
	runSummaryCmd.Flags().BoolVar(&runAllFlag, "all", false, "Aggregate across every session under .kai/runs/")
	runCmd.AddCommand(runLastCmd, runDiffCmd, runSummaryCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(gateCmd)

	rootCmd.AddCommand(mergeCmd)
	rootCmd.AddCommand(checkoutCmd)

	// Query
	queryCmd.GroupID = groupQuery
	rootCmd.AddCommand(queryCmd)

	// Authorship
	blameCmd.GroupID = groupQuery
	statsCmd.GroupID = groupQuery
	blameCmd.Flags().BoolVar(&blameSummary, "summary", false, "Show summary percentages only")
	blameCmd.Flags().BoolVar(&blameJSON, "json", false, "Output as JSON")
	statsCmd.Flags().BoolVar(&statsJSON, "json", false, "Output as JSON")
	checkpointCmd.Flags().StringVar(&checkpointAgent, "agent", "", "Agent name (default: read from stdin or session file)")
	checkpointCmd.Flags().StringVar(&checkpointModel, "model", "", "Model name (optional)")
	checkpointCmd.Flags().StringVar(&checkpointFile, "file", "", "Edited file (default: read from stdin tool input)")
	checkpointCmd.Flags().StringVar(&checkpointLines, "lines", "", "Line range 'start-end' (default: computed from stdin tool input)")
	rootCmd.AddCommand(blameCmd)
	rootCmd.AddCommand(statsCmd)
	rootCmd.AddCommand(checkpointCmd)

	// CI & Testing
	ciCmd.GroupID = groupCI
	testCmd.GroupID = groupCI
	rootCmd.AddCommand(ciCmd)
	rootCmd.AddCommand(testCmd)

	// Remote & Sync
	remoteCmd.GroupID = groupRemote
	pushCmd.GroupID = groupRemote
	fetchCmd.GroupID = groupRemote
	pullCmd.GroupID = groupRemote
	cloneCmd.GroupID = groupRemote
	authCmd.GroupID = groupRemote
	updateCmd.GroupID = groupRemote
	rootCmd.AddCommand(remoteCmd)
	orgCmd.GroupID = groupRemote
	rootCmd.AddCommand(orgCmd)
	rootCmd.AddCommand(pushCmd)
	rootCmd.AddCommand(fetchCmd)
	rootCmd.AddCommand(pullCmd)
	rootCmd.AddCommand(cloneCmd)
	rootCmd.AddCommand(updateCmd)
	updateCmd.Flags().Bool("check", false, "Check for updates without installing")

	liveCmd.GroupID = groupRemote
	liveCmd.AddCommand(liveStatusCmd)
	liveCmd.AddCommand(liveOnCmd)
	liveCmd.AddCommand(liveOffCmd)
	liveCmd.AddCommand(liveRunCmd)
	liveCmd.AddCommand(liveCheckpointCmd)
	liveCmd.AddCommand(liveModeCmd)
	liveRunCmd.Flags().BoolVar(&liveRunCheckpoint, "checkpoint", false, "Checkpoint mode: hold local edits, push only on 'kai live checkpoint'")
	liveOnCmd.Flags().String("files", "", "Comma-separated file paths to scope sync to (default: all files)")
	liveStatusCmd.Flags().Int64("since", 0, "Only count sync_events with seq greater than this")
	liveStatusCmd.Flags().Int("limit", 100, "Max files listed")
	liveStatusCmd.Flags().Bool("no-files", false, "Skip listing touched files (summary only)")
	rootCmd.AddCommand(liveCmd)

	// Advanced (low-level/plumbing commands)
	snapshotCmd.GroupID = groupAdvanced
	snapCmd.GroupID = groupAdvanced
	analyzeCmd.GroupID = groupAdvanced
	dumpCmd.GroupID = groupAdvanced
	graphCmd.GroupID = groupAdvanced
	listCmd.GroupID = groupAdvanced
	logCmd.GroupID = groupAdvanced
	refCmd.GroupID = groupAdvanced
	tagCmd.GroupID = groupAdvanced
	cherryPickCmd.GroupID = groupAdvanced
	rebaseCmd.GroupID = groupAdvanced
	bisectCmd.GroupID = groupAdvanced
	shadowCmd.GroupID = groupAdvanced
	modulesCmd.GroupID = groupAdvanced
	pickCmd.GroupID = groupAdvanced
	pruneCmd.GroupID = groupAdvanced
	purgeCmd.GroupID = groupAdvanced
	completionCmd.GroupID = groupAdvanced
	remoteLogCmd.GroupID = groupAdvanced
	telemetryCmd.GroupID = groupAdvanced
	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(snapCmd)
	rootCmd.AddCommand(analyzeCmd)
	rootCmd.AddCommand(dumpCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(logCmd)
	rootCmd.AddCommand(refCmd)
	rootCmd.AddCommand(tagCmd)
	rootCmd.AddCommand(cherryPickCmd)
	rootCmd.AddCommand(rebaseCmd)
	rootCmd.AddCommand(bisectCmd)
	rootCmd.AddCommand(shadowCmd)
	rootCmd.AddCommand(modulesCmd)
	rootCmd.AddCommand(pickCmd)
	rootCmd.AddCommand(pruneCmd)
	rootCmd.AddCommand(purgeCmd)
	rootCmd.AddCommand(completionCmd)
	rootCmd.AddCommand(remoteLogCmd)

	// Add telemetry subcommands
	telemetryCmd.AddCommand(telemetryEnableCmd)
	telemetryCmd.AddCommand(telemetryDisableCmd)
	telemetryCmd.AddCommand(telemetryStatusCmd)
	telemetryCmd.AddCommand(telemetryFlushCmd)
	rootCmd.AddCommand(telemetryCmd)

	// Version subcommand
	versionCmd.Flags().BoolVar(&versionShort, "short", false, "Print just the version number (strip 'kai ' prefix and pre-release/build metadata)")
	versionCmd.Flags().BoolVar(&versionJSON, "json", false, "Emit version as JSON ({version, build, commit})")
	rootCmd.AddCommand(versionCmd)

	// Bench
	benchCmd.GroupID = groupCI
	benchCmd.Flags().StringVar(&benchTask, "task", "", "The coding task/question to benchmark (required)")
	benchCmd.Flags().StringVar(&benchModel, "model", "", "Claude model to use (e.g. sonnet, opus)")
	_ = benchCmd.MarkFlagRequired("task")
	rootCmd.AddCommand(benchCmd)

	// Pre-injection context
	primeCmd.GroupID = groupAdvanced
	rootCmd.AddCommand(primeCmd)

	// Hook management
	hookCmd.GroupID = groupStart
	hookCmd.AddCommand(hookInstallCmd)
	hookCmd.AddCommand(hookUninstallCmd)
	rootCmd.AddCommand(hookCmd)

	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "Apply automatic repairs (currently: upgrade stale kai-managed git hooks)")
	rootCmd.AddCommand(doctorCmd)

	bridgeCmd.AddCommand(bridgeImportCmd)
	bridgeCmd.AddCommand(bridgeStatusCmd)
	bridgeMilestoneCmd.Flags().StringVar(&milestoneLabel, "label", "", "Milestone label (becomes the git commit subject)")
	bridgeMilestoneCmd.Flags().StringVar(&milestoneAssert, "assert", "", "Trust assertion (tests-pass, types-ok, lints-clean, manual-verified)")
	bridgeMilestoneCmd.Flags().StringVar(&milestonePlanHash, "plan-hash", "", "Plan hash (optional, used with assert=tests-pass)")
	bridgeCmd.AddCommand(bridgeMilestoneCmd)
	rootCmd.AddCommand(bridgeCmd)

	// Add auth subcommands
	authLoginCmd.Flags().StringVar(&authLoginToken, "token", "", "Access token for non-interactive login (CI)")
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authStatusCmd)
	rootCmd.AddCommand(authCmd)

	// Billing usage
	usageCmd.GroupID = groupRemote
	rootCmd.AddCommand(usageCmd)

	// Add review subcommands
	reviewCmd.AddCommand(reviewOpenCmd)
	reviewCmd.AddCommand(reviewListCmd)
	reviewCmd.AddCommand(reviewViewCmd)
	reviewCmd.AddCommand(reviewStatusCmd)
	reviewCmd.AddCommand(reviewApproveCmd)
	reviewCmd.AddCommand(reviewRequestChangesCmd)
	reviewCmd.AddCommand(reviewCloseCmd)
	reviewCmd.AddCommand(reviewEditCmd)
	reviewCmd.AddCommand(reviewReadyCmd)
	reviewCmd.AddCommand(reviewExportCmd)
	reviewCmd.AddCommand(reviewSummaryCmd)
	reviewCmd.AddCommand(reviewCommentCmd)
	reviewCmd.AddCommand(reviewCommentsCmd)
	reviewSummaryCmd.Flags().BoolVarP(&reviewInteractive, "interactive", "i", false, "Interactive drill-down mode")
	reviewSummaryCmd.Flags().BoolVar(&reviewAI, "ai", false, "Run AI review (requires ANTHROPIC_API_KEY)")
	rootCmd.AddCommand(reviewCmd)

	// Config commands
	configCmd.GroupID = groupAdvanced
	configCmd.AddCommand(configShowCmd)
	configShowCmd.Flags().BoolVar(&configShowJSON, "json", false, "Output as JSON instead of YAML")
	configShowCmd.Flags().BoolVarP(&configShowQuiet, "quiet", "q", false, "Suppress the runtime block and print only the config section")
	rootCmd.AddCommand(configCmd)

	// Graph commands (graphCmd + graphExportCmd defined in graph.go;
	// the subcommand is wired in graph.go's init).
	rootCmd.AddCommand(graphCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// shortID safely truncates an ID string to 12 characters.
func shortID(s string) string {
	if len(s) >= 12 {
		return s[:12]
	}
	return s
}

// snapshotAuthor returns the identity to record on snapshots taken
// in the current repo. Distinct from git.author (the last upstream
// commit's author): this is the kai USER, not whoever wrote the
// code state the snapshot reflects. Sourced in priority order:
//
//  1. git config user.name + user.email (the obvious answer when set)
//  2. $USER + hostname (covers fresh systems without a git identity)
//  3. "" (last resort — the kai log will fall back to git.author)
//
// Returns the name in canonical "Name <email>" form when both halves
// are available, or just one half if only one is.
func snapshotAuthor() string {
	name := strings.TrimSpace(runGitConfig("user.name"))
	email := strings.TrimSpace(runGitConfig("user.email"))
	if name == "" && email == "" {
		// Fall back to $USER@hostname so a fresh dev box still
		// surfaces something more useful than the upstream
		// committer of whatever they cloned.
		user := strings.TrimSpace(os.Getenv("USER"))
		if user == "" {
			user = strings.TrimSpace(os.Getenv("USERNAME")) // Windows
		}
		host, _ := os.Hostname()
		switch {
		case user == "" && host == "":
			return ""
		case user == "":
			return host
		case host == "":
			return user
		default:
			return fmt.Sprintf("%s@%s", user, host)
		}
	}
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	default:
		return email
	}
}

// sameAuthor compares two "Name <email>" strings tolerant of
// whitespace and case. Used by `kai log` to suppress the redundant
// "Git commit by: ..." line when kai.author and git.author match
// (the common case on repos you actively commit to).
func sameAuthor(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// runGitConfig returns the value of `git config <key>` or "" if
// unset / git missing. Silent — this is best-effort metadata.
func runGitConfig(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// debugf prints a timestamped debug message to stderr when verbose mode is active.
func debugf(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "[kai debug] %s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// skipModulesFile is set by clone to skip creating kai.modules.yaml
var skipModulesFile bool
var initExplain bool
var initGitBridge bool
var initForce bool
var initAssumeYes bool
var initOrg string
var initEmail string
var initNoRemote bool

// initOrgOverride returns an explicit org slug from --org or KAI_ORG, else "".
func initOrgOverride() string {
	if strings.TrimSpace(initOrg) != "" {
		return strings.TrimSpace(initOrg)
	}
	return strings.TrimSpace(os.Getenv("KAI_ORG"))
}

func findOrgBySlug(orgs []remote.OrgInfo, slug string) *remote.OrgInfo {
	for i := range orgs {
		if orgs[i].Slug == slug {
			return &orgs[i]
		}
	}
	return nil
}

func orgSlugs(orgs []remote.OrgInfo) string {
	s := make([]string, len(orgs))
	for i, o := range orgs {
		s[i] = o.Slug
	}
	return strings.Join(s, ", ")
}

// initMode suppresses chatty output from sub-commands called during kai init
var initMode bool

// ANSI color helpers for init output. Respect NO_COLOR and non-TTY stderr.
var stderrColor bool

// stdoutColor reports whether stdout should receive ANSI color. Used by
// commands that print diff/listing output (kai diff, etc.) where the caller
// often pipes into less/grep and wants plain text in that case.
var stdoutColor bool

func initColors() {
	if os.Getenv("NO_COLOR") != "" {
		return
	}
	if fi, err := os.Stderr.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		stderrColor = true
	}
	if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		stdoutColor = true
	}
}

func cBold(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

func cGreen(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[32m" + s + "\033[0m"
}

func cRed(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[31m" + s + "\033[0m"
}

func cDim(s string) string {
	if !stderrColor {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

// spinner runs an animated gradient pulse on stderr while work happens.
// A soft glow bounces across a short bar — subtle, not flashy.
//
//	stop := spinner("Building semantic graph")
//	err := doWork()
//	stop(err)
func spinner(label string) func(error) {
	const barWidth = 16
	const pulseHalf = 3

	// Soft blue gradient — visible but not loud
	pulseColors := []string{"240", "246", "74", "81", "74", "246"}
	dimColor := "238"

	iabs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}

	renderBar := func(pos int) string {
		var b strings.Builder
		for i := 0; i < barWidth; i++ {
			dist := iabs(i - pos)
			if dist > pulseHalf {
				if stderrColor {
					fmt.Fprintf(&b, "\033[38;5;%sm─\033[0m", dimColor)
				} else {
					b.WriteRune('─')
				}
			} else {
				lvl := pulseHalf - dist
				idx := lvl * (len(pulseColors) - 1) / pulseHalf
				if stderrColor {
					fmt.Fprintf(&b, "\033[38;5;%sm─\033[0m", pulseColors[idx])
				} else {
					b.WriteRune('━')
				}
			}
		}
		return b.String()
	}

	// Render first frame immediately so it's visible even for fast tasks
	fmt.Fprintf(os.Stderr, "\r  %s %s\033[K", renderBar(0), label)

	done := make(chan struct{})
	go func() {
		pos := 1
		dir := 1
		for {
			select {
			case <-done:
				return
			default:
				time.Sleep(45 * time.Millisecond)
				fmt.Fprintf(os.Stderr, "\r  %s %s\033[K", renderBar(pos), label)
				pos += dir
				if pos >= barWidth-1 {
					dir = -1
				} else if pos <= 0 {
					dir = 1
				}
			}
		}
	}()

	return func(err error) {
		close(done)
		time.Sleep(50 * time.Millisecond)

		if err != nil {
			errMsg := err.Error()
			if len(errMsg) > 60 {
				errMsg = errMsg[:57] + "..."
			}
			fmt.Fprintf(os.Stderr, "\r  %s %s %s\033[K\n", cRed("✗"), label, cDim(errMsg))
		} else {
			fmt.Fprintf(os.Stderr, "\r  %s %s\033[K\n", cGreen("✓"), label)
		}
	}
}

// personalSlug derives the personal-org slug from an email's local-part
// (lowercased, dots → hyphens) — the same derivation the server uses when it
// auto-creates a user's personal org.
func personalSlug(email string) string {
	if idx := strings.Index(email, "@"); idx > 0 {
		return strings.ToLower(strings.ReplaceAll(email[:idx], ".", "-"))
	}
	return ""
}

// pickPersonalOrg returns the caller's personal org (slug == personalSlug of
// the logged-in email), falling back to the first org. Used for
// non-interactive init so it never has to ask which org to use.
func pickPersonalOrg(orgs []remote.OrgInfo, email string) *remote.OrgInfo {
	if slug := personalSlug(email); slug != "" {
		for i := range orgs {
			if orgs[i].Slug == slug {
				return &orgs[i]
			}
		}
	}
	if len(orgs) > 0 {
		return &orgs[0]
	}
	return nil
}

func runInit(cmd *cobra.Command, args []string) error {
	initColors()
	te := telemetry.NewEvent("init")
	defer te.Finish()

	// Show explain if requested
	if initExplain {
		cwd, _ := os.Getwd()
		ctx := explain.ExplainInit(cwd)
		ctx.Print(os.Stdout)
	}

	// Container-vs-project guard: if cwd already has a
	// kai.projects.yaml, it's claiming to be a CONTAINER of projects
	// below it. Initializing a .kai/ here would make the directory
	// claim to be both — the misconfig that produces silent
	// cross-DB mismatches downstream (May 2026: orchestrator wrote
	// to one .kai/, queried another, surfaced "no such table: refs"
	// three error layers down). Refuse at the door rather than let
	// the broken state get created in the first place. --force opts
	// back in for users who actually want both (rare; we don't gate
	// the existing escape hatch).
	cwd, _ := os.Getwd()
	if !initForce {
		yamlPath := filepath.Join(cwd, projects.ProjectsFileName)
		if _, err := os.Stat(yamlPath); err == nil {
			fmt.Fprintf(os.Stderr,
				"%s refusing to init: %s already exists here.\n"+
					"  This directory is configured as a CONTAINER of projects (sub-repos listed in the yaml),\n"+
					"  not a project itself. Initializing a .kai/ here would create a misconfig that fails at\n"+
					"  integrate time with cryptic SQL errors.\n\n"+
					"  Two ways forward:\n"+
					"    1. cd into one of the sub-projects listed in %s and run kai init there.\n"+
					"    2. If this dir really IS the project, delete %s first, or use %s.\n",
				cRed("✗"), yamlPath, projects.ProjectsFileName, projects.ProjectsFileName,
				cBold("kai init --force"))
			return fmt.Errorf("refusing to init: %s exists at cwd", projects.ProjectsFileName)
		}
	}

	// Already-initialized short-circuit. The presence of the kai
	// db.sqlite is the durable signal: every code path that fully
	// succeeds creates it (capture-on-init, clone, restore). If
	// it's there, the heavy steps below — git history import,
	// `runCapture`, MCP wiring, the interactive remote setup, the
	// auto `runPush` — would all redo work the user didn't ask
	// for. `--force` opts back into the full path for users who
	// genuinely want a re-init (e.g. after wiping .kai by hand).
	dbPath := filepath.Join(kaiDir, dbFile)
	if !initForce {
		if _, err := os.Stat(dbPath); err == nil {
			fmt.Fprintf(os.Stderr,
				"%s kai is already initialized in this directory (%s).\n"+
					"  Use %s to re-snapshot the workspace, %s to upload it, or %s to force a full re-init.\n",
				cGreen("✓"), kaiDir,
				cBold("kai capture"), cBold("kai push"), cBold("kai init --force"))
			return nil
		}
	}

	// Create the kai data directory
	debugf("creating kai data directory at %s", kaiDir)
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		return fmt.Errorf("creating kai data directory: %w", err)
	}

	// Maintain a .gitignore entry only when the data dir lives in the
	// worktree (.kai); the .git/kai layout is auto-ignored by git so we
	// skip touching .gitignore there.
	if kaipath.NeedsGitignore(kaiDir) {
		ensureGitignore(filepath.Base(kaiDir))
	}

	// --git-bridge: mark bridge enabled before runHookInstall runs so the
	// post-commit hook gets written. Requires .git/ to actually exist.
	if initGitBridge {
		if _, err := os.Stat(".git"); err != nil {
			return fmt.Errorf("--git-bridge requires a git repository (no .git directory)")
		}
		sentinel := filepath.Join(kaiDir, "bridge-enabled")
		if err := os.WriteFile(sentinel, []byte("1\n"), 0644); err != nil {
			return fmt.Errorf("enabling git bridge: %w", err)
		}
	}

	// Create objects directory
	objPath := filepath.Join(kaiDir, objectsDir)
	if err := os.MkdirAll(objPath, 0755); err != nil {
		return fmt.Errorf("creating objects directory: %w", err)
	}
	debugf("database path: %s", filepath.Join(kaiDir, dbFile))

	// Write default kai.modules.yaml in project root (not in .kai) only if it doesn't exist
	// This file is meant to be committed to version control and shared with the team
	// Skip during clone since the remote repo may have its own modules file
	if !skipModulesFile {
		if _, err := os.Stat(modulesFile); os.IsNotExist(err) {
			modulesContent := `# Kai module definitions
# This file maps file paths to logical modules for better intent generation.
# Commit this file to version control to share with your team.
#
# Example:
#   modules:
#     - name: Auth
#       paths:
#         - src/auth/**
#         - lib/session.js
#     - name: API
#       paths:
#         - src/routes/**
#         - src/controllers/**

modules: []
`
			if err := os.WriteFile(modulesFile, []byte(modulesContent), 0644); err != nil {
				return fmt.Errorf("writing %s: %w", modulesFile, err)
			}
		}
	}

	// Write AI agent guide in .kai directory
	agentGuideFile := filepath.Join(kaiDir, "AGENTS.md")
	if _, err := os.Stat(agentGuideFile); os.IsNotExist(err) {
		agentGuide := `# Kai - AI Agent Guide

This project uses **Kai**, a semantic version control system. Unlike Git which tracks
line-by-line text changes, Kai understands code at a semantic level—identifying
functions, classes, variables, and how they relate to your project's architecture.

## What is Kai?

Kai is NOT a replacement for Git. It works alongside Git (or standalone) to provide:
- **Semantic snapshots** - Captures what your code means, not just what it looks like
- **Change classification** - Automatically detects if you added a function, changed a condition, etc.
- **Intent generation** - Creates human-readable summaries like "Update Auth login timeout"

Think of it as: Git tracks "line 42 changed from X to Y", Kai tracks "the login function's timeout was reduced from 1 hour to 30 minutes".

## 2-Minute Quick Start (Recommended)

Get Kai's value in 6 simple commands:

` + "```" + `bash
# 1. Initialize Kai
kai init

# 2. Scan your project (snapshot + analyze in one step)
kai capture

# 3. Make changes to your code...

# 4. See what changed semantically
kai diff

# 5. Open a review
kai review open --title "Fix bug"

# 6. Preview CI impact
kai ci plan --explain

# 7. Complete the review
kai review view <id>        # View review details
kai review approve <id>     # Approve the review
kai review close <id> --state merged
` + "```" + `

That's it! You now have semantic diffs, change classification, and selective CI.

## The Core Commands

| Command | What it does |
|---------|-------------|
| ` + "`" + `kai capture` + "`" + ` | Snapshot + analyze in one step (recommended) |
| ` + "`" + `kai diff` + "`" + ` | Show semantic differences |
| ` + "`" + `kai review open` + "`" + ` | Create a code review |
| ` + "`" + `kai review view <id>` + "`" + ` | View review details |
| ` + "`" + `kai review approve <id>` + "`" + ` | Approve a review |
| ` + "`" + `kai review close <id>` + "`" + ` | Close review (--state merged\|abandoned) |
| ` + "`" + `kai ci plan` + "`" + ` | Compute affected tests |

## Getting Started (Detailed)

If you want more control, here's the step-by-step approach:

### Step 1: Initialize Kai
` + "```" + `bash
kai init
` + "```" + `
This creates a ` + "`" + `.kai/` + "`" + ` directory with the database and object storage.

### Step 2: Create a Snapshot
` + "```" + `bash
# From directory (recommended)
kai snap .

# Or from Git branch/tag/commit
kai snapshot create --git main
` + "```" + `

### Step 3: Make Changes and Diff
After modifying code:
` + "```" + `bash
kai capture                    # Re-capture with changes
kai diff                    # See semantic differences
` + "```" + `

### Step 4: Review and CI
` + "```" + `bash
kai review open --title "Fix login bug"
kai ci plan --explain       # See what tests to run
` + "```" + `

### Step 5: Complete the Review
` + "```" + `bash
kai review view <id>        # View the review details
kai review approve <id>     # Approve the review
kai review close <id> --state merged  # Close as merged
` + "```" + `

## Quick Reference

### Check Status
` + "```" + `bash
kai status                    # Show pending changes since last snapshot
` + "```" + `

### References (avoid typing long hashes)
- ` + "`" + `@snap:last` + "`" + ` - Most recent snapshot
- ` + "`" + `@snap:prev` + "`" + ` - Previous snapshot
- ` + "`" + `@cs:last` + "`" + ` - Most recent changeset
- ` + "`" + `snap.main` + "`" + `, ` + "`" + `cs.feature` + "`" + ` - Named refs (create with ` + "`" + `kai ref set snap.main @snap:last` + "`" + `)

### Remote Operations (sync with server)
` + "```" + `bash
kai clone http://server/org/repo           # Clone a repository (creates directory)
kai clone http://server/org/repo mydir     # Clone into specific directory
kai remote set origin https://kailab.example.com --tenant myorg --repo myproject
kai auth login                # Authenticate
kai push origin snap.latest   # Upload to server
kai fetch origin              # Download from server
` + "```" + `

## Key Concepts

| Concept | What it is | Analogy |
|---------|------------|---------|
| **Snapshot** | Semantic capture of codebase | Like a Git commit, but understands code structure |
| **ChangeSet** | Diff between two snapshots | Like ` + "`" + `git diff` + "`" + `, but classifies change types |
| **Intent** | Human summary of changes | Like a commit message, but auto-generated |
| **Module** | Logical file grouping | Like folders, but by feature (Auth, Billing, etc.) |

## Change Types Kai Detects

| Type | What it means | Example |
|------|---------------|---------|
| ` + "`" + `FUNCTION_ADDED` + "`" + ` | New function created | Added ` + "`" + `validateToken()` + "`" + ` |
| ` + "`" + `FUNCTION_REMOVED` + "`" + ` | Function deleted | Removed ` + "`" + `legacyAuth()` + "`" + ` |
| ` + "`" + `CONDITION_CHANGED` + "`" + ` | If/comparison changed | ` + "`" + `if (x > 100)` + "`" + ` → ` + "`" + `if (x > 50)` + "`" + ` |
| ` + "`" + `CONSTANT_UPDATED` + "`" + ` | Literal value changed | ` + "`" + `TIMEOUT = 3600` + "`" + ` → ` + "`" + `1800` + "`" + ` |
| ` + "`" + `API_SURFACE_CHANGED` + "`" + ` | Function signature changed | Added parameter to function |
| ` + "`" + `FILE_ADDED` + "`" + ` | New file created | Added ` + "`" + `auth/mfa.ts` + "`" + ` |
| ` + "`" + `FILE_DELETED` + "`" + ` | File removed | Deleted ` + "`" + `deprecated/old.ts` + "`" + ` |

## Common Tasks

### "I want to see what changed in my code"
` + "```" + `bash
# Semantic diff (default) - shows function/class/variable changes
kai diff
# Output shows:
#   ~ auth/login.ts
#     ~ function login(user) -> login(user, token)
#     + function validateMFA(code)
#   Summary: 1 file, 2 units changed

# Line-level diff like git (with colors)
kai diff -p

# Just file paths
kai diff --name-only

# JSON output for programmatic use
kai diff --json
` + "```" + `

### "I want a git-style line diff"
` + "```" + `bash
kai diff -p
# Output shows:
#   diff --kai a/src/auth.ts b/src/auth.ts
#   --- a/src/auth.ts
#   +++ b/src/auth.ts
#   @@ -42 +42 @@
#   -  const timeout = 3600;
#   +  const timeout = 1800;
` + "```" + `

### "I want to compare two Git branches"
` + "```" + `bash
# Must use explicit --git flag (no positional args)
kai snapshot create --git main
kai snapshot create --git feature-branch
kai analyze symbols @snap:prev
kai analyze symbols @snap:last
kai changeset create @snap:prev @snap:last
kai intent render @cs:last
` + "```" + `

### "I want to save a named reference"
` + "```" + `bash
kai ref set snap.main @snap:last      # Name the current snapshot
kai ref set snap.v1.0 abc123          # Name by ID
kai ref list                          # See all refs
` + "```" + `

## Accessing Raw Diffs (Ground Truth)

Kai provides semantic understanding, but raw Git diffs remain the authoritative source when you need to verify accuracy.

### When to Check the Raw Diff

| Scenario | Why Raw Diff Helps |
|----------|-------------------|
| **Ground truth** | If Kai miscategorizes a change (e.g., labels logic as "formatting"), the raw diff is authoritative |
| **Context visibility** | See actual code around changes, not just symbol names |
| **Uncategorized changes** | Catch formatting normalization, semicolon fixes—usually noise, but sometimes meaningful (e.g., ASI bugs in JS) |
| **Verification** | Confirm the structured summary matches what actually changed |

### Getting Both Views

` + "```" + `bash
# Kai's structured semantic analysis
kai dump @cs:last --json

# Raw Git diff for the same changes (ground truth)
git diff HEAD~1..HEAD

# Or use Kai's diff with --raw flag
kai diff @snap:prev @snap:last --raw
` + "```" + `

### Recommended Workflow

1. **Start with Kai** — Use structured data for speed and semantic understanding
2. **Verify when needed** — Check raw diff if something seems miscategorized
3. **Trust the diff** — If Kai and raw diff disagree, the diff is correct

## CI & Test Selection

Kai provides intelligent test selection for CI pipelines. Instead of running all tests on every change, analyze which tests are affected.

### Generate a Test Plan

` + "```" + `bash
# Generate test selection plan from a changeset
kai ci plan @cs:last --out plan.json

# Human-readable explanation
kai ci plan @cs:last --explain

# Force full suite (panic switch)
KAI_FORCE_FULL=1 kai ci plan @cs:last --out plan.json
` + "```" + `

### Safety Modes

| Mode | Description |
|------|-------------|
| ` + "`" + `shadow` + "`" + ` | Compute plan but run full suite. Compare predictions to learn. |
| ` + "`" + `guarded` + "`" + ` | Run selective with auto-fallback on risk. Default mode. |
| ` + "`" + `strict` + "`" + ` | Run selective only. Use panic switch for full suite. |

` + "```" + `bash
kai ci plan @cs:last --safety-mode=shadow   # Learning phase
kai ci plan @cs:last --safety-mode=guarded  # Safe default
kai ci plan @cs:last --safety-mode=strict   # High confidence
` + "```" + `

### Find Affected Tests

` + "```" + `bash
# Which tests are affected by changes between two snapshots?
kai test affected @snap:prev @snap:last

# Uses import graph tracing to find transitive dependencies
` + "```" + `

### Structural Risks Detected

| Risk | Severity | Meaning |
|------|----------|---------|
| ` + "`" + `config_change` + "`" + ` | High | package.json, tsconfig, etc. changed |
| ` + "`" + `test_infra` + "`" + ` | High | Fixtures, mocks, setup files changed |
| ` + "`" + `dynamic_import` + "`" + ` | High | Dynamic require/import detected |
| ` + "`" + `no_test_mapping` + "`" + ` | Medium | Changed files have no test coverage |
| ` + "`" + `cross_module_change` + "`" + ` | Medium | Changes span 3+ modules |

## Working Snapshot Model

Kai uses a two-tier snapshot model to prevent database bloat:

| Ref | Purpose | GC Root? |
|-----|---------|----------|
| ` + "`" + `@snap:working` + "`" + ` | Current working directory state | No (ephemeral) |
| ` + "`" + `@snap:last` + "`" + ` | Last committed baseline | No (ephemeral) |
| ` + "`" + `snap.main` + "`" + `, etc. | Named refs you create | Yes (permanent) |

### How it works:

1. **` + "`" + `kai capture` + "`" + `** creates a snapshot and updates ` + "`" + `snap.latest` + "`" + `
2. **` + "`" + `kai status` + "`" + `** compares working directory to ` + "`" + `snap.latest` + "`" + `
3. **` + "`" + `kai review open` + "`" + `** creates a changeset for review
4. **` + "`" + `kai prune` + "`" + `** cleans up unreferenced snapshots

### Garbage Collection

` + "```" + `bash
kai prune              # Dry-run (shows what would be deleted)
kai prune --yes        # Actually delete unreachable content
kai prune --since 7    # Only delete content older than 7 days
kai prune --keep "src/**"  # Preserve files matching pattern
` + "```" + `

**What stays alive:**
- Snapshots referenced by workspaces
- Snapshots referenced by reviews
- Snapshots with named refs (` + "`" + `snap.main` + "`" + `, etc.)

**What gets cleaned up:**
- Old ` + "`" + `@snap:working` + "`" + ` snapshots (replaced by newer ones)
- Orphaned changesets with no review

## Troubleshooting

| Error | Fix |
|-------|-----|
| "Kai not initialized" | Run ` + "`" + `kai init` + "`" + ` first |
| "No snapshots found" | Create one with ` + "`" + `kai snapshot create --dir .` + "`" + ` |
| "ambiguous prefix" | Use more characters of the ID, or use ` + "`" + `@snap:last` + "`" + ` |
| "Last capture was X minutes ago" | Run ` + "`" + `kai capture` + "`" + ` or use ` + "`" + `--force` + "`" + ` |

## More Information

Run ` + "`" + `kai --help` + "`" + ` or ` + "`" + `kai <command> --help` + "`" + ` for detailed usage.
`
		if err := os.WriteFile(agentGuideFile, []byte(agentGuide), 0644); err != nil {
			return fmt.Errorf("writing AGENTS.md: %w", err)
		}
	}

	// Write AI coding tool context files (CLAUDE.md, .github/copilot-instructions.md, etc.)
	writeAIContextFiles()

	// Open database and apply schema. dbPath was computed at the
	// top of the function for the already-initialized check.
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	// Set WAL mode outside transaction (SQLite requirement)
	db.Exec("PRAGMA journal_mode=WAL")

	// Apply schema inline (since we may not have the schema file available)
	schema := `
CREATE TABLE IF NOT EXISTS nodes (
  id BLOB PRIMARY KEY,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS nodes_kind ON nodes(kind);

CREATE TABLE IF NOT EXISTS edges (
  src BLOB NOT NULL,
  type TEXT NOT NULL,
  dst BLOB NOT NULL,
  at  BLOB,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (src, type, dst, at)
);

CREATE INDEX IF NOT EXISTS edges_src ON edges(src);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst);
CREATE INDEX IF NOT EXISTS edges_type ON edges(type);
CREATE INDEX IF NOT EXISTS edges_at ON edges(at);

-- Named references (aliases)
CREATE TABLE IF NOT EXISTS refs (
  name TEXT PRIMARY KEY,
  target_id BLOB NOT NULL,
  target_kind TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS refs_kind ON refs(target_kind);

-- Human-readable slugs
CREATE TABLE IF NOT EXISTS slugs (
  target_id BLOB PRIMARY KEY,
  slug TEXT UNIQUE NOT NULL
);

-- Sequence log for navigation
CREATE TABLE IF NOT EXISTS logs (
  kind TEXT NOT NULL,
  seq INTEGER NOT NULL,
  id BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (kind, seq)
);
CREATE INDEX IF NOT EXISTS logs_id ON logs(id);

-- Ref change log for auditability (append-only)
CREATE TABLE IF NOT EXISTS ref_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  old_target BLOB,
  new_target BLOB NOT NULL,
  actor TEXT,
  moved_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ref_log_name ON ref_log(name);
CREATE INDEX IF NOT EXISTS ref_log_moved_at ON ref_log(moved_at);

-- Index for prune --since filtering
CREATE INDEX IF NOT EXISTS nodes_created_at ON nodes(created_at);

-- AI authorship attribution ranges
CREATE TABLE IF NOT EXISTS authorship_ranges (
  snapshot_id BLOB NOT NULL,
  file_path TEXT NOT NULL,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  author_type TEXT NOT NULL,
  agent TEXT DEFAULT '',
  model TEXT DEFAULT '',
  session_id TEXT DEFAULT '',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (snapshot_id, file_path, start_line)
);
CREATE INDEX IF NOT EXISTS authorship_snap ON authorship_ranges(snapshot_id);
CREATE INDEX IF NOT EXISTS authorship_file ON authorship_ranges(snapshot_id, file_path);
`
	// Apply schema in a transaction
	tx, err := db.BeginTx()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		tx.Rollback()
		return fmt.Errorf("applying schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing schema: %w", err)
	}

	initMode = true
	defer func() { initMode = false }()

	reader := bufio.NewReader(os.Stdin)

	// Non-interactive when --yes is passed or stdin isn't a terminal (scripts,
	// pipes, CI). In that mode init never blocks on a prompt: it auto-picks the
	// personal org and links an existing repo, and skips login if not already
	// authenticated.
	nonInteractive := initAssumeYes || !isatty.IsTerminal(os.Stdin.Fd())

	// ── Step 2: Detect git repo ──
	isGitRepo := false
	if _, err := os.Stat(".git"); err == nil {
		isGitRepo = true

		// Auto-import git history. runGitImport caps at importMaxCommits (default 50)
		// so large repos only import the most recent slice.
		if countOut, err := exec.Command("git", "rev-list", "--count", "HEAD").Output(); err == nil {
			if count, _ := strconv.Atoi(strings.TrimSpace(string(countOut))); count > 0 {
				stop := spinner("Importing git history")
				importErr := runGitImport(db)
				stop(importErr)
			}
		}

		// ── Step 3: Install hooks (auto-capture on commit, auto-push on git push) ──
		if err := runHookInstall(cmd, nil); err != nil {
			debugf("hook install: %v", err)
		}
	}

	// ── Install the agent-session ingest hook (Claude Code + Codex) so each
	// session run in this repo updates .kai/loops/ for the Overview/Loops views.
	// Project-local, merge-not-clobber; runs for git and non-git kai repos alike.
	if wd, err := os.Getwd(); err == nil {
		writeAgentHooks(wd)
	}

	// ── Step 4: Run first capture to build the semantic graph ──
	stop := spinner("Building semantic graph")
	captureErr := runCapture(cmd, []string{"."})
	stop(captureErr)

	// --no-remote: stop after the local graph build. Skips signup/login,
	// repo linking, and the automatic push — for users who want kai's
	// local semantics without connecting to kaicontext.com, and for
	// headless/automation flows (e.g. autofix) that only need the graph
	// and must never block on auth or reach the network.
	if initNoRemote {
		printInitFinish(isGitRepo)
		return nil
	}

	// ── Step 5: Sign up / log in to kaicontext.com ──
	serverURL := os.Getenv("KAI_SERVER")
	if serverURL == "" {
		serverURL = remote.DefaultServer
	}

	token, authErr := remote.GetValidAccessToken()
	if authErr != nil || token == "" {
		// Resolve the email to sign up / log in with. Interactive runs prompt
		// for it; non-interactive runs (pipes, CI, --yes) take it from --email
		// or KAI_INIT_EMAIL so automation can authenticate without a TTY. With
		// no email available non-interactively we skip login rather than block.
		var email string
		if nonInteractive {
			email = initEmail
			if email == "" {
				email = strings.TrimSpace(os.Getenv("KAI_INIT_EMAIL"))
			}
			if email == "" {
				fmt.Fprintf(os.Stderr, "  Not connected to kaicontext.com — skipping (run %s to connect).\n", cBold("kai auth login"))
				printInitFinish(isGitRepo)
				return nil
			}
		} else {
			fmt.Println()
			fmt.Fprintf(os.Stderr, "  Connect to kaicontext.com %s. Enter your email %s: ", cDim("(free)"), cDim("(or press Enter to skip)"))
			input, _ := reader.ReadString('\n')
			email = strings.TrimSpace(input)
			if email == "" {
				fmt.Fprintf(os.Stderr, "  Skipped. Run %s later to connect.\n", cBold("kai auth login"))
				printInitFinish(isGitRepo)
				return nil
			}
		}

		// Use source=cli to skip the approval gate
		authClient := remote.NewAuthClient(serverURL)
		fmt.Fprintf(os.Stderr, "  Sending login link to %s...\n", email)
		mlResult, err := authClient.SendMagicLinkWithSource(email, "cli")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", cRed("Failed"), err)
			fmt.Fprintf(os.Stderr, "  You can try again later with: %s\n", cBold("kai auth login"))
			printInitFinish(isGitRepo)
			return nil
		}

		var magicToken string
		if mlResult.DevToken != "" {
			magicToken = mlResult.DevToken
		} else if nonInteractive {
			// Production has no dev token and there's no TTY to paste one into,
			// so login can't complete here. Leave a clear breadcrumb to finish.
			fmt.Fprintf(os.Stderr, "  Login link sent to %s — run %s after clicking it to finish connecting.\n", email, cBold("kai auth login"))
			printInitFinish(isGitRepo)
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "  Check your email for a login link %s.\n", cDim("(from support@kaicontext.com)"))
			fmt.Fprint(os.Stderr, "  Paste the token here: ")
			tokenInput, _ := reader.ReadString('\n')
			tokenInput = strings.TrimSpace(tokenInput)
			if strings.Contains(tokenInput, "token=") {
				parts := strings.Split(tokenInput, "token=")
				if len(parts) > 1 {
					magicToken = strings.Split(parts[1], "&")[0]
				}
			} else {
				magicToken = tokenInput
			}
		}

		if magicToken == "" {
			fmt.Fprintf(os.Stderr, "  Skipped. Run %s later to connect.\n", cBold("kai auth login"))
			printInitFinish(isGitRepo)
			return nil
		}

		tokens, err := authClient.ExchangeToken(magicToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", cRed("Login failed"), err)
			printInitFinish(isGitRepo)
			return nil
		}

		creds := &remote.Credentials{
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			Email:        email,
			ExpiresAt:    time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix(),
			ServerURL:    serverURL,
		}
		if err := remote.SaveCredentials(creds); err != nil {
			fmt.Fprintf(os.Stderr, "  %s could not save credentials: %v\n", cRed("Warning:"), err)
		}
		fmt.Fprintf(os.Stderr, "  %s Logged in as %s\n", cGreen("✓"), email)
	} else {
		email, _, _ := remote.GetAuthStatus()
		debugf("already logged in as %s", email)
	}

	// Ensure personal org exists, create repo, offer to push
	token, authErr = remote.GetValidAccessToken()
	if authErr == nil && token != "" {
		ctrl := remote.NewControlClient(serverURL)
		orgs, err := ctrl.ListOrgs()

		var selectedOrg *remote.OrgInfo

		if err == nil {
			if len(orgs) == 0 {
				// Safety net — server normally auto-creates a personal org.
				// Derive the slug from the email local-part silently.
				creds, _ := remote.LoadCredentials()
				slug := "my-org"
				if creds != nil && creds.Email != "" {
					if s := personalSlug(creds.Email); s != "" {
						slug = s
					}
				}
				newOrg, err := ctrl.CreateOrg(slug, slug)
				if err != nil {
					debugf("could not create org: %v", err)
				} else {
					selectedOrg = newOrg
					debugf("created org: %s", selectedOrg.Slug)
				}
			} else if len(orgs) == 1 {
				selectedOrg = &orgs[0]
				debugf("using org: %s", selectedOrg.Slug)
			} else if want := initOrgOverride(); want != "" {
				selectedOrg = findOrgBySlug(orgs, want)
				if selectedOrg == nil {
					return fmt.Errorf("org %q not found — you belong to: %s", want, orgSlugs(orgs))
				}
				debugf("using org from --org/KAI_ORG: %s", selectedOrg.Slug)
			} else {
				// kai init never prompts for the org: default to the personal
				// org (slug derived from the login email). Override with
				// --org <slug> or KAI_ORG. (A blocking org menu here left
				// repos half-initialized when interrupted.)
				authEmail, _, _ := remote.GetAuthStatus()
				selectedOrg = pickPersonalOrg(orgs, authEmail)
				if selectedOrg != nil {
					fmt.Fprintf(os.Stderr, "  Using org %s (override with --org).\n", selectedOrg.Slug)
				}
			}
		}

		// Create or select repo for current directory
		if selectedOrg != nil {
			projectName := remote.DetectProjectName()

			// Check if a repo with this name already exists
			repoExists := false
			if repos, listErr := ctrl.ListRepos(selectedOrg.Slug); listErr == nil {
				for _, r := range repos {
					if r.Name == projectName {
						repoExists = true
						break
					}
				}
			}

			if repoExists {
				// Never prompt: link to the existing repo of this name.
				fmt.Fprintf(os.Stderr, "\n  Linking to existing repo %s/%s.\n", selectedOrg.Slug, cBold(projectName))
				choice := "1"
				switch choice {
				case "1":
					// Link to existing — just set the remote
				case "2":
					fmt.Fprint(os.Stderr, "  New repo name: ")
					newName, _ := reader.ReadString('\n')
					newName = strings.TrimSpace(newName)
					if newName != "" {
						projectName = newName
						if _, createErr := ctrl.CreateRepo(selectedOrg.Slug, projectName, "private"); createErr != nil {
							fmt.Fprintf(os.Stderr, "  %s could not create repo: %v\n", cRed("Warning:"), createErr)
						}
					}
				case "3":
					projectName = ""
				default:
					// Treat as link to existing
				}
			} else {
				_, createErr := ctrl.CreateRepo(selectedOrg.Slug, projectName, "private")
				if createErr != nil {
					debugf("could not create repo: %v", createErr)
				} else {
					fmt.Fprintf(os.Stderr, "Created repo: %s/%s\n", selectedOrg.Slug, projectName)
				}
			}

			if projectName != "" {
				// Set up the remote
				remoteEntry := &remote.RemoteEntry{
					URL:    serverURL,
					Tenant: selectedOrg.Slug,
					Repo:   projectName,
				}
				if err := remote.SetRemote("origin", remoteEntry); err != nil {
					debugf("setting remote: %v", err)
				}

				// Push the semantic graph automatically
				stop := spinner("Pushing to kaicontext.com")
				pushErr := runPush(cmd, []string{"origin"})
				stop(pushErr)
			}
		}
	}

	printInitFinish(isGitRepo)
	return nil
}

// printInitFinish prints the final success message.
// autoAttributeFromMCPSession checks if an MCP session was active when this
// capture was triggered (e.g., AI agent made edits, then git commit hook ran).
// If so, auto-creates authorship checkpoints for changed files.
func autoAttributeFromMCPSession(kaiDir, workDir string) {
	// Check all MCP session files (one per PID, supports multiple Claude windows)
	entries, err := os.ReadDir(kaiDir)
	if err != nil {
		return
	}

	var session map[string]interface{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "mcp-session-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(kaiDir, e.Name()))
		if err != nil {
			continue
		}
		var s map[string]interface{}
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		// Use the most recently updated session
		if session == nil {
			session = s
		} else {
			sUpdated, _ := s["updatedAt"].(float64)
			curUpdated, _ := session["updatedAt"].(float64)
			if sUpdated > curUpdated {
				session = s
			}
		}
	}
	// Also check legacy single-file format
	if session == nil {
		data, err := os.ReadFile(filepath.Join(kaiDir, "mcp-session.json"))
		if err != nil {
			return
		}
		if json.Unmarshal(data, &session) != nil {
			return
		}
	}
	if session == nil {
		return
	}

	// Check if session was updated recently (within 5 minutes)
	updatedAt, ok := session["updatedAt"].(float64)
	if !ok {
		return
	}
	age := time.Since(time.UnixMilli(int64(updatedAt)))
	if age > 5*time.Minute {
		debugf("MCP session too old (%v), skipping auto-attribution", age)
		return
	}

	// Check if the MCP process is still running
	if pid, ok := session["pid"].(float64); ok {
		process, err := os.FindProcess(int(pid))
		if err != nil || process.Signal(syscall.Signal(0)) != nil {
			debugf("MCP session process not running, skipping auto-attribution")
			return
		}
	}

	agentName, _ := session["agent"].(string)
	if agentName == "" {
		agentName = "ai-agent"
	}
	modelName, _ := session["model"].(string)
	sessionID, _ := session["sessionId"].(string)

	// Get changed files from git (staged first, then unstaged-vs-HEAD).
	changedOutput, err := exec.Command("git", "-C", workDir, "diff", "--cached", "--name-only").Output()
	if err != nil || strings.TrimSpace(string(changedOutput)) == "" {
		changedOutput, err = exec.Command("git", "-C", workDir, "diff", "--name-only", "HEAD").Output()
		if err != nil {
			return
		}
	}

	changed := strings.TrimSpace(string(changedOutput))
	if changed == "" {
		return
	}

	files := strings.Split(changed, "\n")
	debugf("Auto-attributing %d files to AI agent %s (session %s)", len(files), agentName, sessionID)

	writer := authorship.NewCheckpointWriter(kaiDir, sessionID)
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		// Read current working-copy content.
		newContent, err := os.ReadFile(filepath.Join(workDir, file))
		if err != nil {
			continue
		}

		// Read previous content from git HEAD. If the file is new (not in HEAD),
		// oldContent is nil and DiffLineRanges will attribute the whole thing.
		var oldContent []byte
		oldBytes, gitErr := exec.Command("git", "-C", workDir, "show", "HEAD:"+file).Output()
		if gitErr == nil {
			oldContent = oldBytes
		}

		ranges := authorship.DiffLineRanges(oldContent, newContent)
		if len(ranges) == 0 {
			// No meaningful change (e.g., pure deletions). Nothing to attribute.
			continue
		}

		for _, r := range ranges {
			writer.Write(authorship.CheckpointRecord{
				File:       file,
				StartLine:  r.Start,
				EndLine:    r.End,
				Action:     "modify",
				AuthorType: "ai",
				Agent:      agentName,
				Model:      modelName,
				SessionID:  sessionID,
				Timestamp:  time.Now().UnixMilli(),
			})
		}
	}
}

func printInitFinish(isGitRepo bool) {
	fmt.Fprintf(os.Stderr, "%s %s\n", cGreen("✓"), cBold("Kai initialized"))
	printBYOMHintIfActive()
}

// printBYOMHintIfActive surfaces the active LLM provider when the
// user has set KAI_PROVIDER. Silent for the kailab default — that's
// the assumed path and the line would just be noise. For BYOM
// users, this is the first place they see "yes kai noticed your
// env var" without having to run `kai auth status` separately.
func printBYOMHintIfActive() {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("KAI_PROVIDER")))
	if kind == "" || kind == string(provider.KindKailab) {
		return
	}
	switch kind {
	case string(provider.KindAnthropic):
		fmt.Fprintf(os.Stderr, "  LLM: anthropic (direct) — using ANTHROPIC_API_KEY\n")
		if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
			fmt.Fprintf(os.Stderr, "  %s ANTHROPIC_API_KEY is not set — `kai` will fail at first turn\n", cRed("Warning:"))
		}
	case string(provider.KindOpenAI):
		base := strings.TrimSpace(os.Getenv("KAI_OPENAI_BASE_URL"))
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		fmt.Fprintf(os.Stderr, "  LLM: openai-compatible — %s\n", base)
	default:
		fmt.Fprintf(os.Stderr, "  %s KAI_PROVIDER=%q is not recognized (want kailab|anthropic|openai)\n",
			cRed("Warning:"), kind)
	}
	if _, _, loggedIn := remote.GetAuthStatus(); !loggedIn {
		fmt.Fprintf(os.Stderr, "  Kailab: not connected — sync and team gates unavailable\n")
		fmt.Fprintf(os.Stderr, "          run `kai auth login` to enable\n")
	}
}

// ensureGitignore appends entry to .gitignore if not already present.
func ensureGitignore(entry string) {
	const gitignorePath = ".gitignore"
	content, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	// Check if already ignored
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == entry {
			return
		}
	}
	// Append with a leading newline if file doesn't end with one
	addition := entry + "\n"
	if len(content) > 0 && content[len(content)-1] != '\n' {
		addition = "\n" + addition
	}
	os.WriteFile(gitignorePath, append(content, []byte(addition)...), 0644)
}

const kaiMCPSection = `## Code Analysis

Use Kai MCP tools instead of reading files when you need to know callers, callees, dependencies, dependents, or test coverage for a file. One kai_context call returns this in ~500 tokens; reading the files yourself costs thousands. Do not read files just to discover call relationships or imports — use kai_context or kai_impact instead.

Do not delegate code exploration to subagents — they cannot access Kai MCP tools.
`

// writeAIContextFiles creates context files for AI coding tools (CLAUDE.md,
// .github/copilot-instructions.md, etc.) if they don't already exist.
// If a file exists but doesn't contain the Kai section, it prepends it.
func writeAIContextFiles() {
	kaiMarker := "Kai MCP tools"

	files := []string{
		"CLAUDE.md",
		".github/copilot-instructions.md",
		".cursorrules",
		"CODEX.md",
		"AGENTS.md",
	}

	for _, path := range files {
		existing, err := os.ReadFile(path)
		if err != nil {
			continue // file doesn't exist, skip
		}
		if strings.Contains(string(existing), kaiMarker) {
			continue // already has the section
		}
		// Prepend the Kai section
		updated := kaiMCPSection + "\n" + string(existing)
		if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
			debugf("warning: could not update %s: %v", path, err)
		}
	}
}

// runGitImport replays git history as semantic snapshots.
func runGitImport(db *graph.DB) error {
	// Get commit count
	countOut, err := exec.Command("git", "rev-list", "--count", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("counting commits: %w", err)
	}
	totalCommits, _ := strconv.Atoi(strings.TrimSpace(string(countOut)))

	maxCommits := importMaxCommits
	if totalCommits > maxCommits {
		if !initMode {
			fmt.Printf("  Repository has %d commits. Importing last %d.\n", totalCommits, maxCommits)
			fmt.Printf("  (Use 'kai import --git --all' for full history)\n")
		}
	} else {
		maxCommits = totalCommits
		if !initMode {
			fmt.Printf("  Importing %d commits...\n", maxCommits)
		}
	}

	// Get commit list (oldest first)
	listCmd := exec.Command("git", "rev-list", "--reverse", fmt.Sprintf("--max-count=%d", maxCommits), "HEAD")
	listOut, err := listCmd.Output()
	if err != nil {
		return fmt.Errorf("listing commits: %w", err)
	}
	commits := strings.Split(strings.TrimSpace(string(listOut)), "\n")

	// Save current branch to restore later
	currentBranch, _ := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	currentRef := strings.TrimSpace(string(currentBranch))
	defer func() {
		exec.Command("git", "checkout", currentRef).Run()
	}()

	creator := snapshot.NewCreator(db, nil)
	refMgr := ref.NewRefManager(db)

	for i, commitHash := range commits {
		commitHash = strings.TrimSpace(commitHash)
		if commitHash == "" {
			continue
		}

		// Get commit message
		msgOut, _ := exec.Command("git", "log", "-1", "--format=%s", commitHash).Output()
		msg := strings.TrimSpace(string(msgOut))

		if !initMode {
			fmt.Fprintf(os.Stderr, "\r  [%d/%d] %s %.7s", i+1, len(commits), msg, commitHash)
		}

		// Checkout this commit
		if err := exec.Command("git", "checkout", "--quiet", commitHash).Run(); err != nil {
			continue
		}

		// Create snapshot (no analysis — too slow for bulk import)
		source, serr := dirio.OpenDirectory(".")
		if serr != nil {
			continue
		}
		snapshotID, err := creator.CreateSnapshot(source)
		if err != nil {
			continue
		}

		// Update refs
		autoRefMgr := ref.NewAutoRefManager(db)
		autoRefMgr.OnSnapshotCreated(snapshotID)

		// Store commit message for push
		os.MkdirAll(kaiDir, 0755)
		os.WriteFile(filepath.Join(kaiDir, "message"), []byte(msg), 0644)

		_ = refMgr
	}

	if !initMode {
		fmt.Fprintf(os.Stderr, "\r\033[K")
		fmt.Printf("  ✓ Imported %d commits as snapshots\n", len(commits))
		fmt.Println("  Run 'kai capture' to add full semantic analysis to the current snapshot.")
	}

	return nil
}

// detectCIPlatform checks the git remote URL to determine GitHub vs GitLab.
func detectCIPlatform() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	if strings.Contains(url, "github.com") {
		return "GitHub"
	}
	if strings.Contains(url, "gitlab") {
		return "GitLab"
	}
	return ""
}

// generateCIConfig creates a CI workflow file for GitHub Actions or GitLab CI.
func generateCIConfig(platform string) error {
	switch platform {
	case "GitHub":
		return generateGitHubActions()
	case "GitLab":
		return generateGitLabCI()
	default:
		return fmt.Errorf("unsupported platform: %s", platform)
	}
}

func generateGitHubActions() error {
	dir := ".github/workflows"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	path := filepath.Join(dir, "kai-sync.yml")
	if _, err := os.Stat(path); err == nil {
		fmt.Println("  .github/workflows/kai-sync.yml already exists, skipping.")
		return nil
	}

	config := `# Kai Sync — keeps semantic graph up to date on every push
# Requires KAI_TOKEN secret (get from: kai auth token)
name: Kai Sync
on:
  push:
    branches: [main, master]

jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Install Kai
        run: curl -sSL https://get.kaicontext.com | sh
      - name: Capture and push
        env:
          KAI_TOKEN: ${{ secrets.KAI_TOKEN }}
        run: |
          kai auth login --token "$KAI_TOKEN"
          kai capture -m "${{ github.event.head_commit.message }}"
          kai push
`
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		return err
	}

	fmt.Println("  ✓ Created .github/workflows/kai-sync.yml")
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Println("    1. Run: kai auth token")
	fmt.Println("    2. Add the token as a GitHub secret named KAI_TOKEN")
	fmt.Println("       Settings → Secrets → Actions → New repository secret")
	fmt.Println("    3. Commit and push the workflow file")
	return nil
}

func generateGitLabCI() error {
	path := ".kai-sync.gitlab-ci.yml"

	// Check if .gitlab-ci.yml already has kai sync
	if existing, err := os.ReadFile(".gitlab-ci.yml"); err == nil {
		if strings.Contains(string(existing), "kai-sync") || strings.Contains(string(existing), "kai capture") {
			fmt.Println("  .gitlab-ci.yml already contains kai sync, skipping.")
			return nil
		}
	}

	config := `# Kai Sync — keeps semantic graph up to date on every push
# Include this in your .gitlab-ci.yml:
#   include:
#     - local: .kai-sync.gitlab-ci.yml
#
# Requires KAI_TOKEN CI variable (get from: kai auth token)
# Settings → CI/CD → Variables → Add variable

kai-sync:
  stage: .post
  image: alpine:3.19
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
  script:
    - apk add --no-cache curl
    - curl -sSL https://get.kaicontext.com | sh
    - kai auth login --token "$KAI_TOKEN"
    - kai capture -m "$CI_COMMIT_MESSAGE"
    - kai push
`
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		return err
	}

	fmt.Println("  ✓ Created .kai-sync.gitlab-ci.yml")
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Println("    1. Add to your .gitlab-ci.yml:")
	fmt.Println("       include:")
	fmt.Println("         - local: .kai-sync.gitlab-ci.yml")
	fmt.Println("    2. Run: kai auth token")
	fmt.Println("    3. Add the token as a GitLab CI variable named KAI_TOKEN")
	fmt.Println("       Settings → CI/CD → Variables → Add variable")
	fmt.Println("    4. Commit and push both files")
	return nil
}

// installGitHook installs a post-commit hook that auto-captures.
func installGitHook() error {
	hookDir := ".git/hooks"
	hookPath := filepath.Join(hookDir, "post-commit")

	// Don't overwrite existing hooks
	if _, err := os.Stat(hookPath); err == nil {
		// Append to existing hook
		existing, _ := os.ReadFile(hookPath)
		if strings.Contains(string(existing), "kai capture") {
			return nil // already installed
		}
		f, err := os.OpenFile(hookPath, os.O_APPEND|os.O_WRONLY, 0755)
		if err != nil {
			return err
		}
		defer f.Close()
		f.WriteString("\n# Kai auto-capture\nkai capture -m \"$(git log -1 --format=%s)\" 2>/dev/null &\n")
		return nil
	}

	// Create new hook
	hook := `#!/bin/sh
# Kai auto-capture — runs after each git commit
# Captures a semantic snapshot with the commit message
kai capture -m "$(git log -1 --format=%s)" 2>/dev/null &
`
	if err := os.WriteFile(hookPath, []byte(hook), 0755); err != nil {
		return err
	}
	return nil
}

func runSnap(cmd *cobra.Command, args []string) error {
	// Set dirPath from argument or default to current directory
	if len(args) > 0 {
		dirPath = args[0]
	} else {
		dirPath = "."
	}
	// Delegate to runSnapshot which handles dirPath mode
	return runSnapshot(cmd, args)
}

// runCapture is the "2-minute value" macro command that performs:
// 1. Snapshot the directory
// 2. Analyze symbols
// 3. Analyze calls (build call graph)
// This is the recommended starting point for new users.
// captureTimingEnabled and captureTimingPrint mirror the snapshot
// package's instrumentation — same KAI_CAPTURE_TIMING=1 gate. We
// can't import the snapshot package's helper into main.go cleanly,
// so we duplicate the 8-line pattern. Phase numbers 10+ live here
// to distinguish them from snapshot.Analyze phases (00-06).
var captureTimingEnabled = os.Getenv("KAI_CAPTURE_TIMING") == "1"

func captureTimingPrint(phase string, start time.Time) {
	if !captureTimingEnabled {
		return
	}
	fmt.Fprintf(os.Stderr, "[capture-timing] %-28s %s\n", phase, time.Since(start).Round(time.Millisecond))
}

// acquireCaptureLock prevents two `kai capture` runs from racing on
// the same .kai/. Returns a release func to defer on success; on
// failure returns an error naming the live PID holding the lock so
// the user can decide whether to wait or kill it. Stale locks from
// crashed prior runs (process gone but file remains) are reclaimed
// automatically — a kai capture that OOMs or gets SIGKILL'd won't
// permanently brick the next capture.
//
// Path is <kaiDir>/capture.lock. We use O_EXCL create to win the
// race atomically rather than a stat-then-create dance (which has
// a TOCTOU between the check and the create).
func acquireCaptureLock(kaiDir string) (func(), error) {
	// Ensure the .kai/ directory exists before opening the lock
	// file. 2026-05-25 dogfood pinned this: in a multi-root
	// workspace, a sibling project without a .kai/ directory
	// (e.g. a `design` project that was never `kai init`'d)
	// caused capture-lock creation to fail with "no such file or
	// directory" — preflight.missing_blobs auto-repair then
	// failed for the WHOLE workspace, blocking plan execution on
	// unrelated kai-aware projects. MkdirAll is idempotent; if
	// the dir already exists it's a no-op.
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		return nil, fmt.Errorf("creating .kai directory: %w", err)
	}
	lockPath := filepath.Join(kaiDir, "capture.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating capture lock: %w", err)
		}
		// Lock exists — is the holder alive, or did a prior run die
		// without cleaning up?
		existing, rerr := os.ReadFile(lockPath)
		if rerr == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(existing))); perr == nil {
				if processAlive(pid) {
					return nil, fmt.Errorf(
						"another kai capture is already running (pid %d).\n"+
							"  Wait for it to finish, or `kill %d` if it appears stuck.\n"+
							"  Two concurrent captures on the same workspace spin at 300%%+ CPU\n"+
							"  contending for the SQLite writer lock without progress.",
						pid, pid)
				}
			}
		}
		// Stale lock — reclaim it. Best-effort: a concurrent run
		// could still win the race here, but the second O_EXCL
		// create below will catch that.
		if rmErr := os.Remove(lockPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("clearing stale capture lock: %w", rmErr)
		}
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("creating capture lock after stale cleanup: %w", err)
		}
	}
	if _, werr := fmt.Fprintf(f, "%d", os.Getpid()); werr != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("writing pid to capture lock: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("closing capture lock: %w", cerr)
	}
	return func() { _ = os.Remove(lockPath) }, nil
}

// processAlive reports whether the given PID is still running.
// Signal 0 is the standard Unix probe: the kernel does the
// existence check but delivers no signal. Returns false on errors
// (no such process, permission denied — which on macOS also means
// the process is gone for non-root callers since the PID has been
// reused or freed).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func runCapture(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("capture")
	defer te.Finish()

	memstat.Log("capture-start")
	memstat.LogBurst("capture", 2*time.Second, 3*time.Second, 5*time.Second, 20*time.Second, 30*time.Second)
	defer memstat.Log("capture-end")

	// Determine path to capture
	capturePath := "."
	if len(args) > 0 {
		capturePath = args[0]
	}

	// Refuse a second concurrent `kai capture` on the same .kai/.
	// Two captures contending for the SQLite writer lock spin at
	// 300%+ CPU for tens of minutes (2026-05-13 dogfood: two
	// background captures accumulated 27 CPU-minutes of spin with
	// no progress before the user noticed). One capture at a time
	// is the right contract; the lockfile is the enforcement.
	//
	// kaipath.Resolve is also used below; computed here so the lock
	// path is resolved before the heavier db.Open / matcher.Load
	// phases below could fail and leave the lock orphaned. A stale
	// lock from a crashed prior run is reclaimed by checking
	// whether the recorded PID is still alive.
	releaseLock, err := acquireCaptureLock(kaipath.Resolve(capturePath))
	if err != nil {
		if te != nil {
			te.Result = "error"
			te.ErrorClass = "capture_lock"
		}
		return err
	}
	defer releaseLock()

	db, err := openDB()
	if err != nil {
		if te != nil {
			te.Result = "error"
			te.ErrorClass = "db_open"
		}
		return err
	}
	defer db.Close()

	// Load modules
	debugf("Loading module configuration...")
	matcher, err := loadMatcher()
	if err != nil {
		if !initMode {
			fmt.Println("failed")
		}
		return err
	}
	moduleCount := len(matcher.GetAllModules())
	debugf("found %d modules", moduleCount)
	debugf("loaded %d modules", moduleCount)

	// Show explanation if requested
	if captureExplain {
		ctx := explain.ExplainCapture(capturePath, moduleCount)
		ctx.Print(os.Stdout)
	}

	// Step 1: Create snapshot
	debugf("Step 1/2: Creating snapshot from %s", capturePath)
	phaseStart := time.Now()

	// Load stat cache for incremental file reading
	cacheDir := kaipath.Resolve(capturePath)
	statCache := dirio.LoadStatCache(cacheDir)

	if !initMode {
		fmt.Fprintf(os.Stderr, "Scanning files...")
	}
	source, err := dirio.OpenDirectory(capturePath, dirio.WithStatCache(statCache))
	if err != nil {
		if !initMode {
			fmt.Fprintf(os.Stderr, " failed\n")
		}
		if te != nil {
			te.Result = "error"
			te.ErrorClass = "dir_open"
		}
		return fmt.Errorf("opening directory: %w", err)
	}

	// By default, capture commits only the caller's own changes — revert any
	// live-synced peer contributions out of the snapshot (per-file, line-level
	// where we both touched a file). `--all` includes everything.
	capSource := filesource.FileSource(source)
	if !captureAll {
		if filtered, n := applyPeerExclusion(capSource, db, kaiDir, capturePath); n > 0 {
			capSource = filtered
			if !initMode {
				fmt.Fprintf(os.Stderr, "Ignoring live-synced changes in %d file(s) (use --all to include)\n", n)
			}
		}
	}

	files, err := capSource.GetFiles()
	if err != nil {
		if !initMode {
			fmt.Fprintf(os.Stderr, " failed\n")
		}
		if te != nil {
			te.Result = "error"
			te.ErrorClass = "get_files"
		}
		return fmt.Errorf("getting files: %w", err)
	}
	if !initMode {
		fmt.Fprintf(os.Stderr, " %d files\n", len(files))
	}

	if !initMode {
		fmt.Fprintf(os.Stderr, "Creating snapshot...")
	}
	creator := snapshot.NewCreator(db, matcher)
	snapshotID, err := creator.CreateSnapshot(capSource)
	if err != nil {
		if !initMode {
			fmt.Fprintf(os.Stderr, " failed\n")
		}
		if te != nil {
			te.Result = "error"
			te.ErrorClass = "snapshot_create"
		}
		return fmt.Errorf("creating snapshot: %w", err)
	}
	if !initMode {
		fmt.Fprintf(os.Stderr, " done\n")
	}
	debugf("snapshot created: %s", util.BytesToHex(snapshotID))

	// Persist stat cache so next capture can skip unchanged files
	if err := statCache.Save(cacheDir); err != nil {
		debugf("warning: failed to save stat cache: %v", err)
	}

	if te != nil {
		te.SetPhase("snapshot", time.Since(phaseStart).Milliseconds())
		te.Stats["files"] = int64(len(files))
		te.Stats["modules"] = int64(moduleCount)
	}

	// Check if snapshot is identical to previous — skip analysis if nothing changed.
	// Snapshot IDs are content-addressed, so same ID = same files = same symbols/graph.
	// BUT: only skip if analysis actually ran on the previous snapshot. kai init's
	// git-history import creates snapshots without analysis, so the first real
	// capture must still analyze even though the IDs match.
	// When a workspace is checked out, capture stages into it and the
	// workspace head is the baseline — not the global snap.latest, which
	// tracks trunk. Compute the baseline ref once; it's reused for the
	// analysis-skip check, authorship forward-porting, and the staging
	// branch below.
	currentCaptureWs, _ := getCurrentWorkspace()
	baselineRefName := "snap.latest"
	if currentCaptureWs != "" {
		baselineRefName = "ws." + currentCaptureWs + ".head"
	}
	existingLatestRef, _ := ref.NewRefManager(db).Get(baselineRefName)
	skipAnalysis := existingLatestRef != nil && bytes.Equal(snapshotID, existingLatestRef.TargetID)
	if skipAnalysis {
		definesEdges, _ := db.GetEdgesByContext(snapshotID, graph.EdgeDefinesIn)
		if len(definesEdges) == 0 {
			skipAnalysis = false
		}
	}

	// Also skip analysis if the MCP file watcher is active — it already
	// updated symbols and edges incrementally. This makes kai capture a
	// fast checkpoint (~500ms) instead of a full re-analyze.
	if !skipAnalysis {
		entries, _ := os.ReadDir(kaiDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "mcp-session-") && strings.HasSuffix(e.Name(), ".json") {
				data, err := os.ReadFile(filepath.Join(kaiDir, e.Name()))
				if err == nil {
					var session map[string]interface{}
					if json.Unmarshal(data, &session) == nil {
						if updatedAt, ok := session["updatedAt"].(float64); ok {
							age := time.Since(time.UnixMilli(int64(updatedAt)))
							if age < 2*time.Minute {
								debugf("MCP watcher active (session %s, %v ago), skipping analysis", e.Name(), age.Round(time.Second))
								skipAnalysis = true
								break
							}
						}
					}
				}
			}
		}
	}

	if skipAnalysis {
		debugf("Snapshot unchanged or watcher active, skipping analysis")
	} else {
		// Step 2: Analyze symbols + build call graph (single pass)
		debugf("Step 2/2: Analyzing...")
		phaseStart = time.Now()
		progress := func(current, total int, filename string) {
			if initMode {
				return
			}
			display := filename
			if len(display) > 40 {
				display = "..." + display[len(display)-37:]
			}
			if verbose {
				debugf("Analyzing... %d/%d %s", current, total, display)
			} else {
				fmt.Fprintf(os.Stderr, "\rAnalyzing... %d/%d", current, total)
			}
		}
		if err := creator.Analyze(snapshotID, progress); err != nil {
			if !initMode {
				fmt.Fprintf(os.Stderr, "\r\033[K")
			}
			debugf("Analysis: warning: some files failed")
			if !initMode {
				fmt.Fprintf(os.Stderr, "  %v\n", err)
			}
		} else {
			if !initMode {
				fmt.Fprintf(os.Stderr, "\r\033[K")
			}
			debugf("Analysis: done")
		}
		if te != nil {
			te.SetPhase("analyze", time.Since(phaseStart).Milliseconds())
		}
	}

	// Auto-detect AI authorship: if an MCP session is active, attribute
	// changed files to the AI agent without requiring kai_checkpoint calls.
	// NOTE: this runs regardless of `skipAnalysis` — in fact, `skipAnalysis`
	// is *usually* true during an active MCP session (watcher did the work),
	// which is the exact case we want to attribute.
	// NOTE: autoAttributeFromMCPSession was a session-presence heuristic
	// that attributed every changed file to the active MCP agent whenever
	// kai capture ran. That's wrong — users type comments while Claude is
	// idle, and those keystrokes were being silently labeled as AI work.
	// The honest replacement is the PostToolUse hook → `kai checkpoint`
	// path, which only records edits that actually flowed through an AI
	// client's tool runner. The heuristic function is kept below purely
	// so old deployments compile; it is intentionally never called.
	_ = autoAttributeFromMCPSession

	// Step 4: Consolidate authorship checkpoints.
	// We run this even when there are zero new checkpoints so that authorship
	// history is forward-ported from the previous snapshot — otherwise files
	// the agent didn't touch would drop their attribution on every capture.
	//
	// At this point snap.latest still points at the PREVIOUS snapshot
	// (it gets rewritten lower in this function), so we grab its ID here
	// and pass it into Consolidate.
	captureTimingPhaseStart := time.Now()
	var prevSnapForAuthorship []byte
	if prev, _ := ref.NewRefManager(db).Get(baselineRefName); prev != nil {
		prevSnapForAuthorship = prev.TargetID
	}
	if authorship.CountPendingCheckpoints(kaiDir) > 0 || prevSnapForAuthorship != nil {
		debugf("Step 4: Consolidating authorship checkpoints...")
		phaseStart = time.Now()
		if err := authorship.Consolidate(db, snapshotID, prevSnapForAuthorship, kaiDir); err != nil {
			debugf("warning: authorship consolidation: %v", err)
		} else {
			debugf("Authorship checkpoints consolidated")
		}
		if te != nil {
			te.SetPhase("authorship", time.Since(phaseStart).Milliseconds())
		}
	}
	captureTimingPrint("10-authorship", captureTimingPhaseStart)
	captureTimingPhaseStart = time.Now()

	// Workspace-aware capture: when a workspace is checked out, stage the
	// snapshot we just built into that workspace instead of advancing the
	// global snap.latest (trunk). This advances ws.<name>.head and records
	// a changeset, so `kai integrate --ws <name>` and `kai diff`/`kai status`
	// see the work. Without this, capture wrote snap.latest while the
	// workspace head stayed at its base and integrate found "no changes".
	if currentCaptureWs != "" {
		mgr := workspace.NewManager(db)
		result, err := mgr.StageSnapshot(currentCaptureWs, snapshotID, matcher, captureMessage, &workspace.StageOptions{
			SignKeyPath: os.Getenv("KAI_SSH_SIGN_KEY"),
		})
		if err != nil {
			if te != nil {
				te.Result = "error"
				te.ErrorClass = "ws_stage"
			}
			return fmt.Errorf("staging into workspace %q: %w", currentCaptureWs, err)
		}

		if len(result.Conflicts) > 0 {
			if !initMode {
				fmt.Printf("Conflicts detected (%d):\n", len(result.Conflicts))
				for _, c := range result.Conflicts {
					fmt.Printf("  %s: %s\n", c.Path, c.Description)
				}
			}
			return fmt.Errorf("resolve conflicts before capturing")
		}

		if result.ChangedFiles == 0 {
			if !initMode {
				fmt.Printf("Nothing to capture (workspace %q already up to date)\n", currentCaptureWs)
			}
			return nil
		}

		// Advance the named refs: ws.<name>.head and the changeset refs.
		// StageSnapshot updated the workspace node's head, but the named
		// ws.<name>.head ref is the caller's responsibility (mirrors
		// runWsStage).
		autoRefMgr := ref.NewAutoRefManager(db)
		if err := autoRefMgr.OnWorkspaceHeadChanged(currentCaptureWs, result.HeadSnapshot); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to update workspace head ref: %v\n", err)
		}
		if err := autoRefMgr.OnChangeSetCreated(result.ChangeSetID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to update changeset ref: %v\n", err)
		}

		// Persist capture message for push (parity with the trunk path).
		if captureMessage != "" {
			dataDir := kaipath.Resolve(capturePath)
			os.MkdirAll(dataDir, 0755)
			os.WriteFile(filepath.Join(dataDir, "message"), []byte(captureMessage), 0644)
		}

		if !initMode {
			newHex := util.BytesToHex(result.HeadSnapshot)[:12]
			fmt.Printf("Captured %s into workspace %s (%d file(s), %d change type(s))\n",
				newHex, currentCaptureWs, result.ChangedFiles, result.ChangeTypes)
			if existingLatestRef != nil {
				fmt.Printf("  ws.%s.head: %s → %s\n", currentCaptureWs,
					util.BytesToHex(existingLatestRef.TargetID)[:12], newHex)
			}
			if result.ChangeSetID != nil {
				fmt.Printf("  changeset: %s\n", util.BytesToHex(result.ChangeSetID)[:12])
			}
		}
		return nil
	}

	// Check if this is the first scan (no snap.latest ref exists)
	refMgr := ref.NewRefManager(db)
	existingLatest, _ := refMgr.Get("snap.latest")
	isFirstScan := existingLatest == nil

	// Store previous snapshot ID before updating refs
	var previousSnapID []byte
	if existingLatest != nil {
		previousSnapID = existingLatest.TargetID
	}

	// Compute change summary BEFORE updating refs (so @snap:last still points to old snapshot).
	// KAI_CAPTURE_SKIP_SUMMARY=1 skips this entirely. Reason: classify.DetectChanges does
	// tree-sitter parse + AST diff for every modified source file. On a workspace with a big
	// modified file (e.g. main.go at 22k lines), this can take 5+ minutes and dominates capture
	// wall time. The summary is purely for human-facing output ("X files, Y modified"). The
	// orchestrator's pre-spawn capture sets this env var because it doesn't display the
	// summary and was hitting the 60s timeout reliably on a working tree with edits to
	// main.go. Terminal captures (kai capture from a shell) keep the summary.
	skipSummary := os.Getenv("KAI_CAPTURE_SKIP_SUMMARY") == "1"
	var capSummary *captureSummary
	if !isFirstScan && !skipSummary {
		debugf("Computing changes...")
		capSummary = computeCaptureSummary(db, snapshotID, matcher)
		debugf("Computing changes: done")
	}
	captureTimingPrint("11-change-summary", captureTimingPhaseStart)
	captureTimingPhaseStart = time.Now()

	// Preserve previous snapshot before overwriting snap.latest.
	// Copy the old snap.latest's meta (git commit info) to the timestamped ref.
	if previousSnapID != nil {
		ts := time.Now().UTC().Format("20060102T150405.000")
		prevRefName := "snap." + ts
		var prevMeta map[string]string
		if existingLatest != nil && existingLatest.Meta != nil {
			prevMeta = existingLatest.Meta
		}
		if err := refMgr.SetWithMeta(prevRefName, previousSnapID, ref.KindSnapshot, "", prevMeta); err != nil {
			debugf("warning: failed to preserve previous snapshot as %s: %v", prevRefName, err)
		} else {
			debugf("preserved previous snapshot as %s", prevRefName)
		}
	}

	// Update refs - like git commit, capture always updates snap.latest
	debugf("Updating refs...")
	debugf("updating refs: snap.latest -> %s", shortID(util.BytesToHex(snapshotID)))
	autoRefMgr := ref.NewAutoRefManager(db)

	// Auto-capture git commit metadata (message, author, SHA) if in a git repo
	var snapMeta map[string]string
	if gitSHA, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		snapMeta = make(map[string]string)
		snapMeta["git.sha"] = strings.TrimSpace(string(gitSHA))
		if msg, err := exec.Command("git", "log", "-1", "--format=%s").Output(); err == nil {
			snapMeta["git.message"] = strings.TrimSpace(string(msg))
		}
		if author, err := exec.Command("git", "log", "-1", "--format=%an <%ae>").Output(); err == nil {
			snapMeta["git.author"] = strings.TrimSpace(string(author))
		}
		if branch, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			snapMeta["git.branch"] = strings.TrimSpace(string(branch))
		}
		debugf("git metadata: sha=%s author=%s msg=%s", snapMeta["git.sha"], snapMeta["git.author"], snapMeta["git.message"])
	}

	// Snapshot author — who took THIS snapshot, distinct from the
	// underlying git.author (which is the last commit's author and
	// can be whatever upstream committed last). Sourced from the
	// user's git identity (user.name / user.email) so it reflects
	// the kai user, not whoever wrote the upstream code. Falls back
	// to $USER + hostname when git config is unset, so a fresh
	// system without a git identity still gets a meaningful label.
	if author := snapshotAuthor(); author != "" {
		if snapMeta == nil {
			snapMeta = make(map[string]string)
		}
		snapMeta["kai.author"] = author
	}

	// Add change summary to meta
	if capSummary != nil && capSummary.hasChanges {
		if snapMeta == nil {
			snapMeta = make(map[string]string)
		}
		snapMeta["changes.files"] = fmt.Sprintf("%d", capSummary.filesChanged)

		// Count changes by type for a brief summary
		if len(capSummary.changeTypes) > 0 {
			buckets := bucketChangeTypes(capSummary.changeTypes)
			var parts []string
			for _, bucket := range []ChangeBucket{BucketStructural, BucketBehavioral, BucketAPIContract} {
				if paths := buckets[bucket]; len(paths) > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", len(paths), strings.ToLower(string(bucket))))
				}
			}
			if len(parts) > 0 {
				snapMeta["changes.summary"] = strings.Join(parts, ", ")
			}
		}
	}

	// Capture-supplied message (`kai capture -m "..."`) lands on
	// the snap's ref metadata as `kai.message`. /log prefers this
	// over `git.message` so each snapshot displays its own headline
	// instead of the underlying git commit's. Without this, every
	// snap on a single git commit reads "initial commit" — looks
	// like nothing happened even after multiple captures.
	if captureMessage != "" {
		if snapMeta == nil {
			snapMeta = make(map[string]string)
		}
		snapMeta["kai.message"] = captureMessage
	}

	// Always update snap.latest (like git commit updates HEAD)
	if err := autoRefMgr.OnSnapshotCreatedWithMeta(snapshotID, snapMeta); err != nil {
		debugf("warning: failed to update refs: %v", err)
	} else {
		debugf("refs updated successfully")
	}

	// Auto-create changeset if there are changes
	if capSummary != nil && capSummary.hasChanges && previousSnapID != nil {
		debugf("Creating changeset...")
		_, err = createChangesetFromSnapshots(db, previousSnapID, snapshotID, "")
		if err != nil {
			debugf("warning: failed to create changeset: %v", err)
		} else {
			debugf("changeset created")
		}
	}

	// Save capture message for push (like git stores commit message)
	if captureMessage != "" {
		dataDir := kaipath.Resolve(capturePath)
		os.MkdirAll(dataDir, 0755)
		os.WriteFile(filepath.Join(dataDir, "message"), []byte(captureMessage), 0644)
	}

	captureTimingPrint("12-refs+meta", captureTimingPhaseStart)

	// Summary — quiet by default, verbose for details
	if !initMode {
		snapHex := util.BytesToHex(snapshotID)[:12]
		if capSummary != nil && capSummary.hasChanges {
			fmt.Printf("Captured %s (%d files, %d modified)\n", snapHex, len(files), capSummary.filesChanged)
		} else if isFirstScan {
			fmt.Printf("Captured %s (%d files)\n", snapHex, len(files))
		} else {
			fmt.Printf("Captured %s (%d files, no changes)\n", snapHex, len(files))
		}
	}

	// Verbose: detailed breakdown
	if capSummary != nil && capSummary.hasChanges {
		debugf("Changes detected: %d file(s) modified", capSummary.filesChanged)
		if len(capSummary.changeTypes) > 0 {
			buckets := bucketChangeTypes(capSummary.changeTypes)
			bucketOrder := []ChangeBucket{BucketStructural, BucketBehavioral, BucketAPIContract}
			for _, bucket := range bucketOrder {
				paths := buckets[bucket]
				if len(paths) == 0 {
					continue
				}
				if len(paths) <= 3 {
					debugf("  %s: %s", bucket, strings.Join(paths, ", "))
				} else {
					debugf("  %s: %d files", bucket, len(paths))
				}
			}
		}
		if len(capSummary.modules) > 0 {
			debugf("  Modules: %s", strings.Join(capSummary.modules, ", "))
		}
	}

	return nil
}

// captureSummary holds a summary of changes detected during capture
type captureSummary struct {
	hasChanges   bool
	filesChanged int
	changeTypes  map[string][]string // category -> list of paths
	modules      []string
}

// ChangeBucket represents a high-level category of changes for simpler UX
type ChangeBucket string

const (
	BucketStructural  ChangeBucket = "Structural"   // Things added/removed/moved
	BucketBehavioral  ChangeBucket = "Behavioral"   // Logic/values changed
	BucketAPIContract ChangeBucket = "API/Contract" // Interface/contract changed
)

// categorizeToBucket maps a raw change category to one of 3 user-friendly buckets
func categorizeToBucket(category string) ChangeBucket {
	switch category {
	// Structural: things were added/removed/moved
	case "FILE_ADDED", "FILE_DELETED", "FUNCTION_ADDED", "FUNCTION_REMOVED",
		"JSON_FIELD_ADDED", "JSON_FIELD_REMOVED", "YAML_KEY_ADDED", "YAML_KEY_REMOVED":
		return BucketStructural
	// Behavioral: logic/values changed
	case "CONDITION_CHANGED", "CONSTANT_UPDATED", "JSON_VALUE_CHANGED",
		"JSON_ARRAY_CHANGED", "YAML_VALUE_CHANGED", "FILE_CONTENT_CHANGED":
		return BucketBehavioral
	// API/Contract: interface/contract changed
	case "API_SURFACE_CHANGED":
		return BucketAPIContract
	default:
		// Unknown categories go to Behavioral as fallback
		return BucketBehavioral
	}
}

// bucketChangeTypes groups raw change types into 3 buckets for simpler display
func bucketChangeTypes(changeTypes map[string][]string) map[ChangeBucket][]string {
	buckets := make(map[ChangeBucket][]string)
	seen := make(map[ChangeBucket]map[string]bool)

	for category, paths := range changeTypes {
		bucket := categorizeToBucket(category)
		if seen[bucket] == nil {
			seen[bucket] = make(map[string]bool)
		}
		for _, path := range paths {
			if !seen[bucket][path] {
				seen[bucket][path] = true
				buckets[bucket] = append(buckets[bucket], path)
			}
		}
	}

	return buckets
}

// computeCaptureSummary computes a quick summary of changes between @snap:last and new snapshot
func computeCaptureSummary(db *graph.DB, newSnapshotID []byte, matcher *module.Matcher) *captureSummary {
	summary := &captureSummary{
		changeTypes: make(map[string][]string),
	}

	// Get baseline snapshot
	baseSnapID, err := resolveSnapshotID(db, "@snap:last")
	if err != nil {
		return summary
	}

	creator := snapshot.NewCreator(db, matcher)

	// Get files from both snapshots
	baseFiles, err := creator.GetSnapshotFiles(baseSnapID)
	if err != nil {
		return summary
	}
	newFiles, err := creator.GetSnapshotFiles(newSnapshotID)
	if err != nil {
		return summary
	}

	// Build maps
	baseFileMap := make(map[string]*graph.Node)
	for _, f := range baseFiles {
		path, _ := f.Payload["path"].(string)
		baseFileMap[path] = f
	}
	newFileMap := make(map[string]*graph.Node)
	for _, f := range newFiles {
		path, _ := f.Payload["path"].(string)
		newFileMap[path] = f
	}

	// Find changed files
	var changedPaths []string
	modulesSet := make(map[string]bool)

	// Check for modified and added files
	for path, newFile := range newFileMap {
		baseFile, exists := baseFileMap[path]
		newDigest, _ := newFile.Payload["digest"].(string)

		if !exists {
			// Added file
			changedPaths = append(changedPaths, path)
			summary.changeTypes["FILE_ADDED"] = append(summary.changeTypes["FILE_ADDED"], path)
		} else {
			baseDigest, _ := baseFile.Payload["digest"].(string)
			if newDigest != baseDigest {
				// Modified file
				changedPaths = append(changedPaths, path)

				// Try to detect semantic changes
				lang, _ := newFile.Payload["lang"].(string)
				beforeContent, _ := db.ReadObject(baseDigest)
				afterContent, _ := db.ReadObject(newDigest)

				if len(beforeContent) > 0 && len(afterContent) > 0 {
					detector := classify.NewDetector()
					var changes []*classify.ChangeType

					switch lang {
					case "json":
						changes, _ = classify.DetectJSONChanges(path, beforeContent, afterContent)
					case "ts", "js", "tsx", "jsx", "go", "py":
						changes, _ = detector.DetectChanges(path, beforeContent, afterContent, "")
					default:
						changes = []*classify.ChangeType{classify.NewFileChange(classify.FileContentChanged, path)}
					}

					for _, ct := range changes {
						category := string(ct.Category)
						summary.changeTypes[category] = append(summary.changeTypes[category], path)
					}
				}
			}
		}

		// Track modules
		if matcher != nil {
			for _, mod := range matcher.MatchPath(path) {
				modulesSet[mod] = true
			}
		}
	}

	// Check for deleted files
	for path := range baseFileMap {
		if _, exists := newFileMap[path]; !exists {
			changedPaths = append(changedPaths, path)
			summary.changeTypes["FILE_DELETED"] = append(summary.changeTypes["FILE_DELETED"], path)
		}
	}

	summary.filesChanged = len(changedPaths)
	summary.hasChanges = len(changedPaths) > 0

	for mod := range modulesSet {
		summary.modules = append(summary.modules, mod)
	}

	return summary
}

// createSnapshotFromDir creates a snapshot from the given directory and returns its ID.
// This is a helper used by commands that need to auto-snapshot (e.g., ws create).
func createSnapshotFromDir(db *graph.DB, dir string) ([]byte, error) {
	debugf("Loading module configuration...")
	matcher, err := loadMatcher()
	if err != nil {
		fmt.Println("failed")
		return nil, err
	}
	fmt.Printf("found %d modules\n", len(matcher.GetAllModules()))

	fmt.Printf("Scanning directory: %s\n", dir)
	source, err := dirio.OpenDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("opening directory: %w", err)
	}

	debugf("Reading files...")
	files, err := source.GetFiles()
	if err != nil {
		fmt.Println("failed")
		return nil, fmt.Errorf("getting files: %w", err)
	}
	debugf("found %d files", len(files))

	debugf("Creating snapshot...")
	creator := snapshot.NewCreator(db, matcher)
	snapshotID, err := creator.CreateSnapshot(source)
	if err != nil {
		fmt.Println("failed")
		return nil, fmt.Errorf("creating snapshot: %w", err)
	}
	fmt.Println("done")

	// Auto-analyze symbols
	fmt.Printf("Analyzing symbols... ")
	progress := func(current, total int, filename string) {
		display := filename
		if len(display) > 40 {
			display = "..." + display[len(display)-37:]
		}
		fmt.Printf("\rAnalyzing symbols... %d/%d %s", current, total, display)
		fmt.Print("\033[K")
	}
	if err := creator.AnalyzeSymbols(snapshotID, progress); err != nil {
		fmt.Println(" failed")
		fmt.Fprintf(os.Stderr, "warning: symbol analysis failed: %v\n", err)
	} else {
		debugf("Analyzing symbols: done")
	}

	// Update auto-refs
	debugf("Updating refs...")
	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnSnapshotCreated(snapshotID); err != nil {
		fmt.Println("failed")
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	} else {
		fmt.Println("done")
	}

	fmt.Printf("Created snapshot: %s\n", util.BytesToHex(snapshotID))
	return snapshotID, nil
}

func runSnapshot(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("snapshot")
	defer te.Finish()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	debugf("Loading module configuration...")
	matcher, err := loadMatcher()
	if err != nil {
		fmt.Println("failed")
		return err
	}
	fmt.Printf("found %d modules\n", len(matcher.GetAllModules()))
	debugf("loaded %d modules", len(matcher.GetAllModules()))

	var source filesource.FileSource

	// Require explicit --git or --dir (no positional args allowed)
	hasExplicitGit := snapshotGitRef != ""
	hasExplicitDir := dirPath != ""
	hasPositionalArg := len(args) > 0

	// Reject positional arguments - must be explicit
	if hasPositionalArg {
		arg := args[0]
		fmt.Println()
		fmt.Println("╭─ Positional arguments not allowed")
		fmt.Println("│")
		fmt.Printf("│  'kai snapshot create %s' is ambiguous.\n", arg)
		fmt.Println("│")
		fmt.Println("│  Please be explicit about the source:")
		fmt.Println("│")
		fmt.Printf("│    kai snapshot create --git %s    # Snapshot from Git commit/branch\n", arg)
		fmt.Printf("│    kai snapshot create --dir %s    # Snapshot from directory\n", arg)
		fmt.Println("│")
		fmt.Println("│  Or use 'kai snap' for quick directory snapshots:")
		fmt.Println("│")
		fmt.Printf("│    kai snap %s\n", arg)
		fmt.Println("│")
		fmt.Println("╰────────────────────────────────────────────")
		return fmt.Errorf("positional arguments not allowed: use --git or --dir")
	}

	// Handle the explicit modes
	if hasExplicitDir {
		// Directory mode - no Git required
		path := dirPath
		if path == "" {
			path = "."
		}
		fmt.Printf("Scanning directory: %s\n", path)
		source, err = dirio.OpenDirectory(path)
		if err != nil {
			return fmt.Errorf("opening directory: %w", err)
		}
	} else if hasExplicitGit {
		// Explicit Git mode
		fmt.Printf("Opening git ref: %s\n", snapshotGitRef)
		source, err = gitio.OpenSource(repoPath, snapshotGitRef)
		if err != nil {
			return fmt.Errorf("opening git source: %w", err)
		}
	} else {
		// No source specified - show helpful usage
		fmt.Println()
		fmt.Println("╭─ No snapshot source specified")
		fmt.Println("│")
		fmt.Println("│  Choose one of:")
		fmt.Println("│")
		fmt.Println("│    kai snapshot create --git main        # From Git commit/branch/tag")
		fmt.Println("│    kai snapshot create --dir .           # From directory")
		fmt.Println("│")
		fmt.Println("│  Or use 'kai snap' for quick directory snapshots:")
		fmt.Println("│")
		fmt.Println("│    kai snap                       # Snapshot current directory")
		fmt.Println("│    kai snap src/                  # Snapshot specific path")
		fmt.Println("│")
		fmt.Println("│  For the full workflow, use 'kai capture':")
		fmt.Println("│")
		fmt.Println("│    kai capture                       # Snapshot + analyze in one step")
		fmt.Println("│")
		fmt.Println("╰────────────────────────────────────────────")
		return fmt.Errorf("snapshot source required: use --git <ref> or --dir <path>")
	}

	debugf("Reading files...")
	files, err := source.GetFiles()
	if err != nil {
		fmt.Println("failed")
		return fmt.Errorf("getting files: %w", err)
	}
	debugf("found %d files", len(files))

	debugf("snapshot: %d files read", len(files))
	debugf("Creating snapshot...")
	creator := snapshot.NewCreator(db, matcher)
	snapshotID, err := creator.CreateSnapshot(source)
	if err != nil {
		fmt.Println("failed")
		return fmt.Errorf("creating snapshot: %w", err)
	}
	fmt.Println("done")
	debugf("snapshot created: %s", util.BytesToHex(snapshotID))

	// Auto-analyze symbols for better intent generation
	fmt.Printf("Analyzing symbols... ")
	progress := func(current, total int, filename string) {
		// Truncate filename if too long
		display := filename
		if len(display) > 40 {
			display = "..." + display[len(display)-37:]
		}
		fmt.Printf("\rAnalyzing symbols... %d/%d %s", current, total, display)
		// Clear rest of line in case previous filename was longer
		fmt.Print("\033[K")
	}
	if err := creator.AnalyzeSymbols(snapshotID, progress); err != nil {
		// Non-fatal - continue without symbols
		fmt.Println(" failed")
		fmt.Fprintf(os.Stderr, "warning: symbol analysis failed: %v\n", err)
	} else {
		debugf("Analyzing symbols: done")
	}

	// Add description if provided
	if snapshotMessage != "" {
		node, err := db.GetNode(snapshotID)
		if err == nil && node != nil {
			node.Payload["description"] = snapshotMessage
			if err := db.UpdateNodePayload(snapshotID, node.Payload); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to save description: %v\n", err)
			}
		}
	}

	// Update auto-refs
	debugf("Updating refs...")
	debugf("updating refs: snap.latest -> %s", shortID(util.BytesToHex(snapshotID)))
	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnSnapshotCreated(snapshotID); err != nil {
		fmt.Println("failed")
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	} else {
		fmt.Println("done")
	}

	fmt.Println()
	fmt.Printf("Created snapshot: %s\n", util.BytesToHex(snapshotID))
	fmt.Printf("Source: %s (%s)\n", source.Identifier(), source.SourceType())
	fmt.Printf("Files: %d\n", len(files))
	return nil
}

func runAnalyzeSymbols(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	snapshotID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving snapshot ID: %w", err)
	}

	matcher, err := loadMatcher()
	if err != nil {
		return err
	}

	creator := snapshot.NewCreator(db, matcher)
	fmt.Printf("Analyzing symbols... ")
	progress := func(current, total int, filename string) {
		display := filename
		if len(display) > 40 {
			display = "..." + display[len(display)-37:]
		}
		fmt.Printf("\rAnalyzing symbols... %d/%d %s", current, total, display)
		fmt.Print("\033[K")
	}
	if err := creator.AnalyzeSymbols(snapshotID, progress); err != nil {
		return fmt.Errorf("analyzing symbols: %w", err)
	}

	fmt.Print("\rAnalyzing symbols... done")
	fmt.Print("\033[K")
	fmt.Println()

	// Print summary of what was found
	fileEdges, err := db.GetEdges(snapshotID, graph.EdgeHasFile)
	if err == nil {
		totalSymbols := 0
		filesWithSymbols := 0
		for _, edge := range fileEdges {
			symbols, err := creator.GetSymbolsInFile(edge.Dst, snapshotID)
			if err == nil && len(symbols) > 0 {
				totalSymbols += len(symbols)
				filesWithSymbols++
			}
		}
		fmt.Printf("Found %d symbols across %d files\n", totalSymbols, filesWithSymbols)
	}

	return nil
}

func runAnalyzeCalls(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	snapshotID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving snapshot ID: %w", err)
	}

	matcher, err := loadMatcher()
	if err != nil {
		return err
	}

	creator := snapshot.NewCreator(db, matcher)
	fmt.Printf("Analyzing calls... ")
	progress := func(current, total int, filename string) {
		display := filename
		if len(display) > 40 {
			display = "..." + display[len(display)-37:]
		}
		fmt.Printf("\rAnalyzing calls... %d/%d %s", current, total, display)
		fmt.Print("\033[K")
	}
	if err := creator.AnalyzeCalls(snapshotID, progress); err != nil {
		return fmt.Errorf("analyzing calls: %w", err)
	}

	fmt.Print("\rAnalyzing calls... done")
	fmt.Print("\033[K")
	fmt.Println()

	// Print summary of edges created
	importEdges, _ := db.GetEdgesOfType(graph.EdgeImports)
	callEdges, _ := db.GetEdgesOfType(graph.EdgeCalls)
	testEdges, _ := db.GetEdgesOfType(graph.EdgeTests)
	fmt.Printf("Found %d imports, %d calls, %d test links\n", len(importEdges), len(callEdges), len(testEdges))

	return nil
}

func runQueryCallers(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	symbolName := args[0]

	// Normalize Go receiver-qualified names
	if idx := strings.LastIndex(symbolName, "."); idx >= 0 {
		symbolName = symbolName[idx+1:]
	}

	var edges []*graph.Edge
	if queryFileFlag != "" {
		edges, err = db.GetEdgesToByPath(queryFileFlag, graph.EdgeCalls)
	} else {
		edges, err = db.GetEdgesOfType(graph.EdgeCalls)
	}
	if err != nil {
		return fmt.Errorf("querying call edges: %w", err)
	}

	type callerEntry struct {
		file string
		line int
	}
	var results []callerEntry
	seen := make(map[string]bool)

	for _, edge := range edges {
		if len(edge.At) == 0 {
			continue
		}
		callNode, err := db.GetNode(edge.At)
		if err != nil || callNode == nil {
			continue
		}
		calleeName, _ := callNode.Payload["calleeName"].(string)
		if calleeName != symbolName {
			continue
		}
		callerFile, _ := callNode.Payload["callerFile"].(string)
		line := 0
		if l, ok := callNode.Payload["line"].(float64); ok {
			line = int(l)
		}
		key := fmt.Sprintf("%s:%d", callerFile, line)
		if seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, callerEntry{file: callerFile, line: line})
	}

	if len(results) == 0 {
		fmt.Printf("No callers found for %q\n", symbolName)
		return nil
	}

	fmt.Printf("%d callers of %s:\n", len(results), symbolName)
	for _, r := range results {
		if r.line > 0 {
			fmt.Printf("  %s:%d\n", r.file, r.line)
		} else {
			fmt.Printf("  %s\n", r.file)
		}
	}
	return nil
}

func runQueryDependents(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	filePath := args[0]

	edges, err := db.GetEdgesToByPath(filePath, graph.EdgeImports)
	if err != nil {
		return fmt.Errorf("querying dependents: %w", err)
	}

	var dependents []string
	seen := make(map[string]bool)
	for _, edge := range edges {
		node, err := db.GetNode(edge.Src)
		if err != nil || node == nil {
			continue
		}
		if path, ok := node.Payload["path"].(string); ok && !seen[path] {
			dependents = append(dependents, path)
			seen[path] = true
		}
	}
	sort.Strings(dependents)

	if len(dependents) == 0 {
		fmt.Printf("No dependents found for %s\n", filePath)
		return nil
	}

	fmt.Printf("%d dependents of %s:\n", len(dependents), filePath)
	for _, d := range dependents {
		fmt.Printf("  %s\n", d)
	}
	return nil
}

func runQueryImpact(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	filePath := args[0]
	maxDepth := queryDepthFlag

	type impactEntry struct {
		path   string
		hop    int
		isTest bool
	}

	visited := make(map[string]int)
	visited[filePath] = 0
	frontier := []string{filePath}
	var results []impactEntry

	for hop := 1; hop <= maxDepth && len(frontier) > 0; hop++ {
		var nextFrontier []string
		for _, current := range frontier {
			// Follow IMPORTS edges
			importEdges, err := db.GetEdgesToByPath(current, graph.EdgeImports)
			if err == nil {
				for _, edge := range importEdges {
					node, err := db.GetNode(edge.Src)
					if err != nil || node == nil {
						continue
					}
					path, ok := node.Payload["path"].(string)
					if !ok {
						continue
					}
					if _, already := visited[path]; already {
						continue
					}
					visited[path] = hop
					results = append(results, impactEntry{path: path, hop: hop, isTest: isTestFileCLI(path)})
					nextFrontier = append(nextFrontier, path)
				}
			}

			// Follow CALLS edges
			callEdges, err := db.GetEdgesToByPath(current, graph.EdgeCalls)
			if err == nil {
				for _, edge := range callEdges {
					node, err := db.GetNode(edge.Src)
					if err != nil || node == nil {
						continue
					}
					path, ok := node.Payload["path"].(string)
					if !ok {
						continue
					}
					if _, already := visited[path]; already {
						continue
					}
					visited[path] = hop
					results = append(results, impactEntry{path: path, hop: hop, isTest: isTestFileCLI(path)})
					nextFrontier = append(nextFrontier, path)
				}
			}
		}
		frontier = nextFrontier
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].hop != results[j].hop {
			return results[i].hop < results[j].hop
		}
		return results[i].path < results[j].path
	})

	if len(results) == 0 {
		fmt.Printf("No downstream impact found for %s\n", filePath)
		return nil
	}

	// Separate source and test files
	var sourceFiles, testFiles []impactEntry
	for _, r := range results {
		if r.isTest {
			testFiles = append(testFiles, r)
		} else {
			sourceFiles = append(sourceFiles, r)
		}
	}

	fmt.Printf("%d files affected by changes to %s:\n", len(results), filePath)
	if len(sourceFiles) > 0 {
		fmt.Println("\n  Source files:")
		for _, r := range sourceFiles {
			fmt.Printf("    [hop %d] %s\n", r.hop, r.path)
		}
	}
	if len(testFiles) > 0 {
		fmt.Println("\n  Tests:")
		for _, r := range testFiles {
			fmt.Printf("    [hop %d] %s\n", r.hop, r.path)
		}
	}
	return nil
}

// isTestFileCLI checks if a file path looks like a test file.
func isTestFileCLI(path string) bool {
	lower := strings.ToLower(path)
	return strings.Contains(lower, "_test.") ||
		strings.Contains(lower, ".test.") ||
		strings.Contains(lower, ".spec.") ||
		strings.Contains(lower, "test_") ||
		strings.HasPrefix(lower, "tests/") ||
		strings.HasPrefix(lower, "test/") ||
		strings.Contains(lower, "__tests__/") ||
		strings.Contains(lower, "/tests/") ||
		strings.Contains(lower, "/test/")
}

func runTestAffected(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	matcher, err := loadMatcher()
	if err != nil {
		return err
	}

	creator := snapshot.NewCreator(db, matcher)

	// Resolve both snapshot IDs
	baseSnapshotID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving base snapshot ID: %w", err)
	}

	headSnapshotID, err := resolveSnapshotID(db, args[1])
	if err != nil {
		return fmt.Errorf("resolving head snapshot ID: %w", err)
	}

	// Get files that changed
	changedFiles, err := getChangedFiles(db, creator, baseSnapshotID, headSnapshotID)
	if err != nil {
		return fmt.Errorf("getting changed files: %w", err)
	}

	if len(changedFiles) == 0 {
		fmt.Println("No changed files found")
		return nil
	}

	// Build a set of changed file paths
	changedPaths := make(map[string]bool)
	for _, f := range changedFiles {
		changedPaths[f] = true
	}

	// Get all files in the snapshot to build the graph context
	files, err := creator.GetSnapshotFiles(headSnapshotID)
	if err != nil {
		return fmt.Errorf("getting snapshot files: %w", err)
	}

	// Build file ID -> path map
	filePathByID := make(map[string]string)
	fileIDByPath := make(map[string][]byte)
	for _, f := range files {
		path, _ := f.Payload["path"].(string)
		idHex := util.BytesToHex(f.ID)
		filePathByID[idHex] = path
		fileIDByPath[path] = f.ID
	}

	// Find all files that depend on the changed files (reverse call graph)
	// and all test files that test the changed files
	affectedTests := make(map[string]bool)

	// First: direct test files for changed files
	for changedPath := range changedPaths {
		// Find TESTS edges pointing to files with this path
		// Uses path-based lookup to handle content-addressed ID changes
		testsEdges, err := db.GetEdgesToByPath(changedPath, graph.EdgeTests)
		if err != nil {
			continue
		}
		for _, e := range testsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				affectedTests[path] = true
			} else {
				// Query node directly if not in current snapshot
				if srcNode, _ := db.GetNode(e.Src); srcNode != nil {
					if srcPath, ok := srcNode.Payload["path"].(string); ok {
						affectedTests[srcPath] = true
					}
				}
			}
		}

		// Also find files that import/call into the changed file
		importsEdges, err := db.GetEdgesToByPath(changedPath, graph.EdgeImports)
		if err != nil {
			continue
		}
		for _, e := range importsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				// If this importer is a test file, add it
				if parse.IsTestFile(path) {
					affectedTests[path] = true
				}
			} else {
				// Query node directly
				if srcNode, _ := db.GetNode(e.Src); srcNode != nil {
					if srcPath, ok := srcNode.Payload["path"].(string); ok {
						if parse.IsTestFile(srcPath) {
							affectedTests[srcPath] = true
						}
					}
				}
			}
		}

		callsEdges, err := db.GetEdgesToByPath(changedPath, graph.EdgeCalls)
		if err != nil {
			continue
		}
		for _, e := range callsEdges {
			srcHex := util.BytesToHex(e.Src)
			if path, ok := filePathByID[srcHex]; ok {
				if parse.IsTestFile(path) {
					affectedTests[path] = true
				}
			} else {
				// Query node directly
				if srcNode, _ := db.GetNode(e.Src); srcNode != nil {
					if srcPath, ok := srcNode.Payload["path"].(string); ok {
						if parse.IsTestFile(srcPath) {
							affectedTests[srcPath] = true
						}
					}
				}
			}
		}
	}

	// Output results
	if len(affectedTests) == 0 {
		fmt.Println("No affected test files found")
		fmt.Println("(Make sure you've run 'kai analyze calls @snap:last' to build the call graph)")
		return nil
	}

	// Sort for consistent output
	var tests []string
	for t := range affectedTests {
		tests = append(tests, t)
	}
	sort.Strings(tests)

	fmt.Printf("Affected test files (%d):\n", len(tests))
	for _, t := range tests {
		fmt.Println(t)
	}

	return nil
}

// CIPlan represents a runner-agnostic selection plan
type CIPlan struct {
	Version       int                `json:"version"`
	Mode          string             `json:"mode"`       // "selective", "expanded", "shadow", "full", "skip"
	Risk          string             `json:"risk"`       // "low", "medium", "high"
	SafetyMode    string             `json:"safetyMode"` // "shadow", "guarded", "strict"
	Confidence    float64            `json:"confidence"` // 0.0-1.0 confidence score
	Targets       CITargets          `json:"targets"`
	Impact        CIImpact           `json:"impact"`
	Policy        CIPolicy           `json:"policy"`
	Safety        CISafety           `json:"safety"`
	Uncertainty   CIUncertainty      `json:"uncertainty"`             // Structured uncertainty info
	ExpansionLog  []string           `json:"expansionLog,omitempty"`  // Why expansions happened
	DynamicImport *DynamicImportInfo `json:"dynamicImport,omitempty"` // Dynamic import analysis
	Coverage      *CoverageInfo      `json:"coverage,omitempty"`      // Coverage-based selection info
	Contracts     *ContractInfo      `json:"contracts,omitempty"`     // Contract/schema change info
	Fallback      CIFallback         `json:"fallback"`                // Fallback/tripwire status
	Provenance    CIProvenance       `json:"provenance"`              // Audit trail
	Prediction    CIPrediction       `json:"prediction,omitempty"`    // For shadow mode comparison
}

// DynamicImportInfo captures dynamic import detection details
type DynamicImportInfo struct {
	Detected  bool                    `json:"detected"`
	Files     []DynamicImportFile     `json:"files,omitempty"`
	Policy    DynamicImportPolicyUsed `json:"policy"`
	Telemetry DynamicImportTelemetry  `json:"telemetry"`
}

// DynamicImportFile represents a detected dynamic import in a file
type DynamicImportFile struct {
	Path         string  `json:"path"`
	Kind         string  `json:"kind"`                   // e.g., "import(variable)", "require(variable)", "__import__()"
	Line         int     `json:"line"`                   // Line number if available
	Bounded      bool    `json:"bounded"`                // True if bounded by webpackInclude or similar
	BoundedBy    string  `json:"boundedBy,omitempty"`    // What bounded it (e.g., "webpackInclude: /\\.widget\\.js$/")
	BoundedRisky bool    `json:"boundedRisky,omitempty"` // True if bounded but footprint exceeds threshold
	Allowlisted  bool    `json:"allowlisted"`            // True if in allowlist
	Confidence   float64 `json:"confidence"`             // 0.0-1.0 confidence this is truly dynamic
	ExpandedTo   string  `json:"expandedTo,omitempty"`   // What module/package it expanded to
}

// DynamicImportPolicyUsed shows what policy was applied
type DynamicImportPolicyUsed struct {
	Expansion      string   `json:"expansion"`            // nearest_module, package, owners, full_suite
	ExpandedTo     []string `json:"expandedTo,omitempty"` // What modules/tests were added
	OwnersFallback bool     `json:"ownersFallback"`
}

// DynamicImportTelemetry provides counters for visibility
type DynamicImportTelemetry struct {
	TotalDetected int    `json:"totalDetected"`
	Bounded       int    `json:"bounded"`
	BoundedRisky  int    `json:"boundedRisky,omitempty"` // Bounded but exceeds footprint threshold
	Unbounded     int    `json:"unbounded"`
	Allowlisted   int    `json:"allowlisted"`
	WidenedTests  int    `json:"widenedTests"`           // How many tests were added due to dynamic imports
	StrategyUsed  string `json:"strategyUsed,omitempty"` // Actual strategy that was applied
	CacheHits     int    `json:"cacheHits,omitempty"`    // Files served from cache
	CacheMisses   int    `json:"cacheMisses,omitempty"`  // Files that needed scanning
}

// DynamicImportCache caches detection results by file digest
type DynamicImportCache struct {
	mu      sync.RWMutex
	entries map[string]DynamicImportCacheEntry
}

// DynamicImportCacheEntry is a cached detection result
type DynamicImportCacheEntry struct {
	DetectorVersion string
	Imports         []DynamicImportFile
}

// Global cache instance
var dynamicImportCache = &DynamicImportCache{
	entries: make(map[string]DynamicImportCacheEntry),
}

// Get retrieves cached results for a file digest
func (c *DynamicImportCache) Get(digest string) ([]DynamicImportFile, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[digest]
	if !ok || entry.DetectorVersion != DetectorVersion {
		return nil, false
	}
	return entry.Imports, true
}

// Set stores detection results for a file digest
func (c *DynamicImportCache) Set(digest string, imports []DynamicImportFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[digest] = DynamicImportCacheEntry{
		DetectorVersion: DetectorVersion,
		Imports:         imports,
	}
}

// CIUncertainty captures structured uncertainty information
type CIUncertainty struct {
	Score   int                   `json:"score"`             // 0-100 uncertainty score (higher = more uncertain)
	Sources []string              `json:"sources"`           // What contributed to uncertainty
	Details *CIUncertaintyDetails `json:"details,omitempty"` // Detailed breakdown
}

// CIUncertaintyDetails provides granular uncertainty info
type CIUncertaintyDetails struct {
	Coverage      *CoverageUncertainty      `json:"coverage,omitempty"`
	DynamicImport *DynamicImportUncertainty `json:"dynamicImport,omitempty"`
}

// CoverageUncertainty captures coverage-related uncertainty
type CoverageUncertainty struct {
	FilesWithoutCoverage int `json:"filesWithoutCoverage"`
	LookbackDays         int `json:"lookbackDays"`
}

// DynamicImportUncertainty captures dynamic import uncertainty
type DynamicImportUncertainty struct {
	Detected  bool `json:"detected"`
	Bounded   int  `json:"bounded"`
	Unbounded int  `json:"unbounded"`
}

// CIFallback captures fallback/tripwire status for auditability
type CIFallback struct {
	Used     bool   `json:"used"`               // Whether fallback was triggered
	Reason   string `json:"reason,omitempty"`   // Why: runtime_tripwire, planner_over_threshold, panic_switch
	Trigger  string `json:"trigger,omitempty"`  // What triggered it: "Cannot find module", "low_confidence", etc.
	ExitCode int    `json:"exitCode,omitempty"` // Exit code that triggered fallback (e.g., 75)
}

// CoverageInfo captures coverage-based test selection info
type CoverageInfo struct {
	Enabled              bool     `json:"enabled"`
	LookbackDays         int      `json:"lookbackDays"`
	FilesWithCoverage    int      `json:"filesWithCoverage"`
	FilesWithoutCoverage int      `json:"filesWithoutCoverage"`
	TestsFromCoverage    []string `json:"testsFromCoverage,omitempty"` // Tests selected via coverage
	CoverageMapAge       string   `json:"coverageMapAge,omitempty"`    // When coverage was last ingested
}

// ContractInfo captures contract/schema change detection
type ContractInfo struct {
	Changed          bool             `json:"changed"`
	SchemasChanged   []ContractChange `json:"schemasChanged,omitempty"`
	TestsFromSchema  []string         `json:"testsFromSchema,omitempty"`  // Tests selected due to schema changes
	GeneratedChanged []string         `json:"generatedChanged,omitempty"` // Generated files that changed
}

// ContractChange represents a changed contract/schema
type ContractChange struct {
	Path         string   `json:"path"`
	Type         string   `json:"type"`                   // openapi, protobuf, graphql
	Service      string   `json:"service,omitempty"`      // Service/module this schema belongs to
	DigestBefore string   `json:"digestBefore,omitempty"` // Hash before change
	DigestAfter  string   `json:"digestAfter,omitempty"`  // Hash after change
	Tests        []string `json:"tests,omitempty"`        // Tests registered for this schema
}

// ========== Coverage Parsing Types ==========

// CoverageMap stores file -> test mappings from coverage reports
type CoverageMap struct {
	Version    int                        `json:"version"`
	RepoID     string                     `json:"repoId,omitempty"`
	Branch     string                     `json:"branch,omitempty"`
	Tag        string                     `json:"tag,omitempty"`
	IngestedAt string                     `json:"ingestedAt"`
	Entries    map[string][]CoverageEntry `json:"entries"` // file_path -> test entries
}

// CoverageEntry represents a single file -> test coverage record
type CoverageEntry struct {
	TestID       string `json:"testId"`                 // Test file/name that covers this file
	LastSeenAt   string `json:"lastSeenAt"`             // ISO8601 timestamp
	HitCount     int    `json:"hitCount"`               // How many times this test hit this file
	LinesCovered []int  `json:"linesCovered,omitempty"` // Specific lines if available
}

// ========== Contract Registry Types ==========

// ContractRegistry stores registered contracts/schemas
type ContractRegistry struct {
	Version   int               `json:"version"`
	Contracts []ContractBinding `json:"contracts"`
}

// ContractBinding links a schema to its tests
type ContractBinding struct {
	Type      string   `json:"type"`                // openapi, protobuf, graphql
	Path      string   `json:"path"`                // Path to schema file
	Service   string   `json:"service,omitempty"`   // Service/module name
	Tests     []string `json:"tests"`               // Glob patterns for contract tests
	Digest    string   `json:"digest,omitempty"`    // Current fingerprint
	Generated []string `json:"generated,omitempty"` // Generated output paths
}

// CIProvenance captures audit information for the plan
type CIProvenance struct {
	Changeset       string   `json:"changeset,omitempty"`  // Changeset ID used
	Base            string   `json:"base,omitempty"`       // Base snapshot ID
	Head            string   `json:"head,omitempty"`       // Head snapshot ID
	KaiVersion      string   `json:"kaiVersion"`           // Kai CLI version
	DetectorVersion string   `json:"detectorVersion"`      // Dynamic import detector version
	GeneratedAt     string   `json:"generatedAt"`          // ISO8601 timestamp
	Analyzers       []string `json:"analyzers"`            // Which analyzers ran
	PolicyHash      string   `json:"policyHash,omitempty"` // Hash of ci-policy.yaml if used
	EnvHash         string   `json:"envHash,omitempty"`    // Hash of tracked environment variables
}

// DetectorVersion is the current version of the dynamic import detector
const DetectorVersion = "1.0.0"

type CITargets struct {
	Run      []string            `json:"run"`
	Skip     []string            `json:"skip"`
	Full     []string            `json:"full,omitempty"` // All tests (for shadow mode comparison)
	Tags     map[string][]string `json:"tags,omitempty"`
	Fallback bool                `json:"fallback"` // If true, runner should fallback to full on failure
}

type CIImpact struct {
	FilesChanged    []string         `json:"filesChanged"`
	SymbolsChanged  []CISymbolChange `json:"symbolsChanged,omitempty"`
	ModulesAffected []string         `json:"modulesAffected,omitempty"`
	Uncertainty     []string         `json:"uncertainty,omitempty"`
}

type CISymbolChange struct {
	FQ     string `json:"fq"`
	Change string `json:"change"`
}

type CIPolicy struct {
	Strategy     string `json:"strategy"`
	Expanded     bool   `json:"expanded"`
	FallbackUsed string `json:"fallbackUsed,omitempty"`
}

// CISafety contains safety-related flags and risk signals
type CISafety struct {
	StructuralRisks  []StructuralRisk `json:"structuralRisks,omitempty"`
	Confidence       float64          `json:"confidence"`    // 0.0-1.0 confidence in selection
	RecommendFull    bool             `json:"recommendFull"` // True if full run recommended
	RecommendReason  string           `json:"recommendReason,omitempty"`
	PanicSwitch      bool             `json:"panicSwitch"`  // Force full run (env/label override)
	AutoExpanded     bool             `json:"autoExpanded"` // Was selection auto-expanded due to risk?
	ExpansionReasons []string         `json:"expansionReasons,omitempty"`
}

// StructuralRisk represents a detected risk pattern
type StructuralRisk struct {
	Type        string `json:"type"`        // Risk type identifier
	Description string `json:"description"` // Human-readable description
	Severity    string `json:"severity"`    // "low", "medium", "high", "critical"
	FilePath    string `json:"filePath,omitempty"`
	Triggered   bool   `json:"triggered"` // Did this risk trigger expansion?
}

// CIPrediction contains shadow mode prediction data for comparison
type CIPrediction struct {
	SelectiveTests   int     `json:"selectiveTests"`   // Number of tests in selective plan
	FullTests        int     `json:"fullTests"`        // Number of tests in full suite
	PredictedSavings float64 `json:"predictedSavings"` // Percentage of tests saved
	// After running full suite, these get populated for comparison
	MissedFailures []string `json:"missedFailures,omitempty"` // Tests that failed but weren't selected
	FalsePositives []string `json:"falsePositives,omitempty"` // Tests selected but didn't need to run
}

// ShadowVerdict is the outcome of a shadow comparison run
type ShadowVerdict string

const (
	ShadowVerdictSafe         ShadowVerdict = "safe"
	ShadowVerdictMissed       ShadowVerdict = "missed"
	ShadowVerdictFallback     ShadowVerdict = "fallback"
	ShadowVerdictFlakySuspect ShadowVerdict = "flaky_suspect"
	ShadowVerdictSkipped      ShadowVerdict = "skipped"
)

// ShadowReport is the full output of a shadow comparison run
type ShadowReport struct {
	Version      int                `json:"version"`
	GeneratedAt  string             `json:"generatedAt"`
	KaiVersion   string             `json:"kaiVersion"`
	GitRange     string             `json:"gitRange"`
	Plan         *CIPlan            `json:"plan"`
	Verdict      ShadowVerdict      `json:"verdict"`
	SelectiveRun *ShadowRunResult   `json:"selectiveRun"`
	FullRun      *ShadowRunResult   `json:"fullRun,omitempty"`
	Metrics      ShadowMetrics      `json:"metrics"`
	Flaky        ShadowFlakyInfo    `json:"flaky"`
	Fallback     ShadowFallbackInfo `json:"fallback"`
}

// TestFailureDetail captures a failed test with its error message
type TestFailureDetail struct {
	Name         string `json:"name"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// FlakyTestDetail captures per-test flaky classification
type FlakyTestDetail struct {
	Name           string  `json:"name"`
	ErrorMessage   string  `json:"errorMessage,omitempty"`
	FailCount      int     `json:"failCount"`
	TotalRetries   int     `json:"totalRetries"`
	Classification string  `json:"classification"` // "flaky", "real", "inconclusive"
	Confidence     float64 `json:"confidence"`     // 0.0-1.0
}

// FlakyHistoryRecord is appended to .kai/flaky-tests.jsonl
type FlakyHistoryRecord struct {
	Timestamp string            `json:"timestamp"`
	GitRange  string            `json:"gitRange"`
	Tests     []FlakyTestDetail `json:"tests"`
}

// ShadowRunResult captures the result of a single test command execution
type ShadowRunResult struct {
	Command     string              `json:"command"`
	ExitCode    int                 `json:"exitCode"`
	DurationS   float64             `json:"durationS"`
	TotalTests  int                 `json:"totalTests"`
	Passed      int                 `json:"passed"`
	FailedTests []TestFailureDetail `json:"failedTests"`
	Skipped     int                 `json:"skipped"`
}

// ShadowMetrics captures comparison metrics between selective and full runs
type ShadowMetrics struct {
	TestsReduced    int     `json:"testsReduced"`
	TestsReducedPct float64 `json:"testsReducedPct"`
	TimeSavedS      float64 `json:"timeSavedS"`
	TimeSavedPct    float64 `json:"timeSavedPct"`
	FalseNegatives  int     `json:"falseNegatives"`
	FalsePositives  int     `json:"falsePositives"`
	Accuracy        float64 `json:"accuracy"`
}

// ShadowFlakyInfo captures flaky test detection results
type ShadowFlakyInfo struct {
	Detected   bool              `json:"detected"`
	FlakyTests []FlakyTestDetail `json:"flakyTests,omitempty"`
	Retries    int               `json:"retries"`
	RealTests  []FlakyTestDetail `json:"realTests,omitempty"`
}

// ShadowFallbackInfo captures fallback trigger information
type ShadowFallbackInfo struct {
	Triggered  bool    `json:"triggered"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// CIPolicyConfig defines risk thresholds and behavior for CI test selection
// Loaded from kai.ci-policy.yaml in repo root
type CIPolicyConfig struct {
	// Version for schema evolution
	Version int `yaml:"version" json:"version"`

	// Thresholds control when to expand or fail
	Thresholds CIPolicyThresholds `yaml:"thresholds" json:"thresholds"`

	// Paranoia rules define additional patterns that trigger expansion
	Paranoia CIPolicyParanoia `yaml:"paranoia" json:"paranoia"`

	// Behavior defines how to handle different risk levels
	Behavior CIPolicyBehavior `yaml:"behavior" json:"behavior"`

	// DynamicImports configures how to handle dynamic import detection
	DynamicImports CIPolicyDynamicImports `yaml:"dynamicImports" json:"dynamicImports"`

	// Coverage configures coverage-based test selection
	Coverage CIPolicyCoverage `yaml:"coverage" json:"coverage"`

	// Contracts configures contract/schema change detection
	Contracts CIPolicyContracts `yaml:"contracts" json:"contracts"`

	// EnvVars lists environment variable names whose values affect test outcomes.
	// A hash of their current values is recorded in provenance for drift detection.
	EnvVars []string `yaml:"envVars" json:"envVars,omitempty"`
}

// CIPolicyDynamicImports configures dynamic import handling
type CIPolicyDynamicImports struct {
	// Expansion strategy: nearest_module, package, owners, full_suite
	Expansion string `yaml:"expansion" json:"expansion"`
	// OwnersFallback: if module unknown, widen to code owners' suites
	OwnersFallback bool `yaml:"ownersFallback" json:"ownersFallback"`
	// MaxFilesThreshold: if >N files in module, widen by owners instead
	MaxFilesThreshold int `yaml:"maxFilesThreshold" json:"maxFilesThreshold"`
	// BoundedRiskThreshold: bounded imports matching >N files are treated as risky
	BoundedRiskThreshold int `yaml:"boundedRiskThreshold" json:"boundedRiskThreshold"`
	// Allowlist: paths where dynamic imports are known-safe (won't trigger expansion)
	Allowlist []string `yaml:"allowlist" json:"allowlist"`
	// BoundGlobs: map pattern -> test globs for bounded expansion
	BoundGlobs map[string][]string `yaml:"boundGlobs" json:"boundGlobs"`
}

// CIPolicyThresholds defines numeric thresholds for risk handling
type CIPolicyThresholds struct {
	// MinConfidence: expand to full if confidence below this (0.0-1.0)
	MinConfidence float64 `yaml:"minConfidence" json:"minConfidence"`
	// MaxUncertainty: expand if uncertainty score exceeds this (0-100)
	MaxUncertainty int `yaml:"maxUncertainty" json:"maxUncertainty"`
	// MaxFilesChanged: expand if more than N files changed
	MaxFilesChanged int `yaml:"maxFilesChanged" json:"maxFilesChanged"`
	// MaxTestsSkipped: expand if more than N% of tests would be skipped
	MaxTestsSkipped float64 `yaml:"maxTestsSkipped" json:"maxTestsSkipped"`
}

// CIPolicyParanoia defines patterns that trigger extra caution
type CIPolicyParanoia struct {
	// AlwaysFullPatterns: globs that trigger full run when matched
	AlwaysFullPatterns []string `yaml:"alwaysFullPatterns" json:"alwaysFullPatterns"`
	// ExpandOnPatterns: globs that trigger expansion (but not full)
	ExpandOnPatterns []string `yaml:"expandOnPatterns" json:"expandOnPatterns"`
	// RiskMultipliers: boost risk score for certain paths
	RiskMultipliers map[string]float64 `yaml:"riskMultipliers" json:"riskMultipliers"`
}

// CIPolicyBehavior defines how to respond to different conditions
type CIPolicyBehavior struct {
	// OnHighRisk: "expand", "warn", "fail"
	OnHighRisk string `yaml:"onHighRisk" json:"onHighRisk"`
	// OnLowConfidence: "expand", "warn", "fail"
	OnLowConfidence string `yaml:"onLowConfidence" json:"onLowConfidence"`
	// OnNoTests: "expand", "warn", "pass" - what to do when no tests selected
	OnNoTests string `yaml:"onNoTests" json:"onNoTests"`
	// FailOnExpansion: if true, exit non-zero when expansion happens
	FailOnExpansion bool `yaml:"failOnExpansion" json:"failOnExpansion"`
}

// CIPolicyCoverage configures coverage-based test selection
type CIPolicyCoverage struct {
	// Enabled: whether to use coverage data for test selection
	Enabled bool `yaml:"enabled" json:"enabled"`
	// LookbackDays: how far back to look for coverage data (default 30)
	LookbackDays int `yaml:"lookbackDays" json:"lookbackDays"`
	// MinHits: minimum hit count to trust a file→test mapping
	MinHits int `yaml:"minHits" json:"minHits"`
	// OnNoCoverage: "expand", "warn", "ignore" - what to do for files without coverage
	OnNoCoverage string `yaml:"onNoCoverage" json:"onNoCoverage"`
	// RetentionDays: prune coverage entries older than this (default 90)
	RetentionDays int `yaml:"retentionDays" json:"retentionDays"`
}

// CIPolicyContracts configures contract/schema change detection
type CIPolicyContracts struct {
	// Enabled: whether to detect contract/schema changes
	Enabled bool `yaml:"enabled" json:"enabled"`
	// OnChange: "add_tests", "expand", "warn" - action when contract changes
	OnChange string `yaml:"onChange" json:"onChange"`
	// Types: which contract types to detect (openapi, protobuf, graphql)
	Types []string `yaml:"types" json:"types"`
	// RetentionRevisions: keep last N revisions of each contract (default 50)
	RetentionRevisions int `yaml:"retentionRevisions" json:"retentionRevisions"`
	// Generated: map of schema input→output globs for generated file tracking
	Generated []CIPolicyGeneratedMapping `yaml:"generated" json:"generated"`
}

// CIPolicyGeneratedMapping maps a schema to its generated outputs
type CIPolicyGeneratedMapping struct {
	Input   string   `yaml:"input" json:"input"`     // Schema file path
	Outputs []string `yaml:"outputs" json:"outputs"` // Generated file globs
}

// DefaultCIPolicy returns a sensible default policy
func DefaultCIPolicy() CIPolicyConfig {
	return CIPolicyConfig{
		Version: 1,
		Thresholds: CIPolicyThresholds{
			MinConfidence:   0.40, // Below 40% = low confidence
			MaxUncertainty:  70,   // Above 70 = high uncertainty
			MaxFilesChanged: 50,   // More than 50 files is suspicious
			MaxTestsSkipped: 0.90, // Skipping >90% of tests is suspicious
		},
		Paranoia: CIPolicyParanoia{
			AlwaysFullPatterns: []string{
				"*.lock",
				"go.mod",
				"go.sum",
				"package.json",
				"Dockerfile",
				".github/workflows/*",
			},
			ExpandOnPatterns: []string{
				"**/config/**",
				"**/setup.*",
				"**/__mocks__/**",
			},
			RiskMultipliers: map[string]float64{
				"src/core/**": 1.5,
				"lib/**":      1.3,
			},
		},
		Behavior: CIPolicyBehavior{
			OnHighRisk:      "expand",
			OnLowConfidence: "expand",
			OnNoTests:       "warn",
			FailOnExpansion: false,
		},
		DynamicImports: CIPolicyDynamicImports{
			Expansion:            "nearest_module", // nearest_module, package, owners, full_suite
			OwnersFallback:       true,
			MaxFilesThreshold:    200,
			BoundedRiskThreshold: 100, // Bounded imports matching >100 files are treated as risky
			Allowlist:            []string{},
			BoundGlobs:           map[string][]string{},
		},
		Coverage: CIPolicyCoverage{
			Enabled:       true,   // Coverage-based selection enabled by default
			LookbackDays:  30,     // Use coverage from last 30 days
			MinHits:       1,      // Trust mappings with at least 1 hit
			OnNoCoverage:  "warn", // Warn but don't expand for files without coverage
			RetentionDays: 90,     // Prune entries older than 90 days
		},
		Contracts: CIPolicyContracts{
			Enabled:            true,                                       // Contract detection enabled by default
			OnChange:           "add_tests",                                // Add registered tests when contracts change
			Types:              []string{"openapi", "protobuf", "graphql"}, // All supported types
			RetentionRevisions: 50,                                         // Keep last 50 revisions per contract
			Generated:          []CIPolicyGeneratedMapping{},               // User-defined schema→outputs
		},
	}

}

// loadCIPolicy loads the CI policy from .kai/rules/ci-policy.yaml or returns defaults
// Falls back to kai.ci-policy.yaml for backwards compatibility
func loadCIPolicy() (CIPolicyConfig, string, error) {
	policy := DefaultCIPolicy()

	// Try primary location first, then fallback
	var data []byte
	var err error
	var usedPath string

	data, err = os.ReadFile(ciPolicyFile)
	if err == nil {
		usedPath = ciPolicyFile
	} else if os.IsNotExist(err) {
		// Try fallback location
		data, err = os.ReadFile(ciPolicyFileFallback)
		if err == nil {
			usedPath = ciPolicyFileFallback
		} else if os.IsNotExist(err) {
			debugf("ci policy: using defaults (no policy file found)")
			return policy, "", nil // Use defaults, no hash
		}
	}

	if err != nil {
		return policy, "", fmt.Errorf("reading CI policy: %w", err)
	}

	if err := yaml.Unmarshal(data, &policy); err != nil {
		return policy, "", fmt.Errorf("parsing %s: %w", usedPath, err)
	}

	// Compute policy hash for provenance
	hash := sha256.Sum256(data)
	policyHash := hex.EncodeToString(hash[:8]) // First 8 bytes as hex

	debugf("ci policy: loaded from %s (hash: %s)", usedPath, policyHash)
	return policy, policyHash, nil
}

// computeEnvHash produces a stable hash of the environment variables listed
// in the CI policy. Returns "" if no env vars are configured.
func computeEnvHash(vars []string) string {
	if len(vars) == 0 {
		return ""
	}
	sorted := make([]string, len(vars))
	copy(sorted, vars)
	sort.Strings(sorted)

	h := sha256.New()
	for _, name := range sorted {
		val := os.Getenv(name)
		fmt.Fprintf(h, "%s=%s\n", name, val)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// Structural risk type constants
const (
	RiskConfigChange      = "config_change"       // package.json, tsconfig, etc changed
	RiskBuildFileChange   = "build_file_change"   // webpack, vite, build configs
	RiskGlobalChange      = "global_change"       // Global state, env vars, shared constants
	RiskDynamicImport     = "dynamic_import"      // Dynamic require/import detected
	RiskReflection        = "reflection"          // Reflection or metaprogramming
	RiskTestInfra         = "test_infra"          // Test helpers, fixtures, mocks changed
	RiskNoTestMapping     = "no_test_mapping"     // Changed files have no test coverage
	RiskCircularDep       = "circular_dependency" // Circular import detected
	RiskNewFile           = "new_file"            // New file with no test coverage
	RiskDeletedFile       = "deleted_file"        // File was deleted
	RiskManyFilesChanged  = "many_files_changed"  // Too many files changed (>threshold)
	RiskCrossModuleChange = "cross_module_change" // Changes span multiple modules
)

// Config file patterns that affect all tests
var configFilePatterns = []string{
	"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
	"tsconfig.json", "tsconfig.*.json", "jsconfig.json",
	".babelrc", "babel.config.js", "babel.config.json",
	".eslintrc", ".eslintrc.js", ".eslintrc.json",
	"jest.config.js", "jest.config.ts", "jest.config.json",
	"vitest.config.js", "vitest.config.ts",
	"webpack.config.js", "vite.config.js", "vite.config.ts",
	"rollup.config.js", ".env", ".env.*",
	"go.mod", "go.sum", "Cargo.toml", "Cargo.lock",
	"requirements.txt", "setup.py", "pyproject.toml",
	"Makefile", "Dockerfile", "docker-compose.yml",
}

// Test infrastructure patterns
var testInfraPatterns = []string{
	"**/fixtures/**", "**/mocks/**", "**/__mocks__/**",
	"**/testutils/**", "**/test-utils/**", "**/helpers/**",
	"**/setup.*", "**/teardown.*", "**/globalSetup.*",
	"conftest.py", "pytest.ini",
}

// DynamicImportPattern represents a pattern for detecting dynamic imports
type DynamicImportPattern struct {
	pattern    string
	kind       string  // Human-readable kind
	confidence float64 // Base confidence (can be adjusted by context)
	language   string  // js, ts, py, go, or "" for any
}

// Dynamic import patterns that indicate runtime-dependent imports
// These make static analysis unreliable
var dynamicImportPatterns = []DynamicImportPattern{
	// JavaScript/TypeScript - High confidence dynamic imports
	{`require\s*\(\s*[^"'\x60\s]`, "require(variable)", 0.9, "js"},
	{`import\s*\(\s*[^"'\x60\s]`, "import(variable)", 0.9, "js"},
	{`__non_webpack_require__\s*\(`, "webpack bypass", 1.0, "js"},

	// require.resolve - often static but can be dynamic
	{`require\.resolve\s*\(\s*[^"'\x60]`, "require.resolve(variable)", 0.8, "js"},

	// Python - dynamic imports
	{`__import__\s*\(\s*[^"']`, "__import__(variable)", 0.9, "py"},
	{`importlib\.import_module\s*\(\s*[^"']`, "importlib.import_module(variable)", 0.9, "py"},
	{`exec\s*\([^)]*import\s`, "exec(import)", 1.0, "py"},

	// Go - plugin loading
	{`plugin\.Open\s*\(\s*[^"]`, "plugin.Open(variable)", 0.9, "go"},

	// Generic dangerous patterns
	{`eval\s*\([^)]*require`, "eval(require)", 1.0, ""},
	{`eval\s*\([^)]*import`, "eval(import)", 1.0, ""},
}

// False positive patterns - if these match, reduce confidence
var dynamicImportFalsePositives = []struct {
	pattern     string
	description string
	reduction   float64 // Reduce confidence by this amount
}{
	// Constant string concatenation (e.g., require("foo/" + "bar"))
	{`require\s*\(\s*["'\x60][^"'\x60]*["'\x60]\s*\+\s*["'\x60]`, "constant concatenation", 0.8},
	// require.resolve with string literal
	{`require\.resolve\s*\(\s*["'\x60][^"'\x60]+["'\x60]\s*\)`, "require.resolve(literal)", 0.9},
	// path.join with __dirname (commonly static)
	{`require\s*\(\s*path\.join\s*\(\s*__dirname`, "path.join(__dirname)", 0.6},
	// Template literal with only static parts
	{`require\s*\(\s*\x60[^\$\x60]+\x60\s*\)`, "static template literal", 0.9},
}

// Bounding patterns - if these are near a dynamic import, it's bounded
var dynamicImportBounders = []struct {
	pattern     string
	description string
}{
	{`/\*\s*webpackInclude:\s*([^*]+)\*/`, "webpackInclude"},
	{`/\*\s*webpackExclude:\s*([^*]+)\*/`, "webpackExclude"},
	{`/\*\s*webpackChunkName:\s*["']([^"']+)["']\s*\*/`, "webpackChunkName"},
	{`/\*\s*@vite-ignore\s*\*/`, "vite-ignore"},
}

// FileContentReader is a function that reads file content by path
type FileContentReader func(path string) ([]byte, error)

// detectDynamicImports checks if a file contains dynamic import patterns
// Returns simple bool/string for backward compatibility
func detectDynamicImports(content []byte, filePath string) (bool, string) {
	files := detectDynamicImportsDetailed(content, filePath, nil)
	if len(files) > 0 {
		return true, files[0].Kind
	}
	return false, ""
}

// detectDynamicImportsDetailed provides detailed dynamic import detection
// with false positive reduction and bounding detection
func detectDynamicImportsDetailed(content []byte, filePath string, policy *CIPolicyDynamicImports) []DynamicImportFile {
	var results []DynamicImportFile
	contentStr := string(content)
	lines := strings.Split(contentStr, "\n")

	// Determine file language from extension
	ext := strings.ToLower(filepath.Ext(filePath))
	var lang string
	switch ext {
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx":
		lang = "js"
	case ".py":
		lang = "py"
	case ".go":
		lang = "go"
	}

	// Check each pattern
	for _, p := range dynamicImportPatterns {
		// Skip patterns for other languages
		if p.language != "" && p.language != lang {
			continue
		}

		re, err := regexp.Compile(p.pattern)
		if err != nil {
			continue
		}

		matches := re.FindAllStringIndex(contentStr, -1)
		for _, match := range matches {
			// Find line number
			lineNum := 1
			for i := range lines {
				if match[0] <= len(strings.Join(lines[:i+1], "\n")) {
					lineNum = i + 1
					break
				}
			}

			// Start with base confidence
			confidence := p.confidence

			// Check for false positive patterns (reduce confidence)
			for _, fp := range dynamicImportFalsePositives {
				fpRe, err := regexp.Compile(fp.pattern)
				if err != nil {
					continue
				}
				// Check in a window around the match
				start := match[0]
				if start > 50 {
					start = match[0] - 50
				}
				end := match[1] + 50
				if end > len(contentStr) {
					end = len(contentStr)
				}
				window := contentStr[start:end]
				if fpRe.MatchString(window) {
					confidence -= fp.reduction
				}
			}

			// Check for bounding patterns BEFORE confidence skip
			// This is important: we only skip low-confidence imports if bounded
			bounded := false
			boundedBy := ""
			for _, b := range dynamicImportBounders {
				bRe, err := regexp.Compile(b.pattern)
				if err != nil {
					continue
				}
				// Check in a window before the match (comments come before)
				start := match[0] - 200
				if start < 0 {
					start = 0
				}
				window := contentStr[start:match[0]]
				if bMatch := bRe.FindStringSubmatch(window); bMatch != nil {
					bounded = true
					if len(bMatch) > 1 {
						boundedBy = fmt.Sprintf("%s: %s", b.description, strings.TrimSpace(bMatch[1]))
					} else {
						boundedBy = b.description
					}
					break
				}
			}

			// Skip ONLY if confidence is very low AND the import is bounded
			// Unbounded imports with low confidence are still risky - include them
			if confidence <= 0.1 && bounded {
				continue
			}

			// Check allowlist
			allowlisted := false
			if policy != nil {
				for _, allow := range policy.Allowlist {
					matched, _ := doublestar.Match(allow, filePath)
					if matched {
						allowlisted = true
						break
					}
				}
			}

			results = append(results, DynamicImportFile{
				Path:        filePath,
				Kind:        p.kind,
				Line:        lineNum,
				Bounded:     bounded,
				BoundedBy:   boundedBy,
				Allowlisted: allowlisted,
				Confidence:  confidence,
			})
		}
	}

	return results
}

// expandForDynamicImports performs scoped expansion based on dynamic import detection
// Uses union model: nearest_module → owners → full_suite (with fallback)
// Returns the list of additional test files to add and expansion info
func expandForDynamicImports(
	detectedImports []DynamicImportFile,
	policy *CIPolicyDynamicImports,
	allTestFiles []string,
	changedFiles []string,
	moduleMappings []ModulePathMapping,
	filesByModule map[string][]string, // map[module name] -> test files in that module
) ([]string, *DynamicImportInfo) {
	info := &DynamicImportInfo{
		Detected: len(detectedImports) > 0,
		Files:    make([]DynamicImportFile, len(detectedImports)),
		Policy: DynamicImportPolicyUsed{
			Expansion:      policy.Expansion,
			OwnersFallback: policy.OwnersFallback,
		},
		Telemetry: DynamicImportTelemetry{
			TotalDetected: len(detectedImports),
		},
	}
	copy(info.Files, detectedImports)

	if len(detectedImports) == 0 {
		return nil, info
	}

	// Classify imports: allowlisted, bounded (safe), bounded-risky, unbounded
	var importsToExpand []DynamicImportFile
	for i, imp := range info.Files {
		if imp.Allowlisted {
			info.Telemetry.Allowlisted++
			continue
		}

		if imp.Bounded {
			// Check if bounded import has a huge footprint (bounded-but-risky)
			if policy.BoundedRiskThreshold > 0 && imp.BoundedBy != "" {
				// Estimate footprint by checking how many files match the bound pattern
				footprint := estimateBoundedFootprint(imp.BoundedBy, allTestFiles)
				if footprint > policy.BoundedRiskThreshold {
					info.Files[i].BoundedRisky = true
					info.Telemetry.BoundedRisky++
					importsToExpand = append(importsToExpand, info.Files[i])
					continue
				}
			}
			info.Telemetry.Bounded++
			continue
		}

		// Unbounded - needs expansion
		info.Telemetry.Unbounded++
		importsToExpand = append(importsToExpand, imp)
	}

	// Nothing to expand
	if len(importsToExpand) == 0 {
		info.Telemetry.StrategyUsed = "none (all safe)"
		return nil, info
	}

	var expandedTests []string
	expandedTestSet := make(map[string]bool)
	strategyUsed := policy.Expansion

	// Union model: try nearest_module first, fall back to owners, then full_suite
	for _, imp := range importsToExpand {
		impTests, impStrategy := expandSingleImport(imp, policy, allTestFiles, moduleMappings, filesByModule)

		// Track what this import expanded to
		for j := range info.Files {
			if info.Files[j].Path == imp.Path && info.Files[j].Line == imp.Line {
				if len(impTests) > 0 {
					info.Files[j].ExpandedTo = impStrategy
				}
				break
			}
		}

		// Add tests to set
		for _, t := range impTests {
			if !expandedTestSet[t] {
				expandedTestSet[t] = true
				expandedTests = append(expandedTests, t)
			}
		}

		// Track if we had to escalate
		if impStrategy == "full_suite" {
			strategyUsed = "full_suite"
		} else if impStrategy == "owners" && strategyUsed != "full_suite" {
			strategyUsed = "owners"
		}
	}

	// Apply MaxFilesThreshold - if expansion exceeds threshold, widen to full suite
	if policy.MaxFilesThreshold > 0 && len(expandedTests) > policy.MaxFilesThreshold {
		expandedTests = allTestFiles
		expandedTestSet = make(map[string]bool)
		for _, t := range allTestFiles {
			expandedTestSet[t] = true
		}
		strategyUsed = "full_suite (threshold exceeded)"
	}

	info.Policy.Expansion = strategyUsed
	info.Policy.ExpandedTo = expandedTests
	info.Telemetry.WidenedTests = len(expandedTests)
	info.Telemetry.StrategyUsed = strategyUsed

	return expandedTests, info
}

// expandSingleImport expands a single dynamic import using the union model
// Returns tests to add and the strategy that was actually used
func expandSingleImport(
	imp DynamicImportFile,
	policy *CIPolicyDynamicImports,
	allTestFiles []string,
	moduleMappings []ModulePathMapping,
	filesByModule map[string][]string,
) ([]string, string) {
	var tests []string
	fileDir := filepath.Dir(imp.Path)

	// Strategy 1: nearest_module
	if policy.Expansion == "nearest_module" || policy.Expansion == "package" || policy.OwnersFallback {
		// Find which module this file belongs to using path prefixes
		var foundModule string
		for _, mod := range moduleMappings {
			for _, prefix := range mod.PathPrefixes {
				if strings.HasPrefix(fileDir, prefix) || strings.HasPrefix(imp.Path, prefix) {
					foundModule = mod.Name
					if modTests, ok := filesByModule[mod.Name]; ok {
						tests = append(tests, modTests...)
					}
					break
				}
			}
			if foundModule != "" {
				break
			}
		}

		if len(tests) > 0 {
			return tests, "nearest_module: " + foundModule
		}
	}

	// Strategy 2: package (same directory)
	if policy.Expansion == "package" || policy.OwnersFallback {
		for _, t := range allTestFiles {
			testDir := filepath.Dir(t)
			if strings.HasPrefix(testDir, fileDir) || testDir == fileDir {
				tests = append(tests, t)
			}
		}

		if len(tests) > 0 {
			return tests, "package: " + fileDir
		}
	}

	// Strategy 3: owners (would parse CODEOWNERS - for now use parent directories)
	if policy.Expansion == "owners" || policy.OwnersFallback {
		// Expand to parent directory tests as a proxy for "team ownership"
		parentDir := filepath.Dir(fileDir)
		for _, t := range allTestFiles {
			testDir := filepath.Dir(t)
			if strings.HasPrefix(testDir, parentDir) {
				tests = append(tests, t)
			}
		}

		if len(tests) > 0 {
			return tests, "owners: " + parentDir
		}
	}

	// Strategy 4: full_suite (nuclear fallback)
	return allTestFiles, "full_suite"
}

// estimateBoundedFootprint estimates how many files a bounded pattern matches
func estimateBoundedFootprint(boundedBy string, allFiles []string) int {
	// Extract the pattern from the boundedBy string (e.g., "webpackInclude: /\.widget\.js$/")
	// For now, do a simple heuristic based on common patterns

	// Check for very broad patterns
	broadPatterns := []string{"**/*", "/**", ".*", ".+"}
	for _, broad := range broadPatterns {
		if strings.Contains(boundedBy, broad) {
			return len(allFiles) // Assume matches everything
		}
	}

	// Check for directory-specific patterns
	if strings.Contains(boundedBy, "/plugins/") || strings.Contains(boundedBy, "/components/") {
		// These are typically large directories
		count := 0
		for _, f := range allFiles {
			if strings.Contains(f, "/plugins/") || strings.Contains(f, "/components/") {
				count++
			}
		}
		return count
	}

	// Default: assume reasonable footprint
	return 10
}

// ModulePathMapping maps module names to their path prefixes for matching
type ModulePathMapping struct {
	Name         string   // Module name (e.g., "App")
	PathPrefixes []string // Extracted path prefixes (e.g., ["src/app", "lib/app"])
}

// extractPathPrefix converts a glob pattern to a path prefix for matching.
// For example: "src/app/**" -> "src/app", "lib/*.js" -> "lib"
func extractPathPrefix(pattern string) string {
	// Remove trailing glob patterns
	prefix := pattern

	// Remove common glob suffixes
	suffixes := []string{"/**", "/*", "**/*", "**"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(prefix, suffix) {
			prefix = strings.TrimSuffix(prefix, suffix)
			break
		}
	}

	// If pattern contains wildcards in the middle, take up to the first wildcard
	if idx := strings.IndexAny(prefix, "*?["); idx != -1 {
		prefix = prefix[:idx]
		// Trim trailing slash if present
		prefix = strings.TrimSuffix(prefix, "/")
	}

	// If we ended up with empty string, use the directory of the pattern
	if prefix == "" {
		prefix = filepath.Dir(pattern)
		if prefix == "." {
			prefix = ""
		}
	}

	return prefix
}

// buildModulePathMappings builds path mappings for modules using their configured glob patterns
func buildModulePathMappings(matcher *module.Matcher, moduleNames []string) []ModulePathMapping {
	var mappings []ModulePathMapping

	for _, name := range moduleNames {
		mod := matcher.GetModule(name)
		if mod == nil {
			continue
		}

		var prefixes []string
		seen := make(map[string]bool)
		for _, pattern := range mod.Paths {
			prefix := extractPathPrefix(pattern)
			if prefix != "" && !seen[prefix] {
				prefixes = append(prefixes, prefix)
				seen[prefix] = true
			}
		}

		if len(prefixes) > 0 {
			mappings = append(mappings, ModulePathMapping{
				Name:         name,
				PathPrefixes: prefixes,
			})
		}
	}

	return mappings
}

// buildModuleTestMap builds a map of module name -> test files in that module
// Uses path prefixes from module configurations for accurate matching
func buildModuleTestMap(testFiles []string, moduleMappings []ModulePathMapping) map[string][]string {
	result := make(map[string][]string)
	for _, mod := range moduleMappings {
		result[mod.Name] = []string{}
	}

	for _, t := range testFiles {
		testDir := filepath.Dir(t)
		for _, mod := range moduleMappings {
			for _, prefix := range mod.PathPrefixes {
				if strings.HasPrefix(testDir, prefix) || strings.HasPrefix(t, prefix) {
					result[mod.Name] = append(result[mod.Name], t)
					break // Only add once per module
				}
			}
		}
	}

	return result
}

// detectStructuralRisks analyzes changed files for patterns that indicate
// higher risk and should trigger expansion in Guarded mode.
func detectStructuralRisks(changedFiles []string, affectedTests map[string]bool, allTestFiles []string, modules []string) []StructuralRisk {
	return detectStructuralRisksWithContent(changedFiles, affectedTests, allTestFiles, modules, nil)
}

// detectStructuralRisksWithContent is like detectStructuralRisks but also checks file content
func detectStructuralRisksWithContent(changedFiles []string, affectedTests map[string]bool, allTestFiles []string, modules []string, readContent FileContentReader) []StructuralRisk {
	var risks []StructuralRisk

	// Check each changed file for risk patterns
	for _, file := range changedFiles {
		basename := filepath.Base(file)

		// Check for config file changes
		for _, pattern := range configFilePatterns {
			matched, _ := filepath.Match(pattern, basename)
			if matched {
				risks = append(risks, StructuralRisk{
					Type:        RiskConfigChange,
					Description: fmt.Sprintf("Config file changed: %s - may affect all tests", file),
					Severity:    "high",
					FilePath:    file,
					Triggered:   true,
				})
				break
			}
		}

		// Check for test infrastructure changes
		for _, pattern := range testInfraPatterns {
			matched, _ := doublestar.Match(pattern, file)
			if matched {
				risks = append(risks, StructuralRisk{
					Type:        RiskTestInfra,
					Description: fmt.Sprintf("Test infrastructure changed: %s - may affect many tests", file),
					Severity:    "high",
					FilePath:    file,
					Triggered:   true,
				})
				break
			}
		}

		// Check if changed file has no test coverage
		if !parse.IsTestFile(file) && !affectedTests[file] {
			hasMapping := false
			for testPath := range affectedTests {
				// Check if any test was found for this file
				if testPath != "" {
					hasMapping = true
					break
				}
			}
			if !hasMapping && len(affectedTests) == 0 {
				risks = append(risks, StructuralRisk{
					Type:        RiskNoTestMapping,
					Description: fmt.Sprintf("No test mapping found for: %s", file),
					Severity:    "medium",
					FilePath:    file,
					Triggered:   false, // Don't auto-expand for individual files
				})
			}
		}

		// Check for dynamic imports if we have a content reader
		if readContent != nil {
			// Only check source files that might have dynamic imports
			ext := strings.ToLower(filepath.Ext(file))
			if ext == ".js" || ext == ".ts" || ext == ".tsx" || ext == ".jsx" ||
				ext == ".mjs" || ext == ".cjs" || ext == ".py" || ext == ".go" {
				content, err := readContent(file)
				if err == nil && len(content) > 0 {
					if hasDynamic, desc := detectDynamicImports(content, file); hasDynamic {
						risks = append(risks, StructuralRisk{
							Type:        RiskDynamicImport,
							Description: fmt.Sprintf("Dynamic import in %s: %s - static analysis may be incomplete", file, desc),
							Severity:    "high",
							FilePath:    file,
							Triggered:   true, // Auto-expand because we can't trust the dependency graph
						})
					}
				}
			}
		}
	}

	// Check for too many files changed
	const manyFilesThreshold = 20
	if len(changedFiles) > manyFilesThreshold {
		risks = append(risks, StructuralRisk{
			Type:        RiskManyFilesChanged,
			Description: fmt.Sprintf("Many files changed (%d) - consider running full suite", len(changedFiles)),
			Severity:    "medium",
			FilePath:    "",
			Triggered:   false, // Warning only, don't auto-expand
		})
	}

	// Check for cross-module changes
	if len(modules) > 2 {
		risks = append(risks, StructuralRisk{
			Type:        RiskCrossModuleChange,
			Description: fmt.Sprintf("Changes span %d modules - increased risk of missed dependencies", len(modules)),
			Severity:    "medium",
			FilePath:    "",
			Triggered:   false, // Warning only
		})
	}

	return risks
}

// calculateConfidence returns a confidence score (0.0-1.0) based on the risk signals
func calculateConfidence(risks []StructuralRisk, testsFound int, changedFiles int) float64 {
	if changedFiles == 0 {
		return 1.0 // No changes = max confidence
	}

	// Start with base confidence
	confidence := 0.8

	// Reduce confidence for each high-severity risk
	for _, risk := range risks {
		switch risk.Severity {
		case "critical":
			confidence -= 0.3
		case "high":
			confidence -= 0.2
		case "medium":
			confidence -= 0.1
		case "low":
			confidence -= 0.05
		}
	}

	// Reduce confidence if no tests found
	if testsFound == 0 {
		confidence -= 0.3
	}

	// Reduce confidence for many changes
	if changedFiles > 10 {
		confidence -= 0.1
	}
	if changedFiles > 20 {
		confidence -= 0.1
	}

	// Clamp to valid range
	if confidence < 0.0 {
		confidence = 0.0
	}
	if confidence > 1.0 {
		confidence = 1.0
	}

	return confidence
}

// shouldExpandForSafety determines if the selection should be expanded based on safety mode and risks
func shouldExpandForSafety(safetyMode string, risks []StructuralRisk, confidence float64, policy CIPolicyConfig) (bool, []string) {
	var reasons []string

	switch safetyMode {
	case "shadow":
		// Shadow mode: never expand, just observe
		return false, nil

	case "strict":
		// Strict mode: never auto-expand (but panic switch can still force full)
		return false, nil

	case "guarded":
		// Use policy thresholds for expansion decisions
		minConfidence := policy.Thresholds.MinConfidence
		if minConfidence == 0 {
			minConfidence = 0.3 // Default fallback
		}

		// Guarded mode: expand if any high-severity triggered risk
		for _, risk := range risks {
			if risk.Triggered && (risk.Severity == "high" || risk.Severity == "critical") {
				reasons = append(reasons, risk.Description)
			}
		}

		// Expand if confidence is below policy threshold
		if confidence < minConfidence {
			reasons = append(reasons, fmt.Sprintf("Low confidence score: %.0f%% (threshold: %.0f%%)", confidence*100, minConfidence*100))
		}

		return len(reasons) > 0, reasons
	}

	return false, nil
}

// checkPanicSwitch checks environment variables and returns true if full run is forced
func checkPanicSwitch() bool {
	// Check for panic switch environment variables
	if os.Getenv("KAI_FORCE_FULL") == "1" || os.Getenv("KAI_FORCE_FULL") == "true" {
		return true
	}
	if os.Getenv("KAI_PANIC") == "1" || os.Getenv("KAI_PANIC") == "true" {
		return true
	}
	return false
}

// getAllTestFiles returns all test files from the file list
func getAllTestFiles(files []*graph.Node) []string {
	var tests []string
	for _, f := range files {
		path, _ := f.Payload["path"].(string)
		if parse.IsTestFile(path) {
			tests = append(tests, path)
		}
	}
	sort.Strings(tests)
	return tests
}

func runCIPlan(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("ci_plan")
	defer te.Finish()

	var db *graph.DB
	var err error
	var baseSnapshotID, headSnapshotID []byte
	var changesetID []byte // Track for provenance
	var changedFiles []string
	var matcher *module.Matcher
	var creator *snapshot.Creator
	var cleanupFunc func() // For temp dir cleanup

	// Handle --git-range mode: create ephemeral DB and snapshots from git
	if ciGitRange != "" {
		// Parse BASE..HEAD format
		parts := strings.Split(ciGitRange, "..")
		if len(parts) != 2 {
			return fmt.Errorf("invalid --git-range format: expected BASE..HEAD (e.g., main..feature)")
		}
		gitBase, gitHead := parts[0], parts[1]

		// Fast path: use git diff + coverage map when available
		coverageMap := loadOrCreateCoverageMap()
		hasCoverage := coverageMap != nil && len(coverageMap.Entries) > 0
		if hasCoverage && !ciNoFast {
			plan, _, err := generateCIPlanFast(gitBase, gitHead, ciGitRepo, coverageMap)
			if err == nil {
				return outputCIPlan(*plan, args)
			}
			fmt.Fprintf(os.Stderr, "Fast path failed (%v), falling back to full snapshot\n", err)
		}

		// Create temp database
		tmpDir, err := os.MkdirTemp("", "kai-ci-*")
		if err != nil {
			return fmt.Errorf("creating temp dir: %w", err)
		}
		cleanupFunc = func() { os.RemoveAll(tmpDir) }
		defer cleanupFunc()

		dbPath := filepath.Join(tmpDir, "db.sqlite")
		objDir := filepath.Join(tmpDir, "objects")
		os.MkdirAll(objDir, 0755)
		db, err = graph.Open(dbPath, objDir)
		if err != nil {
			return fmt.Errorf("creating temp database: %w", err)
		}
		defer db.Close()

		// Apply schema to temp database
		if err := applyDBSchema(db); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Creating snapshot from git ref: %s\n", gitBase)
		baseSnapshotID, err = createSnapshotFromGitRef(db, ciGitRepo, gitBase)
		if err != nil {
			return fmt.Errorf("creating base snapshot: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Creating snapshot from git ref: %s\n", gitHead)
		headSnapshotID, err = createSnapshotFromGitRef(db, ciGitRepo, gitHead)
		if err != nil {
			return fmt.Errorf("creating head snapshot: %w", err)
		}

		// Create changeset
		fmt.Fprintf(os.Stderr, "Creating changeset...\n")
		changesetID, err = createChangesetFromSnapshots(db, baseSnapshotID, headSnapshotID, "")
		if err != nil {
			return fmt.Errorf("creating changeset: %w", err)
		}

		// Set up matcher and creator for the rest of the function
		matcher, _ = loadMatcher()
		if matcher == nil {
			matcher = module.NewMatcher(nil)
		}
		creator = snapshot.NewCreator(db, matcher)

	} else {
		// Normal mode: use existing .kai database
		db, err = openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		matcher, err = loadMatcher()
		if err != nil {
			return err
		}

		creator = snapshot.NewCreator(db, matcher)

		// Resolve the selector - could be changeset, workspace, or snapshot pair
		// Default to @cs:last if no argument given
		selector := "@cs:last"
		if len(args) > 0 {
			selector = args[0]
		}

		// Try to resolve as changeset first
		// Accept both @cs: prefix and raw hex IDs
		isChangesetSelector := strings.HasPrefix(selector, "@cs:")
		if !isChangesetSelector && len(selector) >= 8 && !strings.HasPrefix(selector, "@") {
			// Try to resolve as raw changeset ID
			if csID, err := util.HexToBytes(selector); err == nil {
				if node, _ := db.GetNode(csID); node != nil && node.Kind == graph.KindChangeSet {
					isChangesetSelector = true
					selector = "@cs:" + selector // Normalize for resolver
				}
			}
		}
		if isChangesetSelector {
			csID, err := resolveChangeSetID(db, selector)
			changesetID = csID // Save for provenance
			if err != nil {
				return fmt.Errorf("resolving changeset: %w", err)
			}

			// Get the changeset's base and head snapshots from payload
			cs, err := db.GetNode(csID)
			if err != nil {
				return fmt.Errorf("getting changeset: %w", err)
			}

			// Get base and head from changeset payload
			// Note: EdgeHas edges point to ChangeType nodes, not snapshots
			if headHex, ok := cs.Payload["head"].(string); ok {
				headSnapshotID, _ = util.HexToBytes(headHex)
			}
			if baseHex, ok := cs.Payload["base"].(string); ok {
				baseSnapshotID, _ = util.HexToBytes(baseHex)
			}
		}

		// If we couldn't resolve snapshots from changeset, try workspace
		if headSnapshotID == nil && strings.HasPrefix(selector, "@ws:") {
			wsName := strings.TrimPrefix(selector, "@ws:")
			mgr := workspace.NewManager(db)
			ws, err := mgr.Get(wsName)
			if err != nil {
				return fmt.Errorf("resolving workspace: %w", err)
			}
			baseSnapshotID = ws.BaseSnapshot
			headSnapshotID = ws.HeadSnapshot
		}

		// Fallback: try as snapshot selector (use with @snap:prev)
		if headSnapshotID == nil {
			headSnapshotID, err = resolveSnapshotID(db, selector)
			if err != nil {
				return fmt.Errorf("could not resolve selector '%s' as changeset, workspace, or snapshot", selector)
			}
			// Try to get previous snapshot as base
			baseSnapshotID, _ = resolveSnapshotID(db, "@snap:prev")
		}

		if headSnapshotID == nil {
			return fmt.Errorf("could not determine head snapshot from selector")
		}
	} // End of else block (normal mode)

	// Get changed files
	changedFiles, err = getChangedFiles(db, creator, baseSnapshotID, headSnapshotID)
	if err != nil {
		return fmt.Errorf("getting changed files: %w", err)
	}
	debugf("ci plan: %d changed files", len(changedFiles))

	// Check panic switch first
	panicSwitch := checkPanicSwitch()

	// Load CI policy configuration
	ciPolicy, policyHash, err := loadCIPolicy()
	if err != nil {
		return fmt.Errorf("loading CI policy: %w", err)
	}
	envHash := computeEnvHash(ciPolicy.EnvVars)
	if envHash != "" {
		debugf("ci plan: env hash=%s (%d vars)", envHash, len(ciPolicy.EnvVars))
	}
	debugf("ci plan: strategy=%s safety-mode=%s risk-policy=%s", ciStrategy, ciSafetyMode, ciRiskPolicy)

	// Build provenance info
	var changesetHex, baseHex, headHex string
	if changesetID != nil {
		changesetHex = util.BytesToHex(changesetID)
	}
	if baseSnapshotID != nil {
		baseHex = util.BytesToHex(baseSnapshotID)
	}
	if headSnapshotID != nil {
		headHex = util.BytesToHex(headSnapshotID)
	}

	// Track which analyzers we use
	analyzersUsed := []string{}

	// Build the plan
	plan := CIPlan{
		Version:    1,
		Mode:       "selective",
		Risk:       "low",
		SafetyMode: ciSafetyMode,
		Confidence: 1.0,
		Targets: CITargets{
			Run:      []string{},
			Skip:     []string{},
			Full:     []string{}, // Will be populated for shadow mode
			Tags:     make(map[string][]string),
			Fallback: ciSafetyMode == "guarded", // Enable fallback in guarded mode
		},
		Impact: CIImpact{
			FilesChanged:    changedFiles,
			SymbolsChanged:  []CISymbolChange{},
			ModulesAffected: []string{},
			Uncertainty:     []string{},
		},
		Policy: CIPolicy{
			Strategy:     ciStrategy,
			Expanded:     false,
			FallbackUsed: "",
		},
		Safety: CISafety{
			StructuralRisks:  []StructuralRisk{},
			Confidence:       1.0,
			RecommendFull:    false,
			PanicSwitch:      panicSwitch,
			AutoExpanded:     false,
			ExpansionReasons: []string{},
		},
		Uncertainty: CIUncertainty{
			Score:   0,
			Sources: []string{},
		},
		ExpansionLog: []string{},
		Provenance: CIProvenance{
			Changeset:       changesetHex,
			Base:            baseHex,
			Head:            headHex,
			KaiVersion:      Version,
			DetectorVersion: DetectorVersion,
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			Analyzers:       analyzersUsed, // Will be populated as we analyze
			PolicyHash:      policyHash,
			EnvHash:         envHash,
		},
		Prediction: CIPrediction{},
	}

	// Get all files in the snapshot for graph context (need this for all paths)
	files, err := creator.GetSnapshotFiles(headSnapshotID)
	if err != nil {
		return fmt.Errorf("getting snapshot files: %w", err)
	}

	// Get all test files for full suite reference
	allTestFiles := getAllTestFiles(files)

	// Handle panic switch - forces full run regardless of mode
	if panicSwitch {
		plan.Mode = "full"
		plan.Risk = "low"
		plan.Targets.Run = allTestFiles
		plan.Safety.RecommendFull = true
		plan.Safety.RecommendReason = "Panic switch activated (KAI_FORCE_FULL or KAI_PANIC env var)"

		// For shadow mode, still track prediction data
		if ciSafetyMode == "shadow" {
			plan.Prediction.SelectiveTests = 0 // Would have been 0 before panic
			plan.Prediction.FullTests = len(allTestFiles)
			plan.Prediction.PredictedSavings = 0.0
		}
	} else if len(changedFiles) == 0 {
		// No changes = nothing to do
		plan.Mode = "skip"
		plan.Risk = "low"
		plan.Safety.Confidence = 1.0
	} else {
		// Find affected targets based on strategy
		affectedTargets := make(map[string]bool)
		fallbackUsed := ""

		filePathByID := make(map[string]string)
		fileIDByPath := make(map[string][]byte)
		for _, f := range files {
			path, _ := f.Payload["path"].(string)
			idHex := util.BytesToHex(f.ID)
			filePathByID[idHex] = path
			fileIDByPath[path] = f.ID
		}

		// Try strategies in order: symbols -> imports -> coverage
		strategies := []string{ciStrategy}
		if ciStrategy == "auto" {
			strategies = []string{"symbols", "imports", "coverage"}
		}

		for _, strat := range strategies {
			switch strat {
			case "symbols":
				// Try symbol-level analysis
				analyzersUsed = append(analyzersUsed, "symbols@1")
				// For now, fall through to imports
				continue

			case "imports":
				analyzersUsed = append(analyzersUsed, "imports@1")
				// Use file-level import graph
				for _, changedPath := range changedFiles {
					// Find test files that test this file BY PATH
					// This handles content-addressed ID changes when files are modified
					testsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeTests)
					for _, e := range testsEdges {
						srcHex := util.BytesToHex(e.Src)
						if path, ok := filePathByID[srcHex]; ok {
							affectedTargets[path] = true
						} else {
							// Source file might not be in current snapshot's filePathByID
							// Query the node directly to get its path
							if srcNode, err := db.GetNode(e.Src); err == nil && srcNode != nil {
								if srcPath, ok := srcNode.Payload["path"].(string); ok {
									affectedTargets[srcPath] = true
								}
							}
						}
					}

					// Find files that import this file and are tests (also by path)
					importsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeImports)
					for _, e := range importsEdges {
						srcHex := util.BytesToHex(e.Src)
						if path, ok := filePathByID[srcHex]; ok {
							if parse.IsTestFile(path) {
								affectedTargets[path] = true
							}
						} else {
							// Query node directly
							if srcNode, err := db.GetNode(e.Src); err == nil && srcNode != nil {
								if srcPath, ok := srcNode.Payload["path"].(string); ok {
									if parse.IsTestFile(srcPath) {
										affectedTargets[srcPath] = true
									}
								}
							}
						}
					}
				}

				if len(affectedTargets) > 0 {
					fallbackUsed = "imports"
					break
				}

			case "coverage":
				// Skip if coverage is disabled in policy
				if !ciPolicy.Coverage.Enabled {
					continue
				}

				analyzersUsed = append(analyzersUsed, "coverage@1")
				// Use coverage map to find tests that cover changed files
				coverageMap := loadOrCreateCoverageMap()

				// Check if coverage data is too old based on policy
				lookbackDays := ciPolicy.Coverage.LookbackDays
				if lookbackDays == 0 {
					lookbackDays = 30 // Default
				}

				// Track coverage stats
				filesWithCoverage := 0
				filesWithoutCoverage := 0
				testsFromCoverage := make(map[string]bool)

				for _, changedPath := range changedFiles {
					// Skip test files themselves
					if parse.IsTestFile(changedPath) {
						continue
					}

					// Check if we have coverage data for this file
					entries, hasCoverage := coverageMap.Entries[changedPath]
					if !hasCoverage {
						// Also try relative path variants
						for mapPath, mapEntries := range coverageMap.Entries {
							if strings.HasSuffix(mapPath, changedPath) || strings.HasSuffix(changedPath, mapPath) {
								entries = mapEntries
								hasCoverage = true
								break
							}
						}
					}

					if hasCoverage && len(entries) > 0 {
						filesWithCoverage++
						for _, entry := range entries {
							// Filter by MinHits policy
							if entry.HitCount >= ciPolicy.Coverage.MinHits && entry.TestID != "aggregate" && entry.TestID != "" {
								testsFromCoverage[entry.TestID] = true
							}
						}
					} else {
						filesWithoutCoverage++
					}
				}

				// Add tests from coverage to affected targets
				for testPath := range testsFromCoverage {
					affectedTargets[testPath] = true
				}

				// Build coverage info for the plan
				var coverageAge string
				if coverageMap.IngestedAt != "" {
					coverageAge = coverageMap.IngestedAt
				}

				plan.Coverage = &CoverageInfo{
					Enabled:              true,
					LookbackDays:         lookbackDays,
					FilesWithCoverage:    filesWithCoverage,
					FilesWithoutCoverage: filesWithoutCoverage,
					TestsFromCoverage:    mapKeysToSortedSlice(testsFromCoverage),
					CoverageMapAge:       coverageAge,
				}

				// If files without coverage, increase uncertainty
				if filesWithoutCoverage > 0 {
					plan.Uncertainty.Score += 10 * filesWithoutCoverage
					plan.Uncertainty.Sources = append(plan.Uncertainty.Sources,
						fmt.Sprintf("no_coverage_data:%d_files", filesWithoutCoverage))
				}

				if len(affectedTargets) > 0 {
					fallbackUsed = "coverage"
					break
				}
			}

			if len(affectedTargets) > 0 {
				break
			}
		}

		// === CONTRACT DETECTION ===
		// Check if any changed files are registered contract schemas
		if ciPolicy.Contracts.Enabled {
			contractRegistry := loadOrCreateContractRegistry()
			if len(contractRegistry.Contracts) > 0 {
				var schemasChanged []ContractChange
				testsFromContracts := make(map[string]bool)
				generatedChanged := make(map[string]bool)

				// Build a set of changed file paths for quick lookup
				changedSet := make(map[string]bool)
				for _, p := range changedFiles {
					changedSet[p] = true
				}

				for _, contract := range contractRegistry.Contracts {
					// Check if this contract schema was changed
					if changedSet[contract.Path] {
						// Read current schema and compute digest
						currentDigest := ""
						if data, err := os.ReadFile(contract.Path); err == nil {
							currentDigest = util.Blake3HashHex(data)
						}

						// If digest changed from registered, this is a schema change
						if currentDigest != "" && currentDigest != contract.Digest {
							change := ContractChange{
								Path:         contract.Path,
								Type:         contract.Type,
								Service:      contract.Service,
								DigestBefore: contract.Digest,
								DigestAfter:  currentDigest,
								Tests:        contract.Tests,
							}
							schemasChanged = append(schemasChanged, change)

							// Add registered tests for this contract
							for _, testPath := range contract.Tests {
								testsFromContracts[testPath] = true
								affectedTargets[testPath] = true
							}
						}
					}

					// Check if any generated files from this contract changed
					for _, genPath := range contract.Generated {
						if changedSet[genPath] {
							generatedChanged[genPath] = true
							// Also add the contract tests when generated files change
							for _, testPath := range contract.Tests {
								testsFromContracts[testPath] = true
								affectedTargets[testPath] = true
							}
						}
					}
				}

				// Build contract info for the plan
				if len(schemasChanged) > 0 || len(generatedChanged) > 0 {
					plan.Contracts = &ContractInfo{
						Changed:          len(schemasChanged) > 0,
						SchemasChanged:   schemasChanged,
						TestsFromSchema:  mapKeysToSortedSlice(testsFromContracts),
						GeneratedChanged: mapKeysToSortedSlice(generatedChanged),
					}

					// Contract changes increase risk
					if len(schemasChanged) > 0 {
						plan.Uncertainty.Score += 20
						plan.Uncertainty.Sources = append(plan.Uncertainty.Sources,
							fmt.Sprintf("contract_change:%d_schemas", len(schemasChanged)))
						analyzersUsed = append(analyzersUsed, "contracts@1")
					}
				}
			}
		}

		// Update provenance with analyzers used
		plan.Provenance.Analyzers = analyzersUsed

		plan.Policy.FallbackUsed = fallbackUsed

		// Convert affected targets to sorted list
		for t := range affectedTargets {
			plan.Targets.Run = append(plan.Targets.Run, t)
		}
		sort.Strings(plan.Targets.Run)

		// If no targets found but there are changes, we have uncertainty
		if len(plan.Targets.Run) == 0 && len(changedFiles) > 0 {
			plan.Risk = "medium"
			plan.Impact.Uncertainty = append(plan.Impact.Uncertainty,
				"No test files found for changed files - dependency graph may be incomplete")
			plan.Uncertainty.Score += 30
			plan.Uncertainty.Sources = append(plan.Uncertainty.Sources, "no_test_mapping:present")

			// Apply risk policy (original behavior still applies)
			switch ciRiskPolicy {
			case "expand":
				// Add all test files as targets
				for _, f := range files {
					path, _ := f.Payload["path"].(string)
					if parse.IsTestFile(path) {
						plan.Targets.Run = append(plan.Targets.Run, path)
					}
				}
				sort.Strings(plan.Targets.Run)
				plan.Policy.Expanded = true
				plan.Risk = "low" // Expanded to be safe
				plan.ExpansionLog = append(plan.ExpansionLog,
					fmt.Sprintf("no_test_mapping → expanded to full suite (%d tests)", len(plan.Targets.Run)))

			case "warn":
				plan.Risk = "high"

			case "fail":
				return fmt.Errorf("uncertainty detected and risk-policy is 'fail': no test mappings found for changed files")
			}
		}

		// === SAFETY MODE LOGIC ===

		// Get modules affected for cross-module risk detection
		var modulesAffected []string
		var moduleMappings []ModulePathMapping
		if matcher != nil {
			moduleSet := make(map[string]bool)
			for _, f := range changedFiles {
				modules := matcher.MatchPath(f)
				for _, m := range modules {
					moduleSet[m] = true
				}
			}
			for m := range moduleSet {
				modulesAffected = append(modulesAffected, m)
			}
			sort.Strings(modulesAffected)
			plan.Impact.ModulesAffected = modulesAffected

			// Build path mappings for accurate test matching
			moduleMappings = buildModulePathMappings(matcher, modulesAffected)
		}

		// Create a content reader for dynamic import detection
		// Map path -> digest for quick lookup
		fileDigestByPath := make(map[string]string)
		for _, f := range files {
			if path, ok := f.Payload["path"].(string); ok {
				if digest, ok := f.Payload["digest"].(string); ok {
					fileDigestByPath[path] = digest
				}
			}
		}
		contentReader := func(path string) ([]byte, error) {
			digest, ok := fileDigestByPath[path]
			if !ok {
				return nil, fmt.Errorf("file not found: %s", path)
			}
			return db.ReadObject(digest)
		}

		// Detect structural risks (with content analysis for dynamic imports)
		risks := detectStructuralRisksWithContent(changedFiles, affectedTargets, allTestFiles, modulesAffected, contentReader)
		plan.Safety.StructuralRisks = risks

		// Detect dynamic imports in detail and perform scoped expansion
		// Uses content-addressable caching by file digest for performance
		var allDynamicImports []DynamicImportFile
		var cacheHits, cacheMisses int
		for _, changedPath := range changedFiles {
			digest, hasDigest := fileDigestByPath[changedPath]

			// Try cache first if we have a digest
			if hasDigest {
				if cached, ok := dynamicImportCache.Get(digest); ok {
					cacheHits++
					allDynamicImports = append(allDynamicImports, cached...)
					continue
				}
			}

			// Cache miss - perform detection
			cacheMisses++
			content, err := contentReader(changedPath)
			if err != nil {
				continue
			}
			imports := detectDynamicImportsDetailed(content, changedPath, &ciPolicy.DynamicImports)
			allDynamicImports = append(allDynamicImports, imports...)

			// Store in cache if we have a digest
			if hasDigest {
				dynamicImportCache.Set(digest, imports)
			}
		}

		// Build module -> test map for scoped expansion
		moduleTestMap := buildModuleTestMap(allTestFiles, moduleMappings)

		// Perform scoped expansion based on policy
		dynamicExpandedTests, dynamicImportInfo := expandForDynamicImports(
			allDynamicImports,
			&ciPolicy.DynamicImports,
			allTestFiles,
			changedFiles,
			moduleMappings,
			moduleTestMap,
		)

		// Add dynamically expanded tests to targets
		if len(dynamicExpandedTests) > 0 {
			for _, t := range dynamicExpandedTests {
				if !affectedTargets[t] {
					affectedTargets[t] = true
					plan.Targets.Run = append(plan.Targets.Run, t)
				}
			}
			sort.Strings(plan.Targets.Run)

			// Log expansion
			plan.ExpansionLog = append(plan.ExpansionLog,
				fmt.Sprintf("dynamic_imports (%s) → expanded by %d tests",
					ciPolicy.DynamicImports.Expansion, len(dynamicExpandedTests)))
		}

		// Attach dynamic import info to plan
		plan.DynamicImport = dynamicImportInfo

		// Add cache stats to telemetry
		if plan.DynamicImport != nil {
			plan.DynamicImport.Telemetry.CacheHits = cacheHits
			plan.DynamicImport.Telemetry.CacheMisses = cacheMisses
		}

		// Add uncertainty sources from structural risks
		for _, r := range risks {
			source := fmt.Sprintf("%s:%s", r.Type, r.Severity)
			plan.Uncertainty.Sources = append(plan.Uncertainty.Sources, source)
			switch r.Severity {
			case "critical":
				plan.Uncertainty.Score += 40
			case "high":
				plan.Uncertainty.Score += 25
			case "medium":
				plan.Uncertainty.Score += 10
			case "low":
				plan.Uncertainty.Score += 5
			}
		}
		// Cap uncertainty at 100
		if plan.Uncertainty.Score > 100 {
			plan.Uncertainty.Score = 100
		}

		// Calculate confidence
		confidence := calculateConfidence(risks, len(affectedTargets), len(changedFiles))
		plan.Safety.Confidence = confidence
		plan.Confidence = confidence // Also set top-level confidence
		debugf("ci plan: %d risks found, confidence=%.2f, %d targets affected", len(risks), confidence, len(affectedTargets))

		// Check if we should expand for safety
		shouldExpand, expansionReasons := shouldExpandForSafety(ciSafetyMode, risks, confidence, ciPolicy)

		// Apply safety mode behavior
		switch ciSafetyMode {
		case "shadow":
			// Shadow mode: compute selective plan but include full test list
			// CI should run the full suite and compare results
			plan.Mode = "shadow"
			plan.Targets.Full = allTestFiles

			// Populate prediction data for comparison
			selectiveCount := len(plan.Targets.Run)
			fullCount := len(allTestFiles)
			var savings float64
			if fullCount > 0 {
				savings = float64(fullCount-selectiveCount) / float64(fullCount) * 100
			}
			plan.Prediction = CIPrediction{
				SelectiveTests:   selectiveCount,
				FullTests:        fullCount,
				PredictedSavings: savings,
			}

			// Log what would have been selected
			if len(risks) > 0 {
				plan.Impact.Uncertainty = append(plan.Impact.Uncertainty,
					fmt.Sprintf("Shadow mode: detected %d structural risks - would have expanded in guarded mode", len(risks)))
			}

		case "guarded":
			// Guarded mode: expand if structural risks triggered
			if shouldExpand {
				// Expand to full suite
				plan.Mode = "expanded"
				plan.Targets.Run = allTestFiles
				plan.Safety.AutoExpanded = true
				plan.Safety.ExpansionReasons = expansionReasons
				plan.Safety.RecommendFull = true
				plan.Safety.RecommendReason = strings.Join(expansionReasons, "; ")
				plan.Risk = "low" // Safe because we expanded
				plan.Policy.Expanded = true

				// Add expansion log entries
				for _, reason := range expansionReasons {
					plan.ExpansionLog = append(plan.ExpansionLog,
						fmt.Sprintf("%s → expanded to full suite", reason))
				}
			} else {
				plan.Mode = "selective"
				// Still recommend full if confidence is low but not critical
				if confidence < 0.5 {
					plan.Safety.RecommendFull = true
					plan.Safety.RecommendReason = fmt.Sprintf("Low confidence (%.0f%%) - consider running full suite", confidence*100)
				}
			}

		case "strict":
			// Strict mode: never auto-expand, just warn
			plan.Mode = "selective"
			plan.Targets.Fallback = false // Disable automatic fallback

			// Still populate warnings
			if shouldExpand {
				plan.Safety.RecommendFull = true
				plan.Safety.RecommendReason = fmt.Sprintf("Strict mode: %d triggered risks detected but not expanding (use KAI_FORCE_FULL=1 for full suite)", len(expansionReasons))
				for _, reason := range expansionReasons {
					plan.Impact.Uncertainty = append(plan.Impact.Uncertainty,
						fmt.Sprintf("Risk detected (not expanding in strict mode): %s", reason))
				}
			}
		}

		debugf("ci plan: mode=%s risk=%s targets=%d", plan.Mode, plan.Risk, len(plan.Targets.Run))

		// Adjust risk level based on safety analysis
		if len(risks) > 0 && plan.Risk == "low" {
			hasHighRisk := false
			for _, r := range risks {
				if r.Severity == "high" || r.Severity == "critical" {
					hasHighRisk = true
					break
				}
			}
			if hasHighRisk && !plan.Safety.AutoExpanded {
				plan.Risk = "medium"
			}
		}
	}

	return outputCIPlan(plan, args)
}

// outputCIPlan handles plan serialization, file writing, and display.
func outputCIPlan(plan CIPlan, args []string) error {
	// Load CI policy for fail-closed checks
	ciPolicy, _, _ := loadCIPolicy()

	planJSON, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}

	// Write to file if specified
	if ciOutFile != "" {
		if err := os.WriteFile(ciOutFile, planJSON, 0644); err != nil {
			return fmt.Errorf("writing plan file: %w", err)
		}
		fmt.Printf("Plan written to %s\n", ciOutFile)
	}

	// Handle --explain flag for human-readable output
	if ciExplain {
		// First show concept explanations
		selectorDisplay := "@cs:last"
		if len(args) > 0 {
			selectorDisplay = args[0]
		} else if ciGitRange != "" {
			selectorDisplay = ciGitRange
		}
		ctx := explain.ExplainCIPlan(
			selectorDisplay,
			plan.Policy.Strategy,
			len(plan.Impact.FilesChanged),
			len(plan.Targets.Run),
		)
		ctx.Print(os.Stdout)
		// Then show the detailed table
		printExplainTable(plan)
		return nil
	}

	// Also print if --json flag or no output file
	if jsonFlag || ciOutFile == "" {
		fmt.Println(string(planJSON))
	} else {
		// Print summary
		fmt.Printf("\nCI Plan Summary:\n")
		fmt.Printf("  Mode: %s\n", plan.Mode)
		fmt.Printf("  Safety Mode: %s\n", plan.SafetyMode)
		fmt.Printf("  Risk: %s\n", plan.Risk)
		fmt.Printf("  Confidence: %.0f%%\n", plan.Safety.Confidence*100)
		fmt.Printf("  Strategy: %s", plan.Policy.Strategy)
		if plan.Policy.FallbackUsed != "" {
			fmt.Printf(" (used: %s)", plan.Policy.FallbackUsed)
		}
		fmt.Println()
		fmt.Printf("  Files changed: %d\n", len(plan.Impact.FilesChanged))
		fmt.Printf("  Targets to run: %d\n", len(plan.Targets.Run))
		if len(plan.Targets.Full) > 0 {
			fmt.Printf("  Full suite size: %d\n", len(plan.Targets.Full))
		}
		if plan.Policy.Expanded || plan.Safety.AutoExpanded {
			fmt.Printf("  (Expanded for safety)\n")
		}
		if plan.Safety.PanicSwitch {
			fmt.Printf("  PANIC SWITCH: Full suite forced via env var\n")
		}
		if len(plan.Safety.StructuralRisks) > 0 {
			fmt.Printf("  Structural risks: %d\n", len(plan.Safety.StructuralRisks))
		}
		// Dynamic import telemetry - detailed output
		if plan.DynamicImport != nil && plan.DynamicImport.Detected {
			fmt.Printf("\n  Dynamic Imports:\n")
			fmt.Printf("    Detected: %d total\n", plan.DynamicImport.Telemetry.TotalDetected)
			if plan.DynamicImport.Telemetry.Bounded > 0 {
				fmt.Printf("    Bounded (safe): %d\n", plan.DynamicImport.Telemetry.Bounded)
			}
			if plan.DynamicImport.Telemetry.BoundedRisky > 0 {
				fmt.Printf("    Bounded (risky footprint): %d\n", plan.DynamicImport.Telemetry.BoundedRisky)
			}
			if plan.DynamicImport.Telemetry.Unbounded > 0 {
				fmt.Printf("    Unbounded: %d\n", plan.DynamicImport.Telemetry.Unbounded)
			}
			if plan.DynamicImport.Telemetry.Allowlisted > 0 {
				fmt.Printf("    Allowlisted: %d\n", plan.DynamicImport.Telemetry.Allowlisted)
			}

			// Show per-file details for unbounded/risky imports
			for _, imp := range plan.DynamicImport.Files {
				if !imp.Bounded || imp.BoundedRisky {
					if imp.Allowlisted {
						continue
					}
					status := "unbounded"
					if imp.BoundedRisky {
						status = "bounded-risky"
					}
					fmt.Printf("    - %s:%d (%s) [%s]", imp.Path, imp.Line, imp.Kind, status)
					if imp.ExpandedTo != "" {
						fmt.Printf(" → %s", imp.ExpandedTo)
					}
					fmt.Println()
				}
			}

			if plan.DynamicImport.Telemetry.WidenedTests > 0 {
				fmt.Printf("    Tests widened: %d (strategy: %s)\n",
					plan.DynamicImport.Telemetry.WidenedTests, plan.DynamicImport.Telemetry.StrategyUsed)
			}
			// Show cache performance stats if any caching occurred
			if plan.DynamicImport.Telemetry.CacheHits > 0 || plan.DynamicImport.Telemetry.CacheMisses > 0 {
				total := plan.DynamicImport.Telemetry.CacheHits + plan.DynamicImport.Telemetry.CacheMisses
				hitRate := float64(plan.DynamicImport.Telemetry.CacheHits) / float64(total) * 100
				fmt.Printf("    Cache: %d/%d hits (%.0f%%)\n",
					plan.DynamicImport.Telemetry.CacheHits, total, hitRate)
			}
		}
		if plan.Safety.RecommendFull && !plan.Safety.AutoExpanded {
			fmt.Printf("  WARNING: %s\n", plan.Safety.RecommendReason)
		}
		// Shadow mode specific output
		if plan.Mode == "shadow" {
			fmt.Printf("\n  Shadow Mode Analysis:\n")
			fmt.Printf("    Selective would run: %d tests\n", plan.Prediction.SelectiveTests)
			fmt.Printf("    Full suite: %d tests\n", plan.Prediction.FullTests)
			fmt.Printf("    Predicted savings: %.1f%%\n", plan.Prediction.PredictedSavings)
		}
	}

	// Fail-closed: exit non-zero if no tests selected and risk is not low
	// This prevents silently passing CI when test selection fails
	if len(plan.Targets.Run) == 0 && plan.Risk != "low" {
		return fmt.Errorf("fail-closed: no tests selected but risk level is '%s' (uncertainty: %d%%)", plan.Risk, plan.Uncertainty.Score)
	}

	// Also fail if policy says to fail on expansion
	if ciPolicy.Behavior.FailOnExpansion && plan.Safety.AutoExpanded {
		return fmt.Errorf("fail-closed: expansion occurred and policy.behavior.failOnExpansion is true")
	}

	return nil
}

// printExplainTable outputs a human-readable why-in/why-out table
func printExplainTable(plan CIPlan) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                           CI TEST SELECTION PLAN                             ║")
	fmt.Printf("╠══════════════════════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ Mode: %-12s  Safety: %-10s  Risk: %-8s  Confidence: %3.0f%% ║\n",
		plan.Mode, plan.SafetyMode, plan.Risk, plan.Confidence*100)
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	// Changes section
	fmt.Println("║ CHANGES                                                                      ║")
	fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
	for _, f := range plan.Impact.FilesChanged {
		if len(f) > 74 {
			f = "..." + f[len(f)-71:]
		}
		fmt.Printf("║   %-75s║\n", f)
	}
	if len(plan.Impact.FilesChanged) == 0 {
		fmt.Println("║   (no changes)                                                               ║")
	}

	// Tests to run section
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Printf("║ TESTS TO RUN (%d)                                                             ║\n", len(plan.Targets.Run))
	fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
	for i, t := range plan.Targets.Run {
		if i >= 10 {
			fmt.Printf("║   ... and %d more                                                            ║\n", len(plan.Targets.Run)-10)
			break
		}
		if len(t) > 74 {
			t = "..." + t[len(t)-71:]
		}
		fmt.Printf("║   %-75s║\n", t)
	}
	if len(plan.Targets.Run) == 0 {
		fmt.Println("║   (none selected)                                                            ║")
	}

	// Tests skipped section
	if len(plan.Targets.Skip) > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
		fmt.Printf("║ TESTS SKIPPED (%d)                                                            ║\n", len(plan.Targets.Skip))
		fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
		for i, t := range plan.Targets.Skip {
			if i >= 5 {
				fmt.Printf("║   ... and %d more                                                            ║\n", len(plan.Targets.Skip)-5)
				break
			}
			if len(t) > 74 {
				t = "..." + t[len(t)-71:]
			}
			fmt.Printf("║   %-75s║\n", t)
		}
	}

	// Risks section
	if len(plan.Safety.StructuralRisks) > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
		fmt.Println("║ STRUCTURAL RISKS                                                             ║")
		fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
		for _, r := range plan.Safety.StructuralRisks {
			triggered := " "
			if r.Triggered {
				triggered = "!"
			}
			severity := fmt.Sprintf("[%s]", r.Severity)
			desc := r.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Printf("║ %s %-8s %-64s║\n", triggered, severity, desc)
		}
	}

	// Dynamic imports section
	if plan.DynamicImport != nil && plan.DynamicImport.Detected {
		fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
		fmt.Printf("║ DYNAMIC IMPORTS (detected: %d, bounded: %d, unbounded: %d)                    ║\n",
			plan.DynamicImport.Telemetry.TotalDetected,
			plan.DynamicImport.Telemetry.Bounded,
			plan.DynamicImport.Telemetry.Unbounded)
		fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
		for i, imp := range plan.DynamicImport.Files {
			if i >= 5 {
				fmt.Printf("║   ... and %d more                                                            ║\n", len(plan.DynamicImport.Files)-5)
				break
			}
			status := "⚠"
			if imp.Bounded {
				status = "✓"
			} else if imp.Allowlisted {
				status = "○"
			}
			desc := fmt.Sprintf("%s %s:%d (%s)", status, imp.Path, imp.Line, imp.Kind)
			if len(desc) > 74 {
				desc = desc[:71] + "..."
			}
			fmt.Printf("║   %-75s║\n", desc)
		}
		fmt.Printf("║   Strategy: %-65s║\n", plan.DynamicImport.Policy.Expansion)
		if plan.DynamicImport.Telemetry.WidenedTests > 0 {
			fmt.Printf("║   Widened tests: %-60d║\n", plan.DynamicImport.Telemetry.WidenedTests)
		}
	}

	// Expansion log section
	if len(plan.ExpansionLog) > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
		fmt.Println("║ EXPANSION LOG                                                                ║")
		fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
		for _, entry := range plan.ExpansionLog {
			if len(entry) > 74 {
				entry = entry[:71] + "..."
			}
			fmt.Printf("║   %-75s║\n", entry)
		}
	}

	// Uncertainty section
	if plan.Uncertainty.Score > 0 {
		fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
		fmt.Printf("║ UNCERTAINTY SCORE: %d/100                                                     ║\n", plan.Uncertainty.Score)
		fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
		for _, s := range plan.Uncertainty.Sources {
			if len(s) > 74 {
				s = s[:71] + "..."
			}
			fmt.Printf("║   %-75s║\n", s)
		}
	}

	// Provenance section
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║ PROVENANCE                                                                   ║")
	fmt.Println("╟──────────────────────────────────────────────────────────────────────────────╢")
	fmt.Printf("║   Generated: %-63s║\n", plan.Provenance.GeneratedAt)
	fmt.Printf("║   Kai Version: %-61s║\n", plan.Provenance.KaiVersion)
	if plan.Provenance.Changeset != "" {
		cs := plan.Provenance.Changeset
		if len(cs) > 16 {
			cs = cs[:16]
		}
		fmt.Printf("║   Changeset: %-63s║\n", cs)
	}
	if plan.Provenance.PolicyHash != "" {
		fmt.Printf("║   Policy Hash: %-61s║\n", plan.Provenance.PolicyHash)
	}
	if plan.Provenance.EnvHash != "" {
		fmt.Printf("║   Env Hash: %-64s║\n", plan.Provenance.EnvHash)
	}
	if len(plan.Provenance.Analyzers) > 0 {
		fmt.Printf("║   Analyzers: %-63s║\n", strings.Join(plan.Provenance.Analyzers, ", "))
	}

	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func runCIPrint(cmd *cobra.Command, args []string) error {
	// Read plan file
	data, err := os.ReadFile(ciPlanFile)
	if err != nil {
		return fmt.Errorf("reading plan file: %w", err)
	}

	var plan CIPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return fmt.Errorf("parsing plan file: %w", err)
	}

	if jsonFlag {
		// Output as JSON
		output, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Println(string(output))
		return nil
	}

	switch ciSection {
	case "targets":
		fmt.Println("Targets to run:")
		if len(plan.Targets.Run) == 0 {
			fmt.Println("  (none)")
		} else {
			for _, t := range plan.Targets.Run {
				fmt.Printf("  %s\n", t)
			}
		}
		if len(plan.Targets.Skip) > 0 {
			fmt.Println("\nTargets to skip:")
			for _, t := range plan.Targets.Skip {
				fmt.Printf("  %s\n", t)
			}
		}

	case "impact":
		fmt.Println("Impact:")
		fmt.Printf("  Files changed: %d\n", len(plan.Impact.FilesChanged))
		for _, f := range plan.Impact.FilesChanged {
			fmt.Printf("    %s\n", f)
		}
		if len(plan.Impact.SymbolsChanged) > 0 {
			fmt.Printf("\n  Symbols changed: %d\n", len(plan.Impact.SymbolsChanged))
			for _, s := range plan.Impact.SymbolsChanged {
				fmt.Printf("    %s (%s)\n", s.FQ, s.Change)
			}
		}
		if len(plan.Impact.Uncertainty) > 0 {
			fmt.Println("\n  Uncertainty:")
			for _, u := range plan.Impact.Uncertainty {
				fmt.Printf("    - %s\n", u)
			}
		}

	case "safety":
		fmt.Println("Safety Analysis")
		fmt.Println(strings.Repeat("-", 40))
		fmt.Printf("Safety Mode:  %s\n", plan.SafetyMode)
		fmt.Printf("Confidence:   %.0f%%\n", plan.Safety.Confidence*100)
		fmt.Printf("Panic Switch: %v\n", plan.Safety.PanicSwitch)
		fmt.Printf("Auto Expanded: %v\n", plan.Safety.AutoExpanded)
		if plan.Safety.RecommendFull {
			fmt.Printf("Recommend Full: yes\n")
			fmt.Printf("Reason: %s\n", plan.Safety.RecommendReason)
		}
		if len(plan.Safety.StructuralRisks) > 0 {
			fmt.Printf("\nStructural Risks: %d\n", len(plan.Safety.StructuralRisks))
			for _, r := range plan.Safety.StructuralRisks {
				triggered := ""
				if r.Triggered {
					triggered = " [TRIGGERED]"
				}
				fmt.Printf("  [%s] %s%s\n", r.Severity, r.Description, triggered)
			}
		}
		if len(plan.Safety.ExpansionReasons) > 0 {
			fmt.Println("\nExpansion Reasons:")
			for _, reason := range plan.Safety.ExpansionReasons {
				fmt.Printf("  - %s\n", reason)
			}
		}
		// Shadow mode prediction
		if plan.Mode == "shadow" {
			fmt.Println("\nPrediction (Shadow Mode):")
			fmt.Printf("  Selective would run: %d tests\n", plan.Prediction.SelectiveTests)
			fmt.Printf("  Full suite: %d tests\n", plan.Prediction.FullTests)
			fmt.Printf("  Predicted savings: %.1f%%\n", plan.Prediction.PredictedSavings)
		}

	case "causes":
		fmt.Println("Test Selection Root Causes")
		fmt.Println(strings.Repeat("-", 40))

		// Build cause map: test -> reasons
		causeMap := make(map[string][]string)

		// 1. Direct changes - tests that are themselves in the changed files
		for _, f := range plan.Impact.FilesChanged {
			for _, t := range plan.Targets.Run {
				if t == f || strings.HasSuffix(f, t) || strings.HasSuffix(t, f) {
					causeMap[t] = append(causeMap[t], fmt.Sprintf("directly changed: %s", f))
				}
			}
		}

		// 2. Symbol-level impact
		for _, sym := range plan.Impact.SymbolsChanged {
			for _, t := range plan.Targets.Run {
				// Check if test imports/depends on this symbol
				if strings.Contains(sym.FQ, filepath.Dir(t)) {
					causeMap[t] = append(causeMap[t], fmt.Sprintf("symbol changed: %s (%s)", sym.FQ, sym.Change))
				}
			}
		}

		// 3. Expansion log entries
		for _, log := range plan.ExpansionLog {
			// Parse expansion log: "reason → tests..."
			parts := strings.SplitN(log, " → ", 2)
			if len(parts) == 2 {
				reason := parts[0]
				// This expansion reason applies to added tests
				for _, t := range plan.Targets.Run {
					if len(causeMap[t]) == 0 {
						causeMap[t] = append(causeMap[t], fmt.Sprintf("expansion: %s", reason))
					}
				}
			}
		}

		// 4. Dynamic import causes
		if plan.DynamicImport != nil && plan.DynamicImport.Detected {
			for _, imp := range plan.DynamicImport.Files {
				if imp.ExpandedTo != "" {
					// ExpandedTo might be a module or test pattern
					for _, t := range plan.Targets.Run {
						if strings.Contains(t, imp.ExpandedTo) || strings.HasPrefix(t, imp.ExpandedTo) {
							status := "unbounded"
							if imp.BoundedRisky {
								status = "bounded-risky"
							}
							causeMap[t] = append(causeMap[t],
								fmt.Sprintf("dynamic import in %s:%d (%s) [%s]",
									imp.Path, imp.Line, imp.Kind, status))
						}
					}
				}
			}
		}

		// 5. Structural risks that triggered expansion
		for _, r := range plan.Safety.StructuralRisks {
			if r.Triggered {
				for _, t := range plan.Targets.Run {
					if len(causeMap[t]) == 0 {
						causeMap[t] = append(causeMap[t],
							fmt.Sprintf("structural risk: %s (%s)", r.Type, r.Severity))
					}
				}
			}
		}

		// 6. Auto-expansion reasons
		if plan.Safety.AutoExpanded {
			for _, reason := range plan.Safety.ExpansionReasons {
				for _, t := range plan.Targets.Run {
					if len(causeMap[t]) == 0 {
						causeMap[t] = append(causeMap[t], fmt.Sprintf("safety expansion: %s", reason))
					}
				}
			}
		}

		// Output organized by test
		if len(plan.Targets.Run) == 0 {
			fmt.Println("  No tests selected.")
		} else {
			for _, t := range plan.Targets.Run {
				fmt.Printf("\n  %s\n", t)
				causes := causeMap[t]
				if len(causes) == 0 {
					fmt.Println("    → dependency graph traversal (inferred)")
				} else {
					// Deduplicate causes
					seen := make(map[string]bool)
					for _, c := range causes {
						if !seen[c] {
							seen[c] = true
							fmt.Printf("    → %s\n", c)
						}
					}
				}
			}
		}

		// Show if full suite was triggered
		if plan.Safety.PanicSwitch {
			fmt.Println("\n  FULL SUITE TRIGGERED (panic switch)")
		}

	case "summary":
		fallthrough
	default:
		// One-line summary for quick reading
		coverageTests := 0
		contractTests := 0
		if plan.Coverage != nil {
			coverageTests = len(plan.Coverage.TestsFromCoverage)
		}
		if plan.Contracts != nil {
			contractTests = len(plan.Contracts.TestsFromSchema)
		}
		fallbackStatus := "used=false"
		if plan.Fallback.Used {
			fallbackStatus = fmt.Sprintf("used=true (%s)", plan.Fallback.Reason)
		}
		fmt.Printf("Mode: %s | Risk: %s (score %d) | Fallback: %s | Coverage: %d tests | Contracts: %d tests\n\n",
			plan.SafetyMode, plan.Risk, plan.Uncertainty.Score, fallbackStatus, coverageTests, contractTests)

		fmt.Println("CI Plan Summary")
		fmt.Println(strings.Repeat("-", 40))
		fmt.Printf("Mode:       %s\n", plan.Mode)
		fmt.Printf("Safety:     %s\n", plan.SafetyMode)
		fmt.Printf("Risk:       %s\n", plan.Risk)
		fmt.Printf("Confidence: %.0f%%\n", plan.Safety.Confidence*100)
		fmt.Printf("Strategy:   %s\n", plan.Policy.Strategy)
		if plan.Policy.FallbackUsed != "" {
			fmt.Printf("Used:       %s\n", plan.Policy.FallbackUsed)
		}
		if plan.Policy.Expanded || plan.Safety.AutoExpanded {
			fmt.Printf("Expanded:   yes (for safety)\n")
		}
		if plan.Safety.PanicSwitch {
			fmt.Printf("Panic:      FULL SUITE FORCED\n")
		}
		fmt.Println()
		fmt.Printf("Changed:    %d files\n", len(plan.Impact.FilesChanged))
		fmt.Printf("Run:        %d targets\n", len(plan.Targets.Run))
		fmt.Printf("Skip:       %d targets\n", len(plan.Targets.Skip))
		if len(plan.Targets.Full) > 0 {
			fmt.Printf("Full Suite: %d targets\n", len(plan.Targets.Full))
		}

		if len(plan.Safety.StructuralRisks) > 0 {
			fmt.Printf("\nRisks:      %d detected\n", len(plan.Safety.StructuralRisks))
		}

		if plan.Safety.RecommendFull && !plan.Safety.AutoExpanded {
			fmt.Printf("\nWARNING: %s\n", plan.Safety.RecommendReason)
		}

		if len(plan.Impact.Uncertainty) > 0 {
			fmt.Printf("\nWarnings: %d\n", len(plan.Impact.Uncertainty))
			for _, u := range plan.Impact.Uncertainty {
				fmt.Printf("  - %s\n", u)
			}
		}

		// Shadow mode prediction summary
		if plan.Mode == "shadow" {
			fmt.Println("\nShadow Mode:")
			fmt.Printf("  Would save: %.1f%% (%d of %d tests)\n",
				plan.Prediction.PredictedSavings,
				plan.Prediction.FullTests-plan.Prediction.SelectiveTests,
				plan.Prediction.FullTests)
		}

		// Coverage info
		if plan.Coverage != nil && plan.Coverage.Enabled {
			fmt.Println("\nCoverage:")
			fmt.Printf("  Files with coverage:    %d\n", plan.Coverage.FilesWithCoverage)
			fmt.Printf("  Files without coverage: %d\n", plan.Coverage.FilesWithoutCoverage)
			if len(plan.Coverage.TestsFromCoverage) > 0 {
				fmt.Printf("  Tests from coverage:    %d\n", len(plan.Coverage.TestsFromCoverage))
			}
		}

		// Contracts info
		if plan.Contracts != nil && plan.Contracts.Changed {
			fmt.Println("\nContracts:")
			fmt.Printf("  Schemas changed: %d\n", len(plan.Contracts.SchemasChanged))
			for _, sc := range plan.Contracts.SchemasChanged {
				fmt.Printf("    - %s (%s)\n", sc.Path, sc.Type)
			}
			if len(plan.Contracts.TestsFromSchema) > 0 {
				fmt.Printf("  Tests from contracts: %d\n", len(plan.Contracts.TestsFromSchema))
			}
		}

		// Fallback status
		if plan.Fallback.Used {
			fmt.Println("\nFallback:")
			fmt.Printf("  Used:   true\n")
			fmt.Printf("  Reason: %s\n", plan.Fallback.Reason)
			if plan.Fallback.Trigger != "" {
				fmt.Printf("  Trigger: %s\n", plan.Fallback.Trigger)
			}
			if plan.Fallback.ExitCode != 0 {
				fmt.Printf("  Exit Code: %d\n", plan.Fallback.ExitCode)
			}
		}
	}

	return nil
}

// RuntimeRiskReport represents the output of detect-runtime-risk
type RuntimeRiskReport struct {
	RisksDetected     bool                `json:"risksDetected"`
	TotalRisks        int                 `json:"totalRisks"`
	TripwireTriggered bool                `json:"tripwireTriggered"`
	Risks             []RuntimeRiskSignal `json:"risks"`
	Recommendation    string              `json:"recommendation"`
}

// RuntimeRiskSignal represents a single detected runtime risk
type RuntimeRiskSignal struct {
	Type        string `json:"type"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
}

// Runtime risk signal types
const (
	RuntimeRiskModuleNotFound = "module_not_found"
	RuntimeRiskImportError    = "import_error"
	RuntimeRiskTypeError      = "type_error"
	RuntimeRiskSetupCrash     = "setup_crash"
	RuntimeRiskCoverageAnomly = "coverage_anomaly"
	RuntimeRiskUnexpectedFail = "unexpected_failure"
)

// Patterns to detect runtime risks in test output
var runtimeRiskPatterns = []struct {
	pattern     string
	riskType    string
	severity    string
	description string
}{
	// ===== Node.js / JavaScript =====
	{`Cannot find module ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Module not found - likely selection miss"},
	{`Error: Cannot find module`, RuntimeRiskModuleNotFound, "critical", "Module not found - likely selection miss"},
	{`Module not found: Error: Can't resolve ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Webpack module resolution failed"},
	{`Error: Cannot resolve module ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Module resolution failed"},
	{`SyntaxError: Cannot use import statement`, RuntimeRiskImportError, "high", "ES module import error"},
	{`ReferenceError: (\w+) is not defined`, RuntimeRiskImportError, "high", "Reference error - possible missing import"},
	{`TypeError: (\w+) is not a function`, RuntimeRiskImportError, "high", "Type error - possible missing export"},
	{`TypeError: Cannot read propert(?:y|ies) of undefined`, RuntimeRiskImportError, "high", "Undefined access - possible missing dependency"},

	// ===== TypeScript =====
	{`error TS2307:.*Cannot find module`, RuntimeRiskModuleNotFound, "critical", "TypeScript module not found"},
	{`error TS2305:.*has no exported member`, RuntimeRiskImportError, "critical", "TypeScript export missing"},
	{`error TS2339:.*does not exist on type`, RuntimeRiskTypeError, "high", "TypeScript property missing"},
	{`error TS\d+:`, RuntimeRiskTypeError, "high", "TypeScript compilation error"},
	{`Cannot find name ['"](\w+)['"]`, RuntimeRiskTypeError, "medium", "TypeScript name resolution failed"},
	// Type error burst detection (3+ errors = critical)
	{`Found \d+ errors?`, RuntimeRiskTypeError, "high", "TypeScript found errors"},

	// ===== Python =====
	{`ModuleNotFoundError: No module named ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Python module not found"},
	{`ImportError: cannot import name ['"]([^'"]+)['"]`, RuntimeRiskImportError, "critical", "Python import name error"},
	{`ImportError: No module named ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Python import error"},
	{`AttributeError: module ['"]([^'"]+)['"] has no attribute`, RuntimeRiskImportError, "high", "Python attribute missing"},
	// Python importlib errors
	{`importlib\..*Error`, RuntimeRiskImportError, "critical", "Python importlib error"},
	{`ModuleSpec.*not found`, RuntimeRiskModuleNotFound, "critical", "Python module spec not found"},
	{`spec_from_file_location.*failed`, RuntimeRiskImportError, "critical", "Python dynamic import failed"},
	{`__import__.*failed`, RuntimeRiskImportError, "critical", "Python __import__ failed"},
	// Python fixture/setup errors
	{`fixture ['"](\w+)['"] not found`, RuntimeRiskSetupCrash, "critical", "pytest fixture not found"},
	{`E\s+ModuleNotFoundError`, RuntimeRiskModuleNotFound, "critical", "pytest module not found"},
	{`ERRORS.*collection`, RuntimeRiskSetupCrash, "critical", "pytest collection errors"},

	// ===== Go =====
	{`undefined: (\w+)`, RuntimeRiskImportError, "high", "Go undefined symbol"},
	{`cannot find package ['"]([^'"]+)['"]`, RuntimeRiskModuleNotFound, "critical", "Go package not found"},
	{`no required module provides package`, RuntimeRiskModuleNotFound, "critical", "Go module missing"},
	// Go plugin load failures
	{`plugin\.Open.*failed`, RuntimeRiskImportError, "critical", "Go plugin load failed"},
	{`plugin: symbol .* not found`, RuntimeRiskImportError, "critical", "Go plugin symbol not found"},
	{`plugin was built with a different version`, RuntimeRiskImportError, "critical", "Go plugin version mismatch"},
	// Go test failures
	{`panic: .*nil pointer`, RuntimeRiskUnexpectedFail, "high", "Go nil pointer panic"},
	{`FAIL\s+[\w/.]+\s+\[build failed\]`, RuntimeRiskSetupCrash, "critical", "Go build failed"},

	// ===== Jest / JavaScript Test Runners =====
	{`beforeAll.*failed|beforeEach.*failed`, RuntimeRiskSetupCrash, "critical", "Test setup hook failed"},
	{`afterAll.*failed|afterEach.*failed`, RuntimeRiskSetupCrash, "high", "Test teardown hook failed"},
	{`Test suite failed to run`, RuntimeRiskSetupCrash, "critical", "Test suite failed to initialize"},
	{`Jest encountered an unexpected token`, RuntimeRiskSetupCrash, "critical", "Jest parse error - config issue"},
	{`Cannot find module.*from.*\.test\.[jt]sx?`, RuntimeRiskModuleNotFound, "critical", "Test file import failed"},
	{`Your test suite must contain at least one test`, RuntimeRiskSetupCrash, "high", "Empty test suite - possible selection miss"},
	{`RUNS.*0 passed`, RuntimeRiskUnexpectedFail, "medium", "All tests failed"},
	// Jest environment errors
	{`Test environment.*not found`, RuntimeRiskSetupCrash, "critical", "Jest environment not found"},
	{`jest-environment-.*not installed`, RuntimeRiskSetupCrash, "critical", "Jest environment missing"},
	{`Could not locate module.*mapped as`, RuntimeRiskModuleNotFound, "critical", "Jest module mapping failed"},

	// ===== Mocha =====
	{`Error: Cannot find module.*mocha`, RuntimeRiskSetupCrash, "critical", "Mocha module error"},
	{`Error \[ERR_MODULE_NOT_FOUND\]`, RuntimeRiskModuleNotFound, "critical", "ESM module not found"},

	// ===== Generic / Cross-platform =====
	{`ENOENT.*no such file or directory`, RuntimeRiskModuleNotFound, "high", "File not found at runtime"},
	{`ENOENT:.*\.js`, RuntimeRiskModuleNotFound, "critical", "JavaScript file missing"},
	{`Error: connect ECONNREFUSED`, RuntimeRiskUnexpectedFail, "low", "Connection refused - service may be down"},
	{`SIGTERM|SIGKILL|SIGSEGV`, RuntimeRiskSetupCrash, "critical", "Process killed - resource issue"},
	{`out of memory|OOM|heap out of memory`, RuntimeRiskSetupCrash, "critical", "Out of memory"},
	{`Maximum call stack size exceeded`, RuntimeRiskUnexpectedFail, "high", "Stack overflow"},

	// ===== Selection Miss Indicators =====
	// These patterns specifically indicate a test selection miss
	{`no tests found`, RuntimeRiskSetupCrash, "high", "No tests found - possible selection miss"},
	{`0 passing`, RuntimeRiskUnexpectedFail, "medium", "Zero passing tests"},
	{`nothing to test`, RuntimeRiskSetupCrash, "high", "Nothing to test - possible selection miss"},
}

func runCIDetectRuntimeRisk(cmd *cobra.Command, args []string) error {
	report := RuntimeRiskReport{
		RisksDetected: false,
		Risks:         []RuntimeRiskSignal{},
	}

	// Determine input source
	var content []byte
	var err error

	if ciLogsFile != "" {
		content, err = os.ReadFile(ciLogsFile)
		if err != nil {
			return fmt.Errorf("reading logs file: %w", err)
		}
	} else if ciStderrFile != "" {
		content, err = os.ReadFile(ciStderrFile)
		if err != nil {
			return fmt.Errorf("reading stderr file: %w", err)
		}
	} else {
		return fmt.Errorf("either --logs or --stderr is required")
	}

	// Load plan if provided (for cross-referencing)
	var plan *CIPlan
	if ciPlanFile != "" {
		planData, err := os.ReadFile(ciPlanFile)
		if err == nil {
			var p CIPlan
			if json.Unmarshal(planData, &p) == nil {
				plan = &p
			}
		}
	}

	// Convert content to string for pattern matching
	contentStr := string(content)

	// Check each pattern
	for _, p := range runtimeRiskPatterns {
		re, err := regexp.Compile(p.pattern)
		if err != nil {
			continue
		}

		matches := re.FindAllStringSubmatch(contentStr, -1)
		for _, match := range matches {
			risk := RuntimeRiskSignal{
				Type:        p.riskType,
				Severity:    p.severity,
				Description: p.description,
			}

			// Extract additional context if available
			if len(match) > 1 {
				risk.Evidence = match[1]
			} else {
				risk.Evidence = match[0]
			}

			// Check if the risk is related to files outside the plan selection
			if plan != nil && risk.File != "" {
				inPlan := false
				for _, t := range plan.Targets.Run {
					if strings.Contains(t, risk.File) || strings.Contains(risk.File, t) {
						inPlan = true
						break
					}
				}
				if inPlan {
					// Risk is in selected files, lower severity
					risk.Description += " (in selected files)"
				} else {
					risk.Description += " (NOT in selected files - possible miss)"
				}
			}

			report.Risks = append(report.Risks, risk)
		}
	}

	// Determine overall risk status
	report.TotalRisks = len(report.Risks)
	report.RisksDetected = report.TotalRisks > 0

	// Count by severity
	criticalCount := 0
	highCount := 0
	for _, r := range report.Risks {
		switch r.Severity {
		case "critical":
			criticalCount++
		case "high":
			highCount++
		}
	}

	// Determine if tripwire should trigger
	tripwireTriggered := criticalCount > 0 || highCount > 0
	if ciRerunOnFail && report.RisksDetected {
		tripwireTriggered = true
	}

	// Generate recommendation
	if criticalCount > 0 {
		report.Recommendation = "RERUN: Critical runtime errors detected. Run full test suite."
	} else if highCount > 0 {
		report.Recommendation = "RERUN: High severity runtime errors detected. Run full test suite."
	} else if report.RisksDetected && ciRerunOnFail {
		report.Recommendation = "RERUN: Failures detected with --rerun-on-fail. Run full test suite."
	} else if report.RisksDetected {
		report.Recommendation = "WARNING: Minor runtime issues detected. Monitor for patterns."
	} else {
		report.Recommendation = "OK: No runtime risk signals detected."
	}

	// Tripwire mode: simple output for CI scripting
	if ciTripwire {
		if tripwireTriggered {
			fmt.Println("RERUN")
			// Exit with code 75 (custom code for "rerun needed")
			// We use a custom error type to signal this
			os.Exit(75)
		}
		fmt.Println("OK")
		return nil
	}

	// Output report
	if jsonFlag {
		report.TripwireTriggered = tripwireTriggered
		output, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(output))
	} else {
		fmt.Println("Runtime Risk Analysis")
		fmt.Println(strings.Repeat("=", 50))
		fmt.Printf("Risks Detected: %v\n", report.RisksDetected)
		fmt.Printf("Total Signals:  %d\n", report.TotalRisks)
		if tripwireTriggered {
			fmt.Printf("Tripwire:       TRIGGERED (rerun recommended)\n")
		}
		fmt.Printf("Recommendation: %s\n", report.Recommendation)

		if len(report.Risks) > 0 {
			fmt.Println("\nDetected Risks:")
			for i, r := range report.Risks {
				if i >= 10 {
					fmt.Printf("  ... and %d more\n", len(report.Risks)-10)
					break
				}
				fmt.Printf("  [%s] %s: %s\n", r.Severity, r.Type, r.Description)
				if r.Evidence != "" {
					evidence := r.Evidence
					if len(evidence) > 60 {
						evidence = evidence[:57] + "..."
					}
					fmt.Printf("         Evidence: %s\n", evidence)
				}
			}
		}

		// CI integration hint
		if tripwireTriggered {
			fmt.Println("\nCI Integration:")
			fmt.Println("  Use --tripwire flag for exit code 75 to trigger rerun:")
			fmt.Println("  kai ci detect-runtime-risk --stderr test.log --tripwire || npm run test:full")
		}
	}

	// Exit non-zero if risks detected that warrant fallback
	if tripwireTriggered {
		return fmt.Errorf("runtime risks detected: %d critical, %d high - rerun full suite", criticalCount, highCount)
	}

	return nil
}

// MissRecord represents a recorded test selection miss
type MissRecord struct {
	Timestamp      string       `json:"timestamp"`
	PlanFile       string       `json:"planFile,omitempty"`
	PlanProvenance CIProvenance `json:"planProvenance,omitempty"`
	FailedTests    []string     `json:"failedTests"`
	SelectedTests  []string     `json:"selectedTests"`
	MissedTests    []string     `json:"missedTests"`              // Failed but not selected
	FalsePositives []string     `json:"falsePositives,omitempty"` // Selected but didn't fail
}

func runCIRecordMiss(cmd *cobra.Command, args []string) error {
	if ciPlanFile == "" {
		return fmt.Errorf("--plan is required")
	}

	// Read plan
	planData, err := os.ReadFile(ciPlanFile)
	if err != nil {
		return fmt.Errorf("reading plan file: %w", err)
	}

	var plan CIPlan
	if err := json.Unmarshal(planData, &plan); err != nil {
		return fmt.Errorf("parsing plan file: %w", err)
	}

	// Get failed tests
	var failedTests []string
	if ciFailedTests != "" {
		failedTests = strings.Split(ciFailedTests, ",")
		for i := range failedTests {
			failedTests[i] = strings.TrimSpace(failedTests[i])
		}
	} else if ciEvidenceFile != "" {
		// Try to parse evidence file (Jest/Mocha JSON format)
		evidenceData, err := os.ReadFile(ciEvidenceFile)
		if err != nil {
			return fmt.Errorf("reading evidence file: %w", err)
		}
		failedTests = extractFailedTestsFromEvidence(evidenceData)
	} else {
		return fmt.Errorf("either --failed or --evidence is required")
	}

	// Build sets for comparison
	selectedSet := make(map[string]bool)
	for _, t := range plan.Targets.Run {
		selectedSet[t] = true
	}

	failedSet := make(map[string]bool)
	for _, t := range failedTests {
		failedSet[t] = true
	}

	// Find misses (failed but not selected)
	var missedTests []string
	for _, t := range failedTests {
		if !selectedSet[t] {
			missedTests = append(missedTests, t)
		}
	}

	// Find false positives (selected but didn't fail)
	// Note: This is tricky because we don't know which selected tests passed
	// We can only record this if we have full test results
	var falsePositives []string
	// For now, we'll leave this empty unless we have evidence

	// Create miss record
	record := MissRecord{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		PlanFile:       ciPlanFile,
		PlanProvenance: plan.Provenance,
		FailedTests:    failedTests,
		SelectedTests:  plan.Targets.Run,
		MissedTests:    missedTests,
		FalsePositives: falsePositives,
	}

	// Output
	if jsonFlag {
		output, _ := json.MarshalIndent(record, "", "  ")
		fmt.Println(string(output))
	} else {
		fmt.Println("Test Selection Miss Report")
		fmt.Println(strings.Repeat("=", 50))
		fmt.Printf("Timestamp:     %s\n", record.Timestamp)
		fmt.Printf("Selected:      %d tests\n", len(record.SelectedTests))
		fmt.Printf("Failed:        %d tests\n", len(record.FailedTests))
		fmt.Printf("Missed:        %d tests\n", len(record.MissedTests))

		if len(missedTests) > 0 {
			fmt.Println("\nMissed Tests (failed but not selected):")
			for _, t := range missedTests {
				fmt.Printf("  - %s\n", t)
			}
			fmt.Println("\nThese tests failed but were not in the selection plan.")
			fmt.Println("Consider investigating missing dependency edges.")
		} else {
			fmt.Println("\nNo misses detected! All failing tests were selected.")
		}
	}

	// Store record for aggregation (append to .kai/ci-misses.jsonl)
	if err := appendMissRecord(record); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to persist miss record: %v\n", err)
	}

	// Exit non-zero if there were misses
	if len(missedTests) > 0 {
		return fmt.Errorf("recorded %d missed tests", len(missedTests))
	}

	return nil
}

// extractFailedTestsFromEvidence parses test results to find failed tests
func extractFailedTestsFromEvidence(data []byte) []string {
	var failedTests []string

	// Try Jest format
	var jestResult struct {
		TestResults []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"testResults"`
	}
	if json.Unmarshal(data, &jestResult) == nil && len(jestResult.TestResults) > 0 {
		for _, tr := range jestResult.TestResults {
			if tr.Status == "failed" {
				failedTests = append(failedTests, tr.Name)
			}
		}
		return failedTests
	}

	// Try pytest format
	var pytestResult struct {
		Tests []struct {
			NodeID  string `json:"nodeid"`
			Outcome string `json:"outcome"`
		} `json:"tests"`
	}
	if json.Unmarshal(data, &pytestResult) == nil && len(pytestResult.Tests) > 0 {
		for _, t := range pytestResult.Tests {
			if t.Outcome == "failed" {
				failedTests = append(failedTests, t.NodeID)
			}
		}
		return failedTests
	}

	// Try Go test JSON format
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		var goResult struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
		}
		if json.Unmarshal([]byte(line), &goResult) == nil {
			if goResult.Action == "fail" && goResult.Test != "" {
				failedTests = append(failedTests, goResult.Package+"/"+goResult.Test)
			}
		}
	}

	return failedTests
}

// appendMissRecord appends a miss record to the CI misses log
func appendMissRecord(record MissRecord) error {
	// Find .kai directory
	kaiPath := filepath.Join(".", kaiDir)
	if _, err := os.Stat(kaiPath); os.IsNotExist(err) {
		return nil // Not in a kai repo, skip
	}

	missesFile := filepath.Join(kaiPath, "ci-misses.jsonl")

	f, err := os.OpenFile(missesFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	_, err = f.WriteString(string(data) + "\n")
	return err
}

// appendFlakyRecord appends a flaky test record to the history log
func appendFlakyRecord(record FlakyHistoryRecord) error {
	kaiPath := filepath.Join(".", kaiDir)
	if _, err := os.Stat(kaiPath); os.IsNotExist(err) {
		return nil
	}

	flakyFile := filepath.Join(kaiPath, "flaky-tests.jsonl")

	f, err := os.OpenFile(flakyFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	_, err = f.WriteString(string(data) + "\n")
	return err
}

// runCIExplainDynamicImports scans files for dynamic imports and explains their impact
func runCIExplainDynamicImports(cmd *cobra.Command, args []string) error {
	// Default to current directory
	targetPath := "."
	if len(args) > 0 {
		targetPath = args[0]
	}

	// Load CI policy
	ciPolicy, _, err := loadCIPolicy()
	if err != nil {
		return fmt.Errorf("loading CI policy: %w", err)
	}

	// Find all files to scan
	var filesToScan []string
	stat, err := os.Stat(targetPath)
	if err != nil {
		return fmt.Errorf("accessing path: %w", err)
	}

	if stat.IsDir() {
		// Walk directory
		err = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip errors
			}
			if info.IsDir() {
				// Skip common non-source directories
				base := filepath.Base(path)
				if base == "node_modules" || base == ".git" || base == "vendor" || base == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			// Only check source files
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".go":
				filesToScan = append(filesToScan, path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walking directory: %w", err)
		}
	} else {
		filesToScan = append(filesToScan, targetPath)
	}

	// Scan each file for dynamic imports
	var allImports []DynamicImportFile
	for _, filePath := range filesToScan {
		content, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		imports := detectDynamicImportsDetailed(content, filePath, &ciPolicy.DynamicImports)
		allImports = append(allImports, imports...)
	}

	// Output
	if jsonFlag {
		output := struct {
			TotalFilesScanned int                    `json:"totalFilesScanned"`
			TotalDetected     int                    `json:"totalDetected"`
			Policy            CIPolicyDynamicImports `json:"policy"`
			Imports           []DynamicImportFile    `json:"imports"`
		}{
			TotalFilesScanned: len(filesToScan),
			TotalDetected:     len(allImports),
			Policy:            ciPolicy.DynamicImports,
			Imports:           allImports,
		}
		data, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	// Human-readable output
	fmt.Println()
	fmt.Println("Dynamic Import Analysis")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Files scanned: %d\n", len(filesToScan))
	fmt.Printf("Dynamic imports found: %d\n", len(allImports))
	fmt.Printf("Expansion strategy: %s\n", ciPolicy.DynamicImports.Expansion)
	fmt.Printf("Owners fallback: %v\n", ciPolicy.DynamicImports.OwnersFallback)
	fmt.Printf("Bounded risk threshold: %d files\n", ciPolicy.DynamicImports.BoundedRiskThreshold)
	fmt.Println()

	if len(allImports) == 0 {
		fmt.Println("No dynamic imports detected.")
		return nil
	}

	// Group by status
	var bounded, boundedRisky, unbounded, allowlisted []DynamicImportFile
	for _, imp := range allImports {
		if imp.Allowlisted {
			allowlisted = append(allowlisted, imp)
		} else if imp.Bounded {
			bounded = append(bounded, imp)
		} else {
			unbounded = append(unbounded, imp)
		}
	}

	// Show unbounded first (most important)
	if len(unbounded) > 0 {
		fmt.Printf("⚠️  UNBOUNDED (%d) - Will trigger expansion:\n", len(unbounded))
		for _, imp := range unbounded {
			fmt.Printf("   %s:%d\n", imp.Path, imp.Line)
			fmt.Printf("      Type: %s (confidence: %.0f%%)\n", imp.Kind, imp.Confidence*100)
			fmt.Printf("      Action: Expand to %s\n", ciPolicy.DynamicImports.Expansion)
		}
		fmt.Println()
	}

	if len(boundedRisky) > 0 {
		fmt.Printf("⚡ BOUNDED-RISKY (%d) - Bounded but large footprint:\n", len(boundedRisky))
		for _, imp := range boundedRisky {
			fmt.Printf("   %s:%d\n", imp.Path, imp.Line)
			fmt.Printf("      Type: %s\n", imp.Kind)
			fmt.Printf("      Bound: %s\n", imp.BoundedBy)
			fmt.Printf("      Action: Treat as unbounded (footprint > %d)\n", ciPolicy.DynamicImports.BoundedRiskThreshold)
		}
		fmt.Println()
	}

	if len(bounded) > 0 {
		fmt.Printf("✓  BOUNDED (%d) - Safe, will not expand:\n", len(bounded))
		for _, imp := range bounded {
			fmt.Printf("   %s:%d → %s\n", imp.Path, imp.Line, imp.BoundedBy)
		}
		fmt.Println()
	}

	if len(allowlisted) > 0 {
		fmt.Printf("○  ALLOWLISTED (%d) - Ignored by policy:\n", len(allowlisted))
		for _, imp := range allowlisted {
			fmt.Printf("   %s:%d\n", imp.Path, imp.Line)
		}
		fmt.Println()
	}

	// Recommendations
	if len(unbounded) > 0 {
		fmt.Println("Recommendations:")
		fmt.Println("  • Add webpackInclude/webpackExclude comments to bound dynamic imports")
		fmt.Println("  • Add paths to dynamicImports.allowlist in .kai/rules/ci-policy.yaml")
		fmt.Println("  • Use explicit imports where possible")
	}

	return nil
}

// getChangedFiles returns paths of files that changed between two snapshots.
func getChangedFiles(db *graph.DB, creator *snapshot.Creator, baseID, headID []byte) ([]string, error) {
	// Get head files
	headFiles, err := creator.GetSnapshotFiles(headID)
	if err != nil {
		return nil, err
	}

	headMap := make(map[string]string) // path -> digest
	for _, f := range headFiles {
		path, _ := f.Payload["path"].(string)
		digest, _ := f.Payload["digest"].(string)
		headMap[path] = digest
	}

	// If no base, all head files are "changed"
	if baseID == nil {
		var paths []string
		for p := range headMap {
			paths = append(paths, p)
		}
		return paths, nil
	}

	// Get base files
	baseFiles, err := creator.GetSnapshotFiles(baseID)
	if err != nil {
		return nil, err
	}

	baseMap := make(map[string]string) // path -> digest
	for _, f := range baseFiles {
		path, _ := f.Payload["path"].(string)
		digest, _ := f.Payload["digest"].(string)
		baseMap[path] = digest
	}

	// Find changed files
	var changed []string

	// New or modified files
	for path, headDigest := range headMap {
		baseDigest, exists := baseMap[path]
		if !exists || baseDigest != headDigest {
			changed = append(changed, path)
		}
	}

	// Deleted files (these could affect tests too)
	for path := range baseMap {
		if _, exists := headMap[path]; !exists {
			changed = append(changed, path)
		}
	}

	return changed, nil
}

// createChangesetFromSnapshots creates a changeset between two snapshots and returns its ID
func createChangesetFromSnapshots(db *graph.DB, baseSnapID, headSnapID []byte, message string) ([]byte, error) {
	matcher, err := loadMatcher()
	if err != nil {
		return nil, err
	}

	// Get files from both snapshots
	creator := snapshot.NewCreator(db, matcher)
	baseFiles, err := creator.GetSnapshotFiles(baseSnapID)
	if err != nil {
		return nil, fmt.Errorf("getting base files: %w", err)
	}

	headFiles, err := creator.GetSnapshotFiles(headSnapID)
	if err != nil {
		return nil, fmt.Errorf("getting head files: %w", err)
	}

	// Build file maps
	baseFileMap := make(map[string]*graph.Node)
	headFileMap := make(map[string]*graph.Node)

	for _, f := range baseFiles {
		if path, ok := f.Payload["path"].(string); ok {
			baseFileMap[path] = f
		}
	}

	for _, f := range headFiles {
		if path, ok := f.Payload["path"].(string); ok {
			headFileMap[path] = f
		}
	}

	// Find changed files (by digest comparison)
	var changedPaths []string
	var changedFileIDs [][]byte

	for path, headFile := range headFileMap {
		baseFile, exists := baseFileMap[path]
		if !exists {
			// Added file
			changedPaths = append(changedPaths, path)
			changedFileIDs = append(changedFileIDs, headFile.ID)
		} else {
			// Check if digest differs
			baseDigest, _ := baseFile.Payload["digest"].(string)
			headDigest, _ := headFile.Payload["digest"].(string)
			if baseDigest != headDigest {
				changedPaths = append(changedPaths, path)
				changedFileIDs = append(changedFileIDs, headFile.ID)
			}
		}
	}

	// Start transaction
	tx, err := db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Create changeset node
	changeSetPayload := map[string]interface{}{
		"base":        util.BytesToHex(baseSnapID),
		"head":        util.BytesToHex(headSnapID),
		"title":       "",
		"description": message,
		"intent":      "",
		"createdAt":   util.NowMs(),
	}
	changeSetID, err := db.InsertNode(tx, graph.KindChangeSet, changeSetPayload)
	if err != nil {
		return nil, fmt.Errorf("inserting changeset: %w", err)
	}

	// Detect change types
	detector := classify.NewDetector()

	// Load symbols for each changed file
	for i := range changedPaths {
		fileID := changedFileIDs[i]
		symbols, err := creator.GetSymbolsInFile(fileID, headSnapID)
		if err == nil && len(symbols) > 0 {
			detector.SetSymbols(util.BytesToHex(fileID), symbols)
		}
	}

	var allChangeTypes []*classify.ChangeType
	var affectedModules []string
	affectedModulesSet := make(map[string]bool)

	for i, path := range changedPaths {
		headFile := headFileMap[path]
		baseFile := baseFileMap[path]

		var beforeContent, afterContent []byte

		// Read after content
		if digest, ok := headFile.Payload["digest"].(string); ok {
			afterContent, _ = db.ReadObject(digest)
		}

		// Read before content (if exists)
		if baseFile != nil {
			if digest, ok := baseFile.Payload["digest"].(string); ok {
				beforeContent, _ = db.ReadObject(digest)
			}
		}

		// Get the file's language
		lang, _ := headFile.Payload["lang"].(string)

		if len(beforeContent) > 0 && len(afterContent) > 0 {
			var changes []*classify.ChangeType
			var err error

			switch lang {
			case "json":
				// Use JSON-specific detection
				changes, err = classify.DetectJSONChanges(path, beforeContent, afterContent)
			case "yaml":
				// Use YAML-specific detection
				changes, err = classify.DetectYAMLChanges(path, beforeContent, afterContent)
			case "ts", "js":
				// Use tree-sitter based detection
				changes, err = detector.DetectChanges(path, beforeContent, afterContent, util.BytesToHex(changedFileIDs[i]))
			default:
				// Non-parseable files get FILE_CONTENT_CHANGED
				changes = []*classify.ChangeType{classify.NewFileChange(classify.FileContentChanged, path)}
			}

			if err == nil && len(changes) > 0 {
				allChangeTypes = append(allChangeTypes, changes...)
			}
		} else if baseFile == nil && len(afterContent) > 0 {
			// New file added
			allChangeTypes = append(allChangeTypes, classify.NewFileChange(classify.FileAdded, path))
		}

		// Create MODIFIES edge to file
		if err := db.InsertEdge(tx, changeSetID, graph.EdgeModifies, changedFileIDs[i], nil); err != nil {
			return nil, fmt.Errorf("inserting MODIFIES edge: %w", err)
		}

		// Map to modules
		modules := matcher.MatchPath(path)
		for _, mod := range modules {
			if !affectedModulesSet[mod] {
				affectedModulesSet[mod] = true
				affectedModules = append(affectedModules, mod)
			}
		}
	}

	// Create ChangeType nodes and HAS edges
	for _, ct := range allChangeTypes {
		payload := classify.GetCategoryPayload(ct)
		ctID, err := db.InsertNode(tx, graph.KindChangeType, payload)
		if err != nil {
			return nil, fmt.Errorf("inserting change type: %w", err)
		}
		if err := db.InsertEdge(tx, changeSetID, graph.EdgeHas, ctID, nil); err != nil {
			return nil, fmt.Errorf("inserting HAS edge: %w", err)
		}

		// Create MODIFIES edges to symbols
		for _, symIDHex := range ct.Evidence.Symbols {
			symID, err := util.HexToBytes(symIDHex)
			if err == nil {
				if err := db.InsertEdge(tx, changeSetID, graph.EdgeModifies, symID, nil); err != nil {
					// Ignore if symbol doesn't exist
				}
			}
		}
	}

	// Create AFFECTS edges to modules
	for _, modName := range affectedModules {
		payload := matcher.GetModulePayload(modName)
		if payload != nil {
			modID, err := db.InsertNode(tx, graph.KindModule, payload)
			if err != nil {
				return nil, fmt.Errorf("inserting module: %w", err)
			}
			if err := db.InsertEdge(tx, changeSetID, graph.EdgeAffects, modID, nil); err != nil {
				return nil, fmt.Errorf("inserting AFFECTS edge: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	// Update auto-refs
	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnChangeSetCreated(changeSetID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	}

	return changeSetID, nil
}

// createSnapshotFromGitRef creates a snapshot from a git ref, including symbol analysis
func createSnapshotFromGitRef(db *graph.DB, repoPath, gitRef string) ([]byte, error) {
	// Load module matcher
	matcher, err := loadMatcher()
	if err != nil {
		// Use empty matcher if no config
		matcher = module.NewMatcher(nil)
	}

	// Open git source
	source, err := gitio.OpenSource(repoPath, gitRef)
	if err != nil {
		return nil, fmt.Errorf("opening git ref %s: %w", gitRef, err)
	}

	// Create snapshot
	creator := snapshot.NewCreator(db, matcher)
	snapshotID, err := creator.CreateSnapshot(source)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot from %s: %w", gitRef, err)
	}

	// Analyze symbols (needed for semantic diff and CI plan)
	if err := analyzeSnapshotSymbols(db, snapshotID); err != nil {
		// Non-fatal - continue without symbols
		fmt.Fprintf(os.Stderr, "warning: symbol analysis failed for %s: %v\n", gitRef, err)
	}

	// Build call graph (needed for CI plan to find affected tests via IMPORTS/TESTS edges)
	if err := creator.AnalyzeCalls(snapshotID, func(current, total int, filename string) {}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: call graph analysis failed for %s: %v\n", gitRef, err)
	}

	return snapshotID, nil
}

// analyzeSnapshotSymbols extracts symbols from all files in a snapshot
func analyzeSnapshotSymbols(db *graph.DB, snapshotID []byte) error {
	matcher, err := loadMatcher()
	if err != nil {
		matcher = module.NewMatcher(nil)
	}

	creator := snapshot.NewCreator(db, matcher)
	// Silent progress for internal use
	progress := func(current, total int, filename string) {}
	return creator.AnalyzeSymbols(snapshotID, progress)
}

func runChangesetCreate(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var baseSnapID, headSnapID []byte

	// Check if using git refs or snapshot IDs
	useGitRefs := changesetGitBase != "" || changesetGitHead != ""

	if useGitRefs {
		// Both --git-base and --git-head required together
		if changesetGitBase == "" || changesetGitHead == "" {
			return fmt.Errorf("both --git-base and --git-head are required when using git refs")
		}

		fmt.Printf("Creating snapshot from git ref: %s\n", changesetGitBase)
		baseSnapID, err = createSnapshotFromGitRef(db, changesetGitRepo, changesetGitBase)
		if err != nil {
			return fmt.Errorf("creating base snapshot: %w", err)
		}

		fmt.Printf("Creating snapshot from git ref: %s\n", changesetGitHead)
		headSnapID, err = createSnapshotFromGitRef(db, changesetGitRepo, changesetGitHead)
		if err != nil {
			return fmt.Errorf("creating head snapshot: %w", err)
		}
	} else {
		// Traditional mode: use positional args as snapshot IDs
		if len(args) != 2 {
			return fmt.Errorf("either provide two snapshot IDs or use --git-base and --git-head")
		}

		baseSnapID, err = resolveSnapshotID(db, args[0])
		if err != nil {
			return fmt.Errorf("resolving base snapshot: %w", err)
		}

		headSnapID, err = resolveSnapshotID(db, args[1])
		if err != nil {
			return fmt.Errorf("resolving head snapshot: %w", err)
		}
	}

	changeSetID, err := createChangesetFromSnapshots(db, baseSnapID, headSnapID, changesetMessage)
	if err != nil {
		return err
	}

	// Get stats for output by querying edges
	modifiedFiles, _ := db.GetEdges(changeSetID, graph.EdgeModifies)
	changeTypes, _ := db.GetEdges(changeSetID, graph.EdgeHas)
	affectedModulesEdges, _ := db.GetEdges(changeSetID, graph.EdgeAffects)

	// Count unique files (filter out symbols from MODIFIES)
	fileCount := 0
	for _, edge := range modifiedFiles {
		node, err := db.GetNode(edge.Dst)
		if err == nil && node.Kind == graph.KindFile {
			fileCount++
		}
	}

	// Get module names
	var moduleNames []string
	for _, edge := range affectedModulesEdges {
		node, err := db.GetNode(edge.Dst)
		if err == nil && node.Kind == graph.KindModule {
			if name, ok := node.Payload["name"].(string); ok {
				moduleNames = append(moduleNames, name)
			}
		}
	}

	fmt.Printf("Created changeset: %s\n", util.BytesToHex(changeSetID))
	fmt.Printf("Changed files: %d\n", fileCount)
	fmt.Printf("Change types detected: %d\n", len(changeTypes))
	fmt.Printf("Affected modules: %v\n", moduleNames)

	return nil
}

func runIntentRender(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	changeSetID, err := resolveChangeSetID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving changeset ID: %w", err)
	}

	gen := intent.NewGenerator(db)

	// Use the new confidence-aware API when extra features are requested
	if showAlternatives || explainIntent || intentConfidence != 0.5 {
		result, err := gen.RenderIntentWithConfidence(changeSetID, editText, regenerateIntent, intentConfidence)
		if err != nil {
			return fmt.Errorf("rendering intent: %w", err)
		}

		if result.Primary != nil {
			fmt.Printf("Intent: %s\n", result.Primary.Text)

			if explainIntent {
				fmt.Printf("  Template: %s\n", result.Primary.Template)
				fmt.Printf("  Confidence: %.0f%%\n", result.Primary.Confidence*100)
				if result.Primary.Reasoning != "" {
					fmt.Printf("  Reasoning: %s\n", result.Primary.Reasoning)
				}
			}

			if showAlternatives && len(result.Alternatives) > 0 {
				fmt.Println("\nAlternatives:")
				for i, alt := range result.Alternatives {
					if i >= 3 {
						break // Show at most 3 alternatives
					}
					fmt.Printf("  - %s (%.0f%%)\n", alt.Text, alt.Confidence*100)
				}
			}

			if len(result.Warnings) > 0 {
				fmt.Println("\nWarnings:")
				for _, warning := range result.Warnings {
					fmt.Printf("  - %s\n", warning)
				}
			}
		}

		return nil
	}

	// Use legacy API for simple cases
	intentText, err := gen.RenderIntent(changeSetID, editText, regenerateIntent)
	if err != nil {
		return fmt.Errorf("rendering intent: %w", err)
	}

	fmt.Printf("Intent: %s\n", intentText)
	return nil
}

func runDump(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	changeSetID, err := resolveChangeSetID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving changeset ID: %w", err)
	}

	result, err := db.GetAllNodesAndEdgesForChangeSet(changeSetID)
	if err != nil {
		return fmt.Errorf("getting changeset data: %w", err)
	}

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON: %w", err)
	}

	fmt.Println(string(output))
	return nil
}

// --- Authorship Commands ---

func runBlame(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	filePath := args[0]

	// Resolve latest snapshot
	resolver := ref.NewResolver(db)
	kind := ref.KindSnapshot
	result, err := resolver.Resolve("@snap:last", &kind)
	if err != nil {
		return fmt.Errorf("no snapshots found — run 'kai capture' first")
	}
	snapID := result.ID

	if blameJSON {
		if blameSummary {
			summary, err := authorship.BlameFileSummary(db, snapID, filePath)
			if err != nil {
				return err
			}
			data, _ := json.MarshalIndent(summary, "", "  ")
			fmt.Println(string(data))
			return nil
		}
		ranges, err := authorship.Blame(db, snapID, filePath)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(map[string]interface{}{
			"file":   filePath,
			"ranges": ranges,
		}, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if blameSummary {
		summary, err := authorship.BlameFileSummary(db, snapID, filePath)
		if err != nil {
			return err
		}
		if summary.TotalLines == 0 {
			fmt.Printf("%s: no authorship data\n", filePath)
			fmt.Println("  Record edits with kai_checkpoint, then run 'kai capture'")
			return nil
		}
		fmt.Printf("%s: %.0f%% AI, %.0f%% human (%d lines)\n",
			filePath, summary.AIPct, 100-summary.AIPct, summary.TotalLines)
		if len(summary.Agents) > 0 {
			for _, agent := range summary.Agents {
				fmt.Printf("  %s\n", agent)
			}
		}
		return nil
	}

	// Default: git-blame-style per-line colored output
	ranges, err := authorship.Blame(db, snapID, filePath)
	if err != nil {
		return err
	}
	if len(ranges) == 0 {
		fmt.Printf("%s: no authorship data\n", filePath)
		fmt.Println("  Record edits with kai_checkpoint, then run 'kai capture'")
		return nil
	}

	return printBlamePretty(filePath, ranges)
}

// printBlamePretty renders git-blame-style output with colored author badges.
// Each source line gets: line number · author pill · code.
func printBlamePretty(filePath string, ranges []graph.AuthorshipRange) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		// File vanished after capture — fall back to range-only view.
		fmt.Printf("%s  (file not readable: %v)\n", filePath, err)
		for _, r := range ranges {
			author := blameAuthorLabel(r)
			fmt.Printf("  %4d-%-4d  %s\n", r.StartLine, r.EndLine, author)
		}
		return nil
	}
	lines := strings.Split(string(content), "\n")
	// Trim trailing empty line from split on trailing newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Build a per-line map: line → AuthorshipRange (last writer wins).
	lineAuthor := make(map[int]graph.AuthorshipRange, len(lines))
	for _, r := range ranges {
		for ln := r.StartLine; ln <= r.EndLine; ln++ {
			lineAuthor[ln] = r
		}
	}

	// Compute author column width (cap at 20).
	authorWidth := 6 // minimum, fits "human"
	for _, r := range ranges {
		if w := len(blameAuthorLabel(r)); w > authorWidth {
			authorWidth = w
		}
	}
	if authorWidth > 20 {
		authorWidth = 20
	}

	// Compute line-number column width.
	lnWidth := len(fmt.Sprintf("%d", len(lines)))
	if lnWidth < 3 {
		lnWidth = 3
	}

	// Header: file, AI %, dominant agents.
	aiLines, humanLines, agentSet := 0, 0, map[string]bool{}
	for _, r := range ranges {
		n := r.EndLine - r.StartLine + 1
		if r.AuthorType == "ai" {
			aiLines += n
			label := blameAuthorLabel(r)
			if label != "" && label != "ai" {
				agentSet[label] = true
			}
		} else {
			humanLines += n
		}
	}
	total := len(lines)
	var pct float64
	if total > 0 {
		pct = float64(aiLines) / float64(total) * 100
	}

	var agentList []string
	for a := range agentSet {
		agentList = append(agentList, a)
	}
	sort.Strings(agentList)

	c := newBlameColorer()
	fmt.Println()
	fmt.Printf("  %s  %s %.0f%% AI  %s (%d lines)\n",
		c.bold(filePath),
		c.dim("·"),
		pct,
		c.dim(fmt.Sprintf("%d AI / %d human", aiLines, humanLines)),
		total,
	)
	if len(agentList) > 0 {
		fmt.Printf("  %s  %s\n", c.dim("agents:"), strings.Join(agentList, c.dim(", ")))
	}
	fmt.Println()

	// Per-line rows. Each line gets a colored left bar + colored line number
	// matching the author. Blocks of same-agent code read as solid color stripes.
	for i, code := range lines {
		ln := i + 1
		r, hasAttr := lineAuthor[ln]
		var badge string
		if hasAttr {
			badge = blameAuthorLabel(r)
		} else {
			badge = "original"
		}
		badgeDisplay := padOrTruncate(badge, authorWidth)
		coloredBadge := c.agent(r, hasAttr, badgeDisplay)
		coloredBar := c.agentBar(r, hasAttr)
		coloredLine := c.agentLineNo(r, hasAttr, fmt.Sprintf("%*d", lnWidth, ln))

		fmt.Printf("  %s %s %s %s\n",
			coloredLine,
			coloredBar,
			coloredBadge,
			code,
		)
	}
	fmt.Println()
	return nil
}

// blameAuthorLabel returns a short human-readable label for an attribution.
// Prefers the model (e.g. "claude-opus-4-6"), falls back to agent name, then "human".
func blameAuthorLabel(r graph.AuthorshipRange) string {
	if r.AuthorType == "human" {
		return "human"
	}
	if r.Model != "" {
		return r.Model
	}
	if r.Agent != "" {
		return r.Agent
	}
	return "ai"
}

func padOrTruncate(s string, w int) string {
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

// blameColorer emits ANSI codes when stdout is a TTY and NO_COLOR is unset.
// Agent colors roughly match the homepage animation palette.
type blameColorer struct {
	enabled bool
}

func newBlameColorer() *blameColorer {
	if os.Getenv("NO_COLOR") != "" {
		return &blameColorer{enabled: false}
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return &blameColorer{enabled: false}
	}
	return &blameColorer{enabled: (fi.Mode() & os.ModeCharDevice) != 0}
}

func (c *blameColorer) wrap(code, s string) string {
	if !c.enabled {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (c *blameColorer) dim(s string) string  { return c.wrap("38;5;244", s) }
func (c *blameColorer) bold(s string) string { return c.wrap("1", s) }

// agentColorCode returns the 256-color code for a given attribution.
// Consolidates the lookup so badge, bar, and line number all stay in sync.
func agentColorCode(r graph.AuthorshipRange, hasAttr bool) string {
	if !hasAttr {
		return "240" // dim gray
	}
	if r.AuthorType == "human" {
		return "172" // orange
	}
	key := strings.ToLower(r.Agent + " " + r.Model)
	switch {
	case strings.Contains(key, "claude"):
		return "99" // purple
	case strings.Contains(key, "cursor"):
		return "36" // teal
	case strings.Contains(key, "copilot") || strings.Contains(key, "github"):
		return "75" // blue
	case strings.Contains(key, "codex") || strings.Contains(key, "gpt") || strings.Contains(key, "openai"):
		return "42" // green
	default:
		return "109" // cyan (generic MCP agent)
	}
}

// agent colors the author badge by attribution type/agent, bold.
func (c *blameColorer) agent(r graph.AuthorshipRange, hasAttr bool, s string) string {
	code := agentColorCode(r, hasAttr)
	if !hasAttr {
		return c.wrap("38;5;"+code, s)
	}
	return c.wrap("38;5;"+code+";1", s)
}

// agentBar returns a solid colored block character for the left gutter,
// rendered in the author color. Two characters wide so it reads as a real bar
// even on narrow terminals.
func (c *blameColorer) agentBar(r graph.AuthorshipRange, hasAttr bool) string {
	code := agentColorCode(r, hasAttr)
	return c.wrap("38;5;"+code, "▌▌")
}

// agentLineNo colors the line number in the author color — dim for
// unattributed, bright+bold for attributed. Matches the bar's hue so blocks
// of same-agent lines form a solid color stripe down the left.
func (c *blameColorer) agentLineNo(r graph.AuthorshipRange, hasAttr bool, s string) string {
	code := agentColorCode(r, hasAttr)
	if !hasAttr {
		return c.wrap("38;5;244", s) // dim gray for originals
	}
	return c.wrap("38;5;"+code, s)
}

func runStats(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	resolver := ref.NewResolver(db)
	kind := ref.KindSnapshot
	result, err := resolver.Resolve("@snap:last", &kind)
	if err != nil {
		return fmt.Errorf("no snapshots found — run 'kai capture' first")
	}

	stats, err := authorship.ProjectStats(db, result.ID)
	if err != nil {
		return err
	}

	if statsJSON {
		data, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if stats.TotalLines == 0 {
		fmt.Println("No authorship data found.")
		fmt.Println("  Record edits with kai_checkpoint, then run 'kai capture'")
		return nil
	}

	c := newBlameColorer()

	// Build a color-aware proportional bar: AI block then human block.
	barWidth := 40
	aiBarLen := int(float64(barWidth) * stats.AIPct / 100)
	if aiBarLen < 0 {
		aiBarLen = 0
	}
	if aiBarLen > barWidth {
		aiBarLen = barWidth
	}
	humanBarLen := barWidth - aiBarLen
	bar := c.wrap("38;5;99", strings.Repeat("█", aiBarLen)) +
		c.wrap("38;5;172", strings.Repeat("█", humanBarLen))

	fmt.Println()
	fmt.Printf("  %s  %s  %s\n",
		c.bold("Project authorship"),
		c.dim("·"),
		c.dim(fmt.Sprintf("%d lines", stats.TotalLines)),
	)
	fmt.Printf("  %s\n", bar)
	fmt.Printf("  %s %s   %s %s\n",
		c.wrap("38;5;99;1", "■"),
		fmt.Sprintf("AI %.1f%% (%d)", stats.AIPct, stats.AILines),
		c.wrap("38;5;172;1", "■"),
		fmt.Sprintf("Human %.1f%% (%d)", 100-stats.AIPct, stats.HumanLines),
	)

	if len(stats.ByAgent) > 0 {
		fmt.Println()
		fmt.Printf("  %s\n", c.dim("By agent"))

		// Sort for stable output — biggest first.
		type agentRow struct {
			name  string
			lines int
		}
		rows := make([]agentRow, 0, len(stats.ByAgent))
		for a, n := range stats.ByAgent {
			rows = append(rows, agentRow{a, n})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].lines > rows[j].lines })

		nameW := 0
		for _, row := range rows {
			if len(row.name) > nameW {
				nameW = len(row.name)
			}
		}
		if nameW > 28 {
			nameW = 28
		}

		for _, row := range rows {
			p := float64(row.lines) / float64(stats.TotalLines) * 100
			fake := graph.AuthorshipRange{AuthorType: "ai", Agent: row.name, Model: row.name}
			colored := c.agent(fake, true, padOrTruncate(row.name, nameW))
			fmt.Printf("  %s  %s %s\n",
				colored,
				fmt.Sprintf("%6d lines", row.lines),
				c.dim(fmt.Sprintf("(%.1f%%)", p)),
			)
		}
	}
	fmt.Println()

	return nil
}

// hookToolInput is the shape of the JSON Claude Code pipes to PostToolUse hooks.
// We only care about tool_name and tool_input — the rest is ignored so the
// struct stays resilient if the schema grows.
type hookToolInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		// Edit / Write / MultiEdit: file_path is common
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string,omitempty"`
		NewString string `json:"new_string,omitempty"`
		Content   string `json:"content,omitempty"` // Write tool
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits,omitempty"` // MultiEdit tool
	} `json:"tool_input"`
}

// runCheckpoint is invoked by the Claude Code PostToolUse hook.
// It reads the tool-use JSON from stdin, figures out which lines were
// actually written by the agent, and drops a checkpoint file that the
// next `kai capture` will consolidate into authorship_ranges.
//
// Unlike the old session-presence heuristic, this only attributes lines
// that came through the AI's tool runner — user keystrokes in the
// editor (including comments the user types while Claude is idle) never
// fire this hook and therefore never get attributed to the agent.
// findForeignKaiDir walks up from a file path looking for a kai data
// directory (.kai/ or .git/kai/) with a db.sqlite — a real Kai project.
// Returns the data-dir path or "" if none.
func findForeignKaiDir(absFilePath string) string {
	dir := filepath.Dir(absFilePath)
	for {
		candidate := kaipath.Resolve(dir)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			if _, err := os.Stat(filepath.Join(candidate, "db.sqlite")); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func runCheckpoint(cmd *cobra.Command, args []string) error {
	// Resolve project dir from cwd (kaiDir is relative to it).
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	kd := filepath.Join(workDir, kaiDir)
	if _, err := os.Stat(kd); os.IsNotExist(err) {
		// Not in a kai-init'd repo — silently succeed so we don't break
		// the user's editor when they're working in unrelated directories.
		return nil
	}

	// Read optional stdin payload (present when invoked as a hook).
	var payload hookToolInput
	var stdinData []byte
	if stat, _ := os.Stdin.Stat(); stat != nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		stdinData, _ = io.ReadAll(os.Stdin)
		if len(stdinData) > 0 {
			_ = json.Unmarshal(stdinData, &payload)
		}
	}

	// File path: flag wins, then stdin.
	file := checkpointFile
	if file == "" {
		file = payload.ToolInput.FilePath
	}
	if file == "" {
		return fmt.Errorf("no file path given (pass --file or pipe a PostToolUse JSON payload)")
	}
	// Resolve file to absolute path for cross-project detection
	var absFile string
	if filepath.IsAbs(file) {
		absFile = filepath.Clean(file)
	} else {
		absFile = filepath.Clean(filepath.Join(workDir, file))
	}

	// Cross-project handling: if the file is outside our project root,
	// find its own .kai/ directory and write the checkpoint there.
	if !strings.HasPrefix(absFile, workDir+string(filepath.Separator)) {
		if foreignKaiDir := findForeignKaiDir(absFile); foreignKaiDir != "" {
			foreignProject := filepath.Dir(foreignKaiDir)
			workDir = foreignProject
			kd = foreignKaiDir
		} else {
			return nil // file not in any kai project — skip silently
		}
	}

	// Make file path relative to the (possibly updated) workDir
	if rel, err := filepath.Rel(workDir, absFile); err == nil {
		file = rel
	}
	// Guard against editor buffers outside any kai project
	if strings.HasPrefix(file, "..") {
		return nil
	}
	newContent, err := os.ReadFile(absFile)
	if err != nil {
		return nil // file vanished or unreadable — nothing to checkpoint
	}

	// Agent name: flag > env > workspace metadata > session file > "mcp-client".
	agent := checkpointAgent
	if agent == "" {
		agent = os.Getenv("KAI_CHECKPOINT_AGENT")
	}
	if agent == "" {
		agent = readAgentFromCurrentWorkspace(kd)
	}
	if agent == "" {
		agent = readAgentFromSessionFile(kd)
	}
	if agent == "" {
		agent = "mcp-client"
	}
	model := checkpointModel

	// Compute the edited line range.
	// Strategy depends on which tool fired the hook:
	//   Write / MultiEdit without a file on disk before → whole file
	//   Edit with new_string → find new_string in the current file
	//   MultiEdit with edits → one checkpoint per edit
	//   Fallback (manual invocation with --lines) → use the flag as-is
	var ranges []authorship.LineRange
	switch {
	case checkpointLines != "":
		if r, ok := parseLineRange(checkpointLines); ok {
			ranges = append(ranges, r)
		}
	case payload.ToolName == "Write":
		// Whole file attribution — everything in the new content is AI.
		total := bytes.Count(newContent, []byte("\n"))
		if len(newContent) > 0 && newContent[len(newContent)-1] != '\n' {
			total++
		}
		if total < 1 {
			total = 1
		}
		ranges = append(ranges, authorship.LineRange{Start: 1, End: total})
	case payload.ToolName == "MultiEdit" && len(payload.ToolInput.Edits) > 0:
		for _, e := range payload.ToolInput.Edits {
			if r, ok := locateNewString([]byte(e.NewString), newContent); ok {
				ranges = append(ranges, r)
			}
		}
	case payload.ToolInput.NewString != "":
		if r, ok := locateNewString([]byte(payload.ToolInput.NewString), newContent); ok {
			ranges = append(ranges, r)
		}
	default:
		// No tool input and no --lines — fall back to entire file so
		// manual `kai checkpoint --file foo.go --agent me` still works.
		total := bytes.Count(newContent, []byte("\n"))
		if len(newContent) > 0 && newContent[len(newContent)-1] != '\n' {
			total++
		}
		if total < 1 {
			total = 1
		}
		ranges = append(ranges, authorship.LineRange{Start: 1, End: total})
	}

	if len(ranges) == 0 {
		return nil
	}

	// Grab a session id so multiple checkpoints in one session group together.
	sessionID := os.Getenv("KAI_CHECKPOINT_SESSION")
	if sessionID == "" {
		sessionID = fmt.Sprintf("hook_%d", time.Now().UnixNano())
	}
	writer := authorship.NewCheckpointWriter(kd, sessionID)
	now := time.Now().UnixMilli()
	for _, r := range ranges {
		writer.Write(authorship.CheckpointRecord{
			File:       file,
			StartLine:  r.Start,
			EndLine:    r.End,
			Action:     "modify",
			AuthorType: "ai",
			Agent:      agent,
			Model:      model,
			SessionID:  sessionID,
			Timestamp:  now,
		})
	}
	return nil
}

// locateNewString finds where `needle` currently sits in `haystack` and
// returns its line range. If the needle appears more than once we pick the
// first occurrence — by convention Claude Code's Edit tool requires unique
// old_strings, so the first-match choice is safe in practice.
func locateNewString(needle, haystack []byte) (authorship.LineRange, bool) {
	if len(needle) == 0 {
		return authorship.LineRange{}, false
	}
	idx := bytes.Index(haystack, needle)
	if idx < 0 {
		return authorship.LineRange{}, false
	}
	// Count newlines up to idx → start line (1-indexed).
	start := 1 + bytes.Count(haystack[:idx], []byte("\n"))
	// Count newlines inside the needle → end line.
	span := bytes.Count(needle, []byte("\n"))
	end := start + span
	return authorship.LineRange{Start: start, End: end}, true
}

// parseLineRange accepts "start-end" or just "N" (single line).
func parseLineRange(s string) (authorship.LineRange, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return authorship.LineRange{}, false
	}
	if dash := strings.Index(s, "-"); dash >= 0 {
		startStr := strings.TrimSpace(s[:dash])
		endStr := strings.TrimSpace(s[dash+1:])
		start, err1 := strconv.Atoi(startStr)
		end, err2 := strconv.Atoi(endStr)
		if err1 != nil || err2 != nil || start < 1 || end < start {
			return authorship.LineRange{}, false
		}
		return authorship.LineRange{Start: start, End: end}, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return authorship.LineRange{}, false
	}
	return authorship.LineRange{Start: n, End: n}, true
}

// readAgentFromSessionFile returns the agent name from the most recently
// updated mcp-session-*.json in .kai/, or "" if none.
func readAgentFromSessionFile(kaiDir string) string {
	entries, err := os.ReadDir(kaiDir)
	if err != nil {
		return ""
	}
	var best map[string]interface{}
	var bestAt float64
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "mcp-session-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(kaiDir, e.Name()))
		if err != nil {
			continue
		}
		var s map[string]interface{}
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		u, _ := s["updatedAt"].(float64)
		if u > bestAt {
			best = s
			bestAt = u
		}
	}
	if best == nil {
		return ""
	}
	if a, ok := best["agent"].(string); ok {
		return a
	}
	return ""
}

// readAgentFromCurrentWorkspace looks up the agent name stored in the
// current workspace's metadata, written by `kai spawn --agent <name>`.
// Returns "" on any failure so the checkpoint resolution ladder falls
// through to the next source. Opens the graph DB read-only on the hot
// path; this is the cost of avoiding state duplication.
func readAgentFromCurrentWorkspace(kdPath string) string {
	wsName, err := os.ReadFile(filepath.Join(kdPath, workspaceFile))
	if err != nil || len(bytes.TrimSpace(wsName)) == 0 {
		return ""
	}
	dbPath := filepath.Join(kdPath, dbFile)
	objPath := filepath.Join(kdPath, objectsDir)
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	mgr := workspace.NewManager(db)
	ws, err := mgr.Get(strings.TrimSpace(string(wsName)))
	if err != nil || ws == nil {
		return ""
	}
	return ws.AgentName
}

func openDB() (*graph.DB, error) {
	// Check if .kai directory exists
	if _, err := os.Stat(kaiDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("Kai not initialized. Run 'kai init' first")
	}
	dbPath := filepath.Join(kaiDir, dbFile)
	objPath := filepath.Join(kaiDir, objectsDir)
	debugf("opening database: %s", dbPath)
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		return nil, err
	}
	debugf("database opened successfully")
	return db, nil
}

// applyDBSchema applies the database schema to a fresh database.
// Used for ephemeral databases in --git-range mode.
func applyDBSchema(db *graph.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS nodes (
  id BLOB PRIMARY KEY,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS nodes_kind ON nodes(kind);

CREATE TABLE IF NOT EXISTS edges (
  src BLOB NOT NULL,
  type TEXT NOT NULL,
  dst BLOB NOT NULL,
  at  BLOB,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (src, type, dst, at)
);

CREATE INDEX IF NOT EXISTS edges_src ON edges(src);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst);
CREATE INDEX IF NOT EXISTS edges_type ON edges(type);
CREATE INDEX IF NOT EXISTS edges_at ON edges(at);

CREATE TABLE IF NOT EXISTS refs (
  name TEXT PRIMARY KEY,
  target_id BLOB NOT NULL,
  target_kind TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS refs_kind ON refs(target_kind);

CREATE TABLE IF NOT EXISTS slugs (
  target_id BLOB PRIMARY KEY,
  slug TEXT UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS logs (
  kind TEXT NOT NULL,
  seq INTEGER NOT NULL,
  id BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (kind, seq)
);
CREATE INDEX IF NOT EXISTS logs_id ON logs(id);

CREATE TABLE IF NOT EXISTS ref_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  old_target BLOB,
  new_target BLOB NOT NULL,
  actor TEXT,
  moved_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ref_log_name ON ref_log(name);
CREATE INDEX IF NOT EXISTS ref_log_moved_at ON ref_log(moved_at);

CREATE INDEX IF NOT EXISTS nodes_created_at ON nodes(created_at);
`
	tx, err := db.BeginTx()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func loadMatcher() (*module.Matcher, error) {
	// Try the new location first (.kai/rules/modules.yaml)
	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return nil, err
	}
	if len(matcher.GetAllModules()) > 0 {
		debugf("modules: loaded %d modules from %s", len(matcher.GetAllModules()), modulesRulesPath)
		return matcher, nil
	}

	// Fall back to legacy location (kai.modules.yaml in project root)
	m, err := module.LoadRulesOrEmpty(modulesFile)
	if err != nil {
		return nil, err
	}
	debugf("modules: loaded %d modules from %s", len(m.GetAllModules()), modulesFile)
	return m, nil
}

// getCurrentWorkspace reads the current workspace name from .kai/workspace
func getCurrentWorkspace() (string, error) {
	path := filepath.Join(kaiDir, workspaceFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // No current workspace
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// setCurrentWorkspace writes the current workspace name to .kai/workspace
func setCurrentWorkspace(name string) error {
	path := filepath.Join(kaiDir, workspaceFile)
	if name == "" {
		// Clear current workspace
		os.Remove(path)
		return nil
	}
	return os.WriteFile(path, []byte(name+"\n"), 0644)
}

func runListSnapshots(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	nodes, err := db.GetNodesByKind(graph.KindSnapshot)
	if err != nil {
		return fmt.Errorf("listing snapshots: %w", err)
	}

	// Build ID -> ref name(s) map (used by both text and JSON paths).
	refMgr := ref.NewRefManager(db)
	allRefs, _ := refMgr.List(nil)
	idToRefs := make(map[string][]string)
	for _, r := range allRefs {
		hex := util.BytesToHex(r.TargetID)
		idToRefs[hex] = append(idToRefs[hex], r.Name)
	}

	if snapshotListJSON {
		// Machine-readable path. Emit a JSON array (possibly empty)
		// so consumers can count length without text parsing. Field
		// names mirror what kai spawn list --json uses where they
		// overlap (id, refs).
		type snapshotJSON struct {
			ID         string   `json:"id"`
			Refs       []string `json:"refs"`
			SourceType string   `json:"source_type"`
			FileCount  int      `json:"file_count"`
		}
		out := make([]snapshotJSON, 0, len(nodes))
		for _, node := range nodes {
			sourceType, _ := node.Payload["sourceType"].(string)
			fileCount := 0
			if fc, ok := node.Payload["fileCount"].(float64); ok {
				fileCount = int(fc)
			}
			hex := util.BytesToHex(node.ID)
			refs := idToRefs[hex]
			if refs == nil {
				refs = []string{}
			}
			out = append(out, snapshotJSON{
				ID:         hex,
				Refs:       refs,
				SourceType: sourceType,
				FileCount:  fileCount,
			})
		}
		data, mErr := json.Marshal(out)
		if mErr != nil {
			return fmt.Errorf("marshaling snapshot list: %w", mErr)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(nodes) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	fmt.Printf("%-30s  %-12s  %-10s  %s\n", "REF", "ID", "TYPE", "FILES")
	fmt.Println(strings.Repeat("-", 80))
	for _, node := range nodes {
		sourceType, _ := node.Payload["sourceType"].(string)

		fileCount := ""
		if fc, ok := node.Payload["fileCount"].(float64); ok {
			fileCount = fmt.Sprintf("%.0f", fc)
		}

		hex := util.BytesToHex(node.ID)
		refNames := idToRefs[hex]
		refDisplay := "-"
		if len(refNames) > 0 {
			refDisplay = strings.Join(refNames, ", ")
		}

		fmt.Printf("%-30s  %-12s  %-10s  %s\n", refDisplay, hex[:12], sourceType, fileCount)
	}

	return nil
}

func runListChangesets(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	nodes, err := db.GetNodesByKind(graph.KindChangeSet)
	if err != nil {
		return fmt.Errorf("listing changesets: %w", err)
	}

	if len(nodes) == 0 {
		fmt.Println("No changesets found.")
		return nil
	}

	fmt.Printf("%-64s  %s\n", "ID", "INTENT")
	fmt.Println(strings.Repeat("-", 80))
	for _, node := range nodes {
		intent, _ := node.Payload["intent"].(string)
		if intent == "" {
			intent = "(no intent)"
		}
		fmt.Printf("%-64s  %s\n", util.BytesToHex(node.ID), intent)
	}

	return nil
}

func runListSymbols(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	snapshotID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving snapshot ID: %w", err)
	}

	// Get all files in the snapshot
	edges, err := db.GetEdges(snapshotID, graph.EdgeHasFile)
	if err != nil {
		return fmt.Errorf("getting snapshot files: %w", err)
	}

	if len(edges) == 0 {
		fmt.Println("No files in snapshot.")
		return nil
	}

	// Build a map of file ID -> path
	fileIDToPath := make(map[string]string)
	for _, edge := range edges {
		node, err := db.GetNode(edge.Dst)
		if err != nil {
			continue
		}
		if node != nil {
			if path, ok := node.Payload["path"].(string); ok {
				fileIDToPath[util.BytesToHex(node.ID)] = path
			}
		}
	}

	// Get symbols for each file, grouped by file path
	type symbolInfo struct {
		Kind      string
		Name      string
		Signature string
	}
	fileSymbols := make(map[string][]symbolInfo)
	totalSymbols := 0

	matcher, err := loadMatcher()
	if err != nil {
		return err
	}
	creator := snapshot.NewCreator(db, matcher)

	for _, edge := range edges {
		symbols, err := creator.GetSymbolsInFile(edge.Dst, snapshotID)
		if err != nil {
			continue
		}
		if len(symbols) == 0 {
			continue
		}

		fileID := util.BytesToHex(edge.Dst)
		path := fileIDToPath[fileID]
		if path == "" {
			path = fileID[:16] + "..."
		}

		for _, sym := range symbols {
			kind, _ := sym.Payload["kind"].(string)
			fqName, _ := sym.Payload["fqName"].(string)
			signature, _ := sym.Payload["signature"].(string)

			fileSymbols[path] = append(fileSymbols[path], symbolInfo{
				Kind:      kind,
				Name:      fqName,
				Signature: signature,
			})
			totalSymbols++
		}
	}

	if totalSymbols == 0 {
		fmt.Println("No symbols found. Run 'kai analyze symbols <snapshot-id>' first.")
		return nil
	}

	// Sort file paths for consistent output
	var paths []string
	for path := range fileSymbols {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// Print symbols grouped by file
	for _, path := range paths {
		fmt.Printf("\n%s\n", path)
		for _, sym := range fileSymbols[path] {
			if sym.Kind == "function" && sym.Signature != "" {
				// For functions, signature already includes "function" keyword
				fmt.Printf("  %s\n", sym.Signature)
			} else if sym.Signature != "" {
				fmt.Printf("  %s %s\n", sym.Kind, sym.Signature)
			} else {
				fmt.Printf("  %s %s\n", sym.Kind, sym.Name)
			}
		}
	}

	fmt.Printf("\n%d symbols in %d files\n", totalSymbols, len(paths))
	return nil
}

// logEntry represents an entry in the log timeline
type logEntry struct {
	ID        string
	Kind      string
	CreatedAt int64
	Summary   string
	Details   map[string]string
}

func runLog(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)

	// Get all snapshot refs sorted by updatedAt descending
	allRefs, err := refMgr.List(nil)
	if err != nil {
		return fmt.Errorf("listing refs: %w", err)
	}

	// Filter to snapshot refs only, sort newest first
	var snapRefs []*ref.Ref
	for _, r := range allRefs {
		if r.TargetKind == ref.KindSnapshot && strings.HasPrefix(r.Name, "snap.") {
			snapRefs = append(snapRefs, r)
		}
	}

	// When a workspace is checked out, its captures advance ws.<name>.head
	// and record changesets but create no snap.* ref, so the loop above
	// can't see them — `kai log` would show only trunk. Surface the
	// workspace lineage by synthesizing snapshot entries from its
	// changesets; they dedupe/sort alongside trunk below, so branch
	// captures appear above the base they branched from.
	if cw, _ := getCurrentWorkspace(); cw != "" {
		mgr := workspace.NewManager(db)
		if csets, err := mgr.GetLog(cw); err == nil {
			for _, cs := range csets {
				headHex, _ := cs.Payload["head"].(string)
				if headHex == "" {
					continue
				}
				headID, err := util.HexToBytes(headHex)
				if err != nil {
					continue
				}
				createdAt, _ := cs.Payload["createdAt"].(float64)
				desc, _ := cs.Payload["description"].(string)
				meta := map[string]string{}
				if desc != "" {
					meta["kai.message"] = desc
				}
				snapRefs = append(snapRefs, &ref.Ref{
					Name:       "ws." + cw + ".head",
					TargetKind: ref.KindSnapshot,
					TargetID:   headID,
					UpdatedAt:  int64(createdAt),
					Meta:       meta,
				})
			}
		}
	}

	sort.Slice(snapRefs, func(i, j int) bool {
		return snapRefs[i].UpdatedAt > snapRefs[j].UpdatedAt
	})

	// Dedupe by target ID (snap.latest and snap.YYYYMMDD point to the same snapshot)
	seen := make(map[string]bool)
	var unique []*ref.Ref
	for _, r := range snapRefs {
		key := util.BytesToHex(r.TargetID)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, r)
	}

	// Parse --since/--until dates
	var sinceTime, untilTime time.Time
	if logSince != "" {
		sinceTime = parseRelativeDate(logSince)
	}
	if logUntil != "" {
		untilTime = parseRelativeDate(logUntil)
	}

	// Apply filters
	var filtered []*ref.Ref
	for _, r := range unique {
		t := time.UnixMilli(r.UpdatedAt)

		// --since filter
		if !sinceTime.IsZero() && t.Before(sinceTime) {
			continue
		}
		// --until filter
		if !untilTime.IsZero() && t.After(untilTime) {
			continue
		}
		// --author filter
		if logAuthor != "" {
			if r.Meta == nil {
				continue
			}
			author := r.Meta["git.author"]
			if !strings.Contains(strings.ToLower(author), strings.ToLower(logAuthor)) {
				continue
			}
		}
		// --grep filter
		if logGrep != "" {
			if r.Meta == nil {
				continue
			}
			msg := r.Meta["git.message"]
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(logGrep)) {
				continue
			}
		}

		filtered = append(filtered, r)
	}

	if logLimit > 0 && len(filtered) > logLimit {
		filtered = filtered[:logLimit]
	}

	// JSON output path. Mirrors the existing patterns on stats /
	// spawn list / snapshot list / gate list: same data the text
	// path renders, emitted as a JSON array. Empty array when
	// nothing matches — machine consumers shouldn't have to special-
	// case "No snapshots found." Returns immediately so the text
	// loop is never entered. The 2026-05-27 executor that
	// introduced the --json flag shipped only the var declaration +
	// flag registration; this branch is the load-bearing piece that
	// was missed (EDIT CHECKLIST item #3 in that plan was skipped
	// despite COMPLETE THE WIRE shipping in v0.32.61 — the
	// executor hit MaxTurns=20 three times and fragmented the work
	// across retries).
	if logJSON {
		type snapJSON struct {
			SnapID    string `json:"snap_id"`
			GitSHA    string `json:"git_sha,omitempty"`
			GitBranch string `json:"git_branch,omitempty"`
			GitAuthor string `json:"git_author,omitempty"`
			GitMsg    string `json:"git_message,omitempty"`
			KaiMsg    string `json:"kai_message,omitempty"`
			KaiAuthor string `json:"kai_author,omitempty"`
			UpdatedAt int64  `json:"updated_at_ms"`
		}
		out := make([]snapJSON, 0, len(filtered))
		for _, r := range filtered {
			entry := snapJSON{
				SnapID:    util.BytesToHex(r.TargetID),
				UpdatedAt: r.UpdatedAt,
			}
			if r.Meta != nil {
				entry.GitSHA = r.Meta["git.sha"]
				entry.GitBranch = r.Meta["git.branch"]
				entry.GitAuthor = r.Meta["git.author"]
				entry.GitMsg = r.Meta["git.message"]
				entry.KaiMsg = r.Meta["kai.message"]
				entry.KaiAuthor = r.Meta["kai.author"]
			}
			out = append(out, entry)
		}
		data, mErr := json.Marshal(out)
		if mErr != nil {
			return fmt.Errorf("marshaling log: %w", mErr)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(filtered) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	for i, r := range filtered {
		snapID := shortID(util.BytesToHex(r.TargetID))

		// Build display from metadata. Prefer the snap's own
		// kai.message (set by `kai capture -m "..."`) over the
		// underlying git commit message — without this, every
		// snap on a single git commit shows the same "initial
		// commit" string and the user can't tell what each
		// capture was about. Track the source so the renderer
		// below can dim git-fallback text.
		gitSHA := ""
		gitMsg := ""
		gitAuthor := ""
		gitBranch := ""
		kaiMsg := ""
		kaiAuthor := ""
		if r.Meta != nil {
			gitSHA = r.Meta["git.sha"]
			gitMsg = r.Meta["git.message"]
			gitAuthor = r.Meta["git.author"]
			gitBranch = r.Meta["git.branch"]
			kaiMsg = r.Meta["kai.message"]
			kaiAuthor = r.Meta["kai.author"]
		}
		// displayMsg is what we actually print; msgIsGitFallback
		// flags whether to render it dimmed with a "(git)" suffix.
		displayMsg := kaiMsg
		msgIsGitFallback := false
		if displayMsg == "" {
			displayMsg = gitMsg
			msgIsGitFallback = displayMsg != ""
		}

		// --oneline mode
		if logOneline {
			line := fmt.Sprintf("\033[33m%s\033[0m", snapID)
			if gitSHA != "" {
				line += fmt.Sprintf(" (%s)", gitSHA)
			}
			if displayMsg != "" {
				if msgIsGitFallback {
					line += " \033[2m" + displayMsg + " (git)\033[0m"
				} else {
					line += " " + displayMsg
				}
			}
			fmt.Println(line)
			continue
		}

		if i > 0 {
			fmt.Println()
		}

		timestamp := time.UnixMilli(r.UpdatedAt).Format("2006-01-02 15:04:05")

		// Header line: yellow commit-style
		if gitSHA != "" {
			fmt.Printf("\033[33msnap %s\033[0m (git %s", snapID, gitSHA)
			if gitBranch != "" {
				fmt.Printf(", %s", gitBranch)
			}
			fmt.Println(")")
		} else {
			fmt.Printf("\033[33msnap %s\033[0m\n", snapID)
		}

		// Author. Two distinct concepts:
		//   - kai.author: who took THIS snapshot (the kai user)
		//   - git.author: who authored the underlying commit (upstream)
		// They're often the same on repos you actively commit to;
		// they diverge on starter kits, freshly-cloned vendor code,
		// and when kai ran on your behalf without you having yet
		// committed. We render kai.author as "Author:" and surface
		// git.author below ONLY when it differs (avoids visual
		// noise on the common case where they match).
		switch {
		case kaiAuthor != "":
			fmt.Printf("Author:    %s\n", kaiAuthor)
			if gitAuthor != "" && !sameAuthor(kaiAuthor, gitAuthor) {
				fmt.Printf("\033[2mGit commit by: %s\033[0m\n", gitAuthor)
			}
		case gitAuthor != "":
			// No kai.author recorded (older snapshot, pre this fix).
			// Fall back to the git author with a label that makes
			// the source clear.
			fmt.Printf("Git author: %s\n", gitAuthor)
		}

		// Date
		fmt.Printf("Date:    %s\n", timestamp)

		// Files + changes
		node, err := db.GetNode(r.TargetID)
		fileCount := ""
		if err == nil && node != nil {
			if fc, ok := node.Payload["fileCount"].(float64); ok {
				fileCount = fmt.Sprintf("%d files", int(fc))
			}
		}
		if fileCount != "" {
			changesFiles := ""
			changesSummary := ""
			if r.Meta != nil {
				changesFiles = r.Meta["changes.files"]
				changesSummary = r.Meta["changes.summary"]
			}
			if changesFiles != "" {
				fmt.Printf("Files:   %s (%s modified)\n", fileCount, changesFiles)
			} else {
				fmt.Printf("Files:   %s\n", fileCount)
			}
			if changesSummary != "" {
				fmt.Printf("Changes: %s\n", changesSummary)
			}
		}

		// Message — kai.message wins; git.message renders dimmed
		// with a "(git)" suffix so the user can tell at a glance
		// the snap inherited its headline from git rather than
		// having its own.
		if displayMsg != "" {
			if msgIsGitFallback {
				fmt.Printf("\n    \033[2m%s (git)\033[0m\n", displayMsg)
			} else {
				fmt.Printf("\n    %s\n", displayMsg)
			}
		}
	}

	return nil
}

// parseRelativeDate parses dates like "2 weeks ago", "yesterday", "2026-04-01".
func parseRelativeDate(s string) time.Time {
	s = strings.TrimSpace(strings.ToLower(s))
	now := time.Now()

	// Try absolute date formats
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05", "Jan 2 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}

	// Relative dates
	switch s {
	case "yesterday":
		return now.AddDate(0, 0, -1)
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "last week":
		return now.AddDate(0, 0, -7)
	case "last month":
		return now.AddDate(0, -1, 0)
	}

	// "N <unit> ago" pattern
	s = strings.TrimSuffix(s, " ago")
	parts := strings.Fields(s)
	if len(parts) == 2 {
		n, err := strconv.Atoi(parts[0])
		if err == nil {
			unit := strings.TrimSuffix(parts[1], "s") // "weeks" -> "week"
			switch unit {
			case "minute", "min":
				return now.Add(-time.Duration(n) * time.Minute)
			case "hour", "hr":
				return now.Add(-time.Duration(n) * time.Hour)
			case "day":
				return now.AddDate(0, 0, -n)
			case "week":
				return now.AddDate(0, 0, -n*7)
			case "month":
				return now.AddDate(0, -n, 0)
			case "year":
				return now.AddDate(-n, 0, 0)
			}
		}
	}

	return time.Time{} // zero value = no filter
}

func runStatus(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("status")
	defer te.Finish()

	// Check if Kai is initialized
	if _, err := os.Stat(kaiDir); os.IsNotExist(err) {
		fmt.Println("Not a Kai repository (run 'kai init' to initialize)")
		return nil
	}

	// Verification surface first (Horizon 1): contracts + structural verdicts,
	// then the working-tree diff below.
	renderVerificationStatus(os.Stdout)

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// For JSON output, skip the header info
	if !statusJSON && !statusNameOnly {
		fmt.Println("Kai initialized")
		fmt.Println()

		// Show current workspace
		currentWs, _ := getCurrentWorkspace()
		if currentWs != "" {
			fmt.Printf("Workspace:  %s\n", currentWs)
		} else {
			fmt.Println("Workspace:  (none) — working on snap.latest")
		}

		// Count snapshots and changesets
		snapshots, err := db.GetNodesByKind(graph.KindSnapshot)
		if err != nil {
			return err
		}
		changesets, err := db.GetNodesByKind(graph.KindChangeSet)
		if err != nil {
			return err
		}

		fmt.Printf("Snapshots:  %d\n", len(snapshots))
		fmt.Printf("Changesets: %d\n", len(changesets))
		fmt.Println()

		if len(snapshots) > 0 {
			fmt.Printf("Checking for changes in: %s\n", statusDir)
			fmt.Println()
		}
	}

	// On a workspace, the baseline is the workspace head, not snap.latest
	// (which tracks trunk). Only override when the user didn't pass an
	// explicit --against, so `kai status` reports changes since the last
	// capture on this branch rather than the whole branch vs trunk.
	effectiveAgainst := statusAgainst
	if effectiveAgainst == "" {
		if cw, _ := getCurrentWorkspace(); cw != "" {
			effectiveAgainst = "ws." + cw + ".head"
		}
	}

	// Compute status using the new status package
	result, err := status.Compute(db, status.Options{
		Dir:      statusDir,
		Against:  effectiveAgainst,
		UseCache: true,
		CacheDir: ".", // Cache in repo root's .kai/cache, not in scanned dir
	})
	if err != nil {
		return err
	}

	// Run semantic analysis if requested
	var semantic *status.SemanticResult
	if statusSemantic && len(result.Modified) > 0 {
		semantic, err = status.AnalyzeSemantic(db, result, status.SemanticOptions{
			Dir: statusDir,
		})
		if err != nil {
			// Non-fatal - continue without semantic info
			fmt.Fprintf(os.Stderr, "warning: semantic analysis failed: %v\n", err)
		}
	}

	// Show explain if requested
	if statusExplain {
		hasBaseline := !result.NoBaseline
		ctx := explain.ExplainStatus(hasBaseline, len(result.Modified), len(result.Added), len(result.Deleted))
		ctx.Print(os.Stdout)
	}

	// Determine output format
	var format status.OutputFormat
	if statusJSON {
		format = status.FormatJSON
	} else if statusNameOnly {
		format = status.FormatNameOnly
	} else {
		format = status.FormatDefault
	}

	// Write output
	if err := status.WriteOutputWithSemantic(os.Stdout, result, semantic, format); err != nil {
		return err
	}

	// For default format, add helpful suggestion if there are changes
	if format == status.FormatDefault && result.HasChanges() {
		fmt.Println()
		if result.NoBaseline {
			fmt.Println("Create your first snapshot with:")
			fmt.Println("  kai capture")
		} else {
			fmt.Println("To see semantic differences:")
			fmt.Println("  kai diff")
			fmt.Println()
			fmt.Println("To capture these changes:")
			fmt.Println("  kai capture")
		}
	}

	return nil
}

func runDiff(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("diff")
	defer te.Finish()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Semantic is the default unless --name-only or --patch is specified
	// JSON also implies semantic
	if !diffNameOnly && !diffPatch && !diffStat {
		diffSemantic = true
	}
	if diffJSON {
		diffSemantic = true
	}

	// Parse a single arg of the form "A..B" as two snapshot refs,
	// matching git's syntax. Common ask: after `kai code` lands an
	// agent's edits, the user pastes the two snap IDs from `kai
	// log` separated by `..` and expects to see what changed.
	// Without this they get "not found: A..B" because the whole
	// string is treated as one ref.
	if len(args) == 1 && strings.Contains(args[0], "..") {
		parts := strings.SplitN(args[0], "..", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			args = []string{parts[0], parts[1]}
		}
	}

	// Default to @snap:last if no args provided (simple mode friendly).
	// On a workspace, default to the workspace head instead — snap.latest
	// tracks trunk, so diffing against it would show the whole branch.
	baseRef := "@snap:last"
	if cw, _ := getCurrentWorkspace(); cw != "" {
		baseRef = "ws." + cw + ".head"
	}
	if len(args) >= 1 {
		baseRef = args[0]
	}

	// Check for stale baseline warning (unless --force or comparing two explicit snapshots)
	if !diffForce && len(args) < 2 {
		refMgr := ref.NewRefManager(db)
		workingRef, _ := refMgr.Get("snap.working")
		if workingRef != nil {
			staleThreshold := int64(10 * 60 * 1000) // 10 minutes in ms
			age := util.NowMs() - workingRef.UpdatedAt
			if age > staleThreshold {
				ageMinutes := age / 60000
				fmt.Fprintf(os.Stderr, "Warning: Last capture was %d minutes ago.\n", ageMinutes)
				fmt.Fprintf(os.Stderr, "  Your working directory may have changed since then.\n")
				fmt.Fprintf(os.Stderr, "  Run 'kai capture' for an accurate diff, or use --force to continue.\n\n")
			}
		}
	}

	// Resolve base snapshot
	baseSnapID, err := resolveSnapshotID(db, baseRef)
	if err != nil {
		// Friendly error for simple mode users
		if baseRef == "@snap:last" {
			fmt.Println()
			fmt.Println("No snapshots found. Create one first:")
			fmt.Println()
			fmt.Println("  kai capture    # Recommended: capture and analyze in one step")
			fmt.Println()
			return fmt.Errorf("no snapshots available")
		}
		return fmt.Errorf("resolving base snapshot: %w", err)
	}

	creator := snapshot.NewCreator(db, nil)

	// Get base snapshot files
	baseFiles, err := creator.GetSnapshotFiles(baseSnapID)
	if err != nil {
		return fmt.Errorf("getting base files: %w", err)
	}

	// Load content if needed for semantic, patch, or stat mode
	needContent := diffSemantic || diffPatch || diffStat

	baseFileMap := make(map[string]string) // path -> digest
	baseContent := make(map[string][]byte) // path -> content (for semantic/patch diff)
	for _, f := range baseFiles {
		path, _ := f.Payload["path"].(string)
		digest, _ := f.Payload["digest"].(string)
		baseFileMap[path] = digest
		if needContent {
			content, _ := creator.GetFileContent(digest)
			baseContent[path] = content
		}
	}

	var headFileMap map[string]string
	var headContent map[string][]byte
	var headLabel string

	if len(args) == 2 {
		// Compare two snapshots
		headSnapID, err := resolveSnapshotID(db, args[1])
		if err != nil {
			return fmt.Errorf("resolving head snapshot: %w", err)
		}

		headFiles, err := creator.GetSnapshotFiles(headSnapID)
		if err != nil {
			return fmt.Errorf("getting head files: %w", err)
		}

		headFileMap = make(map[string]string)
		headContent = make(map[string][]byte)
		for _, f := range headFiles {
			path, _ := f.Payload["path"].(string)
			digest, _ := f.Payload["digest"].(string)
			headFileMap[path] = digest
			if needContent {
				content, _ := creator.GetFileContent(digest)
				headContent[path] = content
			}
		}

		headLabel = util.BytesToHex(headSnapID)[:12]
	} else {
		// Compare snapshot vs working directory
		source, err := dirio.OpenDirectory(diffDir)
		if err != nil {
			return fmt.Errorf("opening directory: %w", err)
		}

		currentFiles, err := source.GetFiles()
		if err != nil {
			return fmt.Errorf("getting current files: %w", err)
		}

		headFileMap = make(map[string]string)
		headContent = make(map[string][]byte)
		for _, f := range currentFiles {
			headFileMap[f.Path] = util.Blake3HashHex(f.Content)
			if needContent {
				headContent[f.Path] = f.Content
			}
		}

		headLabel = "working directory"
	}

	// Compute file-level differences
	var added, modified, deleted []string

	for path, headDigest := range headFileMap {
		if baseDigest, exists := baseFileMap[path]; !exists {
			added = append(added, path)
		} else if headDigest != baseDigest {
			modified = append(modified, path)
		}
	}

	for path := range baseFileMap {
		if _, exists := headFileMap[path]; !exists {
			deleted = append(deleted, path)
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)

	// Show explain if requested (for both semantic and simple mode)
	fileCount := len(added) + len(modified) + len(deleted)
	if diffExplain {
		var changeTypes []string
		if len(added) > 0 {
			changeTypes = append(changeTypes, fmt.Sprintf("%d file(s) added", len(added)))
		}
		if len(modified) > 0 {
			changeTypes = append(changeTypes, fmt.Sprintf("%d file(s) modified", len(modified)))
		}
		if len(deleted) > 0 {
			changeTypes = append(changeTypes, fmt.Sprintf("%d file(s) deleted", len(deleted)))
		}
		// Get affected modules
		var modules []string
		if matcher, err := loadMatcher(); err == nil {
			modulesSet := make(map[string]bool)
			allPaths := append(append(added, modified...), deleted...)
			for _, path := range allPaths {
				for _, mod := range matcher.MatchPath(path) {
					modulesSet[mod] = true
				}
			}
			for mod := range modulesSet {
				modules = append(modules, mod)
			}
		}
		ctx := explain.ExplainDiffFull(baseRef, headLabel, fileCount, changeTypes, modules)
		ctx.Print(os.Stdout)
	}

	// No differences
	if len(added) == 0 && len(modified) == 0 && len(deleted) == 0 {
		if diffJSON {
			fmt.Println(`{"files":[],"summary":{"filesAdded":0,"filesModified":0,"filesRemoved":0,"unitsAdded":0,"unitsModified":0,"unitsRemoved":0}}`)
		} else {
			fmt.Println("No differences.")
		}
		return nil
	}

	// Semantic diff mode
	if diffSemantic {
		differ := diff.NewDiffer()
		sd := &diff.SemanticDiff{
			Base: util.BytesToHex(baseSnapID)[:12],
			Head: headLabel,
		}

		// Process added files
		for _, path := range added {
			fd, _ := differ.DiffFile(path, nil, headContent[path])
			if fd != nil {
				sd.Files = append(sd.Files, *fd)
			}
		}

		// Process modified files
		for _, path := range modified {
			fd, _ := differ.DiffFile(path, baseContent[path], headContent[path])
			if fd != nil {
				sd.Files = append(sd.Files, *fd)
			}
		}

		// Process deleted files
		for _, path := range deleted {
			fd, _ := differ.DiffFile(path, baseContent[path], nil)
			if fd != nil {
				sd.Files = append(sd.Files, *fd)
			}
		}

		sd.ComputeSummary()

		if diffJSON {
			jsonOut, err := sd.FormatJSON()
			if err != nil {
				return fmt.Errorf("formatting JSON: %w", err)
			}
			fmt.Println(string(jsonOut))
		} else {
			if stdoutColor {
				fmt.Printf("\033[1mDiff:\033[0m \033[2m%s → %s\033[0m\n\n", sd.Base, sd.Head)
				fmt.Print(sd.FormatTextColor())
			} else {
				fmt.Printf("Diff: %s → %s\n\n", sd.Base, sd.Head)
				fmt.Print(sd.FormatText())
			}
		}

		return nil
	}

	// Patch mode - line-level diff like git
	if diffPatch {
		fmt.Printf("Diff: %s → %s\n\n", util.BytesToHex(baseSnapID)[:12], headLabel)

		// Show added files
		for _, path := range added {
			fmt.Printf("\033[1mdiff --kai a/%s b/%s\033[0m\n", path, path)
			fmt.Println("--- /dev/null")
			fmt.Printf("+++ b/%s\n", path)
			content := headContent[path]
			if content != nil {
				lines := strings.Split(string(content), "\n")
				fmt.Printf("@@ -0,0 +1,%d @@\n", len(lines))
				for _, line := range lines {
					fmt.Printf("\033[32m+%s\033[0m\n", line)
				}
			}
			fmt.Println()
		}

		// Show modified files
		for _, path := range modified {
			fmt.Printf("\033[1mdiff --kai a/%s b/%s\033[0m\n", path, path)
			fmt.Printf("--- a/%s\n", path)
			fmt.Printf("+++ b/%s\n", path)
			before := baseContent[path]
			after := headContent[path]
			if before != nil && after != nil {
				showUnifiedDiff(string(before), string(after))
			}
			fmt.Println()
		}

		// Show deleted files
		for _, path := range deleted {
			fmt.Printf("\033[1mdiff --kai a/%s b/%s\033[0m\n", path, path)
			fmt.Printf("--- a/%s\n", path)
			fmt.Println("+++ /dev/null")
			content := baseContent[path]
			if content != nil {
				lines := strings.Split(string(content), "\n")
				fmt.Printf("@@ -1,%d +0,0 @@\n", len(lines))
				for _, line := range lines {
					fmt.Printf("\033[31m-%s\033[0m\n", line)
				}
			}
			fmt.Println()
		}

		return nil
	}

	// Simple file-level output
	if !diffJSON {
		fmt.Printf("Diff: %s → %s\n\n", util.BytesToHex(baseSnapID)[:12], headLabel)
	}

	if diffStat {
		// git diff --stat style: show each file with +/- line counts
		totalInsertions := 0
		totalDeletions := 0
		maxPathLen := 0
		type statEntry struct {
			path       string
			insertions int
			deletions  int
		}
		var entries []statEntry

		for _, path := range added {
			lines := strings.Count(string(headContent[path]), "\n")
			entries = append(entries, statEntry{path, lines, 0})
			totalInsertions += lines
			if len(path) > maxPathLen {
				maxPathLen = len(path)
			}
		}
		for _, path := range modified {
			oldLines := strings.Count(string(baseContent[path]), "\n")
			newLines := strings.Count(string(headContent[path]), "\n")
			ins := 0
			del := 0
			if newLines > oldLines {
				ins = newLines - oldLines
			} else {
				del = oldLines - newLines
			}
			// Rough estimate: changed lines = min(old, new) delta
			// For a better count, use actual line diff
			if ins == 0 && del == 0 {
				ins = 1 // at least something changed
				del = 1
			}
			entries = append(entries, statEntry{path, ins, del})
			totalInsertions += ins
			totalDeletions += del
			if len(path) > maxPathLen {
				maxPathLen = len(path)
			}
		}
		for _, path := range deleted {
			lines := strings.Count(string(baseContent[path]), "\n")
			entries = append(entries, statEntry{path, 0, lines})
			totalDeletions += lines
			if len(path) > maxPathLen {
				maxPathLen = len(path)
			}
		}

		if maxPathLen > 50 {
			maxPathLen = 50
		}

		for _, e := range entries {
			path := e.path
			if len(path) > 50 {
				path = "..." + path[len(path)-47:]
			}
			bar := strings.Repeat("+", e.insertions) + strings.Repeat("-", e.deletions)
			if len(bar) > 40 {
				ratio := float64(e.insertions) / float64(e.insertions+e.deletions)
				plus := int(ratio * 40)
				minus := 40 - plus
				bar = strings.Repeat("+", plus) + strings.Repeat("-", minus)
			}
			fmt.Printf(" %-*s | %s\n", maxPathLen, path, bar)
		}
		fmt.Printf(" %d files changed, %d insertions(+), %d deletions(-)\n",
			len(entries), totalInsertions, totalDeletions)
		return nil
	}

	if diffNameOnly {
		for _, path := range added {
			fmt.Printf("A %s\n", path)
		}
		for _, path := range modified {
			fmt.Printf("M %s\n", path)
		}
		for _, path := range deleted {
			fmt.Printf("D %s\n", path)
		}
	} else {
		if len(added) > 0 {
			fmt.Printf("Added (%d):\n", len(added))
			for _, path := range added {
				fmt.Printf("  + %s\n", path)
			}
			fmt.Println()
		}

		if len(modified) > 0 {
			fmt.Printf("Modified (%d):\n", len(modified))
			for _, path := range modified {
				fmt.Printf("  ~ %s\n", path)
			}
			fmt.Println()
		}

		if len(deleted) > 0 {
			fmt.Printf("Deleted (%d):\n", len(deleted))
			for _, path := range deleted {
				fmt.Printf("  - %s\n", path)
			}
		}
	}

	return nil
}

// Workspace command implementations

func runWsCreate(cmd *cobra.Command, args []string) error {
	// Get workspace name from positional arg or --name flag
	name := wsName
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		return fmt.Errorf("workspace name required (pass as argument or use --name)")
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var baseID []byte

	// Count how many base sources are specified
	sourceCount := 0
	if wsBase != "" {
		sourceCount++
	}
	if wsFromDir != "" {
		sourceCount++
	}
	if wsFromGit != "" {
		sourceCount++
	}

	// Check for conflicting options
	if sourceCount > 1 {
		fmt.Println()
		fmt.Println("╭─ Conflicting base options")
		fmt.Println("│")
		fmt.Println("│  You specified multiple base sources. Use only one of:")
		fmt.Println("│")
		fmt.Println("│    --from-dir <path>    # From directory snapshot")
		fmt.Println("│    --from-git <ref>     # From Git commit/branch/tag")
		fmt.Println("│    --base <selector>    # From existing snapshot")
		fmt.Println("│")
		fmt.Println("╰────────────────────────────────────────────")
		return fmt.Errorf("conflicting base options: use only one of --from-dir, --from-git, or --base")
	}

	if wsFromDir != "" {
		// Create base from directory
		fmt.Printf("Creating base snapshot from directory: %s\n", wsFromDir)
		fmt.Println()
		baseID, err = createSnapshotFromDir(db, wsFromDir)
		if err != nil {
			return fmt.Errorf("creating directory snapshot: %w", err)
		}
		fmt.Println()
	} else if wsFromGit != "" {
		// Create base from Git ref
		fmt.Printf("Creating base snapshot from Git ref: %s\n", wsFromGit)
		matcher, err := loadMatcher()
		if err != nil {
			return err
		}
		source, err := gitio.OpenSource(".", wsFromGit)
		if err != nil {
			return fmt.Errorf("opening git ref: %w", err)
		}
		creator := snapshot.NewCreator(db, matcher)
		baseID, err = creator.CreateSnapshot(source)
		if err != nil {
			return fmt.Errorf("creating git snapshot: %w", err)
		}
		// Analyze symbols
		progress := func(current, total int, filename string) {}
		_ = creator.AnalyzeSymbols(baseID, progress)

		// Update refs
		autoRefMgr := ref.NewAutoRefManager(db)
		_ = autoRefMgr.OnSnapshotCreated(baseID)
		fmt.Printf("Created base snapshot: %s\n", util.BytesToHex(baseID)[:12])
		fmt.Println()
	} else if wsBase != "" {
		// Explicit base snapshot provided
		baseID, err = resolveSnapshotID(db, wsBase)
		if err != nil {
			return fmt.Errorf("resolving base snapshot: %w", err)
		}
	} else {
		// Auto-snapshot current directory (default behavior)
		fmt.Println("No base specified, auto-snapshotting current directory...")
		fmt.Println()
		baseID, err = createSnapshotFromDir(db, ".")
		if err != nil {
			return fmt.Errorf("auto-snapshot failed: %w", err)
		}
		fmt.Println()
	}

	mgr := workspace.NewManager(db)
	ws, err := mgr.Create(name, baseID, wsDescription)
	if err != nil {
		return fmt.Errorf("creating workspace: %w", err)
	}

	// Update auto-refs
	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnWorkspaceCreated(name, baseID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	}

	fmt.Printf("Created workspace: %s\n", name)
	fmt.Printf("ID: %s\n", util.BytesToHex(ws.ID))
	fmt.Printf("Base snapshot: %s\n", util.BytesToHex(ws.BaseSnapshot)[:12])
	return nil
}

func runWsList(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)
	workspaces, err := mgr.List()
	if err != nil {
		return fmt.Errorf("listing workspaces: %w", err)
	}

	currentWs, _ := getCurrentWorkspace()

	if len(workspaces) == 0 {
		fmt.Println("No workspaces found.")
		return nil
	}

	fmt.Printf("  %-20s  %-10s  %-12s  %-12s  %s\n", "NAME", "STATUS", "BASE", "HEAD", "CHANGESETS")
	fmt.Println(strings.Repeat("-", 82))
	for _, ws := range workspaces {
		baseStr := ""
		headStr := ""
		if len(ws.BaseSnapshot) > 0 {
			baseStr = util.BytesToHex(ws.BaseSnapshot)[:12]
		}
		if len(ws.HeadSnapshot) > 0 {
			headStr = util.BytesToHex(ws.HeadSnapshot)[:12]
		}
		marker := " "
		if ws.Name == currentWs {
			marker = "*"
		}
		fmt.Printf("%s %-20s  %-10s  %-12s  %-12s  %d\n",
			marker, ws.Name, ws.Status, baseStr, headStr, len(ws.OpenChangeSets))
	}

	return nil
}

func runWsStage(cmd *cobra.Command, args []string) error {
	// Resolve workspace name: positional arg > --ws flag > current workspace
	name := wsName
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		// Try current workspace
		current, err := getCurrentWorkspace()
		if err != nil {
			return fmt.Errorf("reading current workspace: %w", err)
		}
		if current == "" {
			return fmt.Errorf("no workspace specified. Use 'kai ws stage <name>' or 'kai ws checkout <name>' first")
		}
		name = current
		fmt.Printf("Using current workspace: %s\n", name)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	matcher, err := loadMatcher()
	if err != nil {
		return err
	}

	// Open directory source
	source, err := dirio.OpenDirectory(wsDir)
	if err != nil {
		return fmt.Errorf("opening directory: %w", err)
	}

	signKey := wsSignKey
	if signKey == "" {
		signKey = os.Getenv("KAI_SSH_SIGN_KEY")
	}

	mgr := workspace.NewManager(db)
	result, err := mgr.Stage(name, source, matcher, wsStageMessage, &workspace.StageOptions{
		SignKeyPath: signKey,
	})
	if err != nil {
		return fmt.Errorf("staging changes: %w", err)
	}

	if result.ChangedFiles == 0 {
		fmt.Println("No changes to stage.")
		return nil
	}

	if len(result.Conflicts) > 0 {
		fmt.Printf("Conflicts detected (%d):\n", len(result.Conflicts))
		for _, c := range result.Conflicts {
			fmt.Printf("  %s: %s\n", c.Path, c.Description)
		}
		return fmt.Errorf("resolve conflicts before staging")
	}

	// Update auto-refs
	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnWorkspaceHeadChanged(name, result.HeadSnapshot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	}
	if err := autoRefMgr.OnChangeSetCreated(result.ChangeSetID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	}

	fmt.Printf("Staged changes:\n")
	fmt.Printf("  Changeset: %s\n", util.BytesToHex(result.ChangeSetID)[:12])
	fmt.Printf("  New head:  %s\n", util.BytesToHex(result.HeadSnapshot)[:12])
	fmt.Printf("  Files:     %d changed\n", result.ChangedFiles)
	fmt.Printf("  Changes:   %d change types detected\n", result.ChangeTypes)

	return nil
}

func runWsLog(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)

	ws, err := mgr.Get(wsName)
	if err != nil {
		return err
	}
	if ws == nil {
		return fmt.Errorf("workspace not found: %s", wsName)
	}

	changesets, err := mgr.GetLog(wsName)
	if err != nil {
		return fmt.Errorf("getting workspace log: %w", err)
	}

	fmt.Printf("Workspace: %s\n", ws.Name)
	fmt.Printf("Status:    %s\n", ws.Status)
	fmt.Printf("Base:      %s\n", util.BytesToHex(ws.BaseSnapshot)[:12])
	fmt.Printf("Head:      %s\n", util.BytesToHex(ws.HeadSnapshot)[:12])
	fmt.Println()

	if len(changesets) == 0 {
		fmt.Println("No changesets yet.")
		return nil
	}

	fmt.Printf("Changesets (%d):\n", len(changesets))
	for i, cs := range changesets {
		description, _ := cs.Payload["description"].(string)
		intent, _ := cs.Payload["intent"].(string)
		createdAt, _ := cs.Payload["createdAt"].(float64)
		t := time.UnixMilli(int64(createdAt))

		fmt.Printf("\n  [%d] %s\n", i+1, util.BytesToHex(cs.ID)[:12])
		fmt.Printf("      Date:   %s\n", t.Format("2006-01-02 15:04:05"))
		if description != "" {
			fmt.Printf("      Message: %s\n", description)
		}
		if intent != "" {
			fmt.Printf("      Intent: %s\n", intent)
		}
	}

	return nil
}

func runWsShelve(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)
	if err := mgr.Shelve(wsName); err != nil {
		return fmt.Errorf("shelving workspace: %w", err)
	}

	fmt.Printf("Workspace %q shelved.\n", wsName)
	return nil
}

func runWsUnshelve(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)
	if err := mgr.Unshelve(wsName); err != nil {
		return fmt.Errorf("unshelving workspace: %w", err)
	}

	fmt.Printf("Workspace %q unshelved.\n", wsName)
	return nil
}

func runWsClose(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)
	if err := mgr.Close(wsName); err != nil {
		return fmt.Errorf("closing workspace: %w", err)
	}

	fmt.Printf("Workspace %q closed.\n", wsName)
	return nil
}

func runWsDelete(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := workspace.NewManager(db)

	// Dry-run: show plan
	if wsDeleteDryRun {
		plan, err := mgr.PlanDelete(wsName)
		if err != nil {
			return fmt.Errorf("planning delete: %w", err)
		}

		fmt.Println("Plan:")
		fmt.Printf("  Remove Workspace node: %s\n", util.BytesToHex(plan.WorkspaceID)[:12])
		fmt.Printf("  Remove edges: %d\n", plan.EdgesRemoved)
		fmt.Printf("  Remove refs: %s\n", strings.Join(plan.RefsRemoved, ", "))
		if plan.OrphanedCSCount > 0 {
			fmt.Printf("Note: %d ChangeSet(s) will become unreferenced and eligible for GC.\n", plan.OrphanedCSCount)
		}
		fmt.Println("Run without --dry-run to apply.")
		return nil
	}

	// Actually delete
	if err := mgr.Delete(wsName, wsDeleteKeepRefs); err != nil {
		return fmt.Errorf("deleting workspace: %w", err)
	}

	fmt.Printf("Workspace %q deleted.\n", wsName)
	fmt.Println("Run `kai prune` to reclaim storage.")
	return nil
}

func runWsCheckout(cmd *cobra.Command, args []string) error {
	// Get workspace name from positional arg or --ws flag
	name := wsName
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		// No name given — leave the current workspace and return to trunk.
		currentWs, _ := getCurrentWorkspace()
		if currentWs == "" {
			fmt.Println("No workspace is currently checked out.")
			return nil
		}

		// Restore the working tree to trunk (snap.latest). Capture inside a
		// workspace stages into ws.<name>.head and leaves snap.latest pointing
		// at trunk, so the files on disk are still the workspace's edits. Without
		// this checkout, "leaving" the workspace only flips the .kai/workspace
		// pointer while the working tree keeps the workspace's code — the user
		// asked to go back to main, so materialize main.
		targetDir, err := filepath.Abs(wsDir)
		if err != nil {
			return fmt.Errorf("resolving target directory: %w", err)
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		trunkID, err := resolveSnapshotID(db, "snap.latest")
		if err != nil {
			// No trunk snapshot yet (e.g. fresh repo). Nothing to restore —
			// just clear the pointer and stop syncing.
			if serr := setCurrentWorkspace(""); serr != nil {
				return fmt.Errorf("clearing workspace: %w", serr)
			}
			stopAutoSync(kaiDir)
			fmt.Printf("Left workspace %q — no trunk snapshot to restore.\n", currentWs)
			return nil
		}

		// Materialize trunk first; only clear the workspace pointer once the
		// files are safely written. If the restore fails, the user is left in
		// the workspace (consistent state) rather than half-exited.
		creator := snapshot.NewCreator(db, nil)
		result, err := creator.Checkout(trunkID, targetDir, wsCheckoutClean)
		if err != nil {
			return fmt.Errorf("restoring trunk: %w", err)
		}

		if err := setCurrentWorkspace(""); err != nil {
			return fmt.Errorf("clearing workspace: %w", err)
		}
		stopAutoSync(kaiDir)

		fmt.Printf("Left workspace %q — now working on snap.latest (%s)\n", currentWs, util.BytesToHex(trunkID)[:12])
		fmt.Printf("  Restored: %d file(s)\n", result.FilesWritten)
		if result.FilesDeleted > 0 {
			fmt.Printf("  Deleted:  %d file(s)\n", result.FilesDeleted)
		}
		return nil
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Get the workspace
	mgr := workspace.NewManager(db)
	ws, err := mgr.Get(name)
	if err != nil {
		return fmt.Errorf("getting workspace: %w", err)
	}
	if ws == nil {
		return fmt.Errorf("workspace %q not found", name)
	}

	// Resolve target directory to absolute path
	targetDir, err := filepath.Abs(wsDir)
	if err != nil {
		return fmt.Errorf("resolving target directory: %w", err)
	}

	// Create target directory if it doesn't exist
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("creating target directory: %w", err)
	}

	// Checkout the head snapshot
	creator := snapshot.NewCreator(db, nil)
	result, err := creator.Checkout(ws.HeadSnapshot, targetDir, wsCheckoutClean)
	if err != nil {
		return fmt.Errorf("checkout failed: %w", err)
	}

	// Set as current workspace
	if err := setCurrentWorkspace(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set current workspace: %v\n", err)
	}

	fmt.Printf("Checkout complete!\n")
	fmt.Printf("  Workspace: %s\n", ws.Name)
	fmt.Printf("  Snapshot:  %s\n", util.BytesToHex(ws.HeadSnapshot)[:12])
	fmt.Printf("  Target:    %s\n", result.TargetDir)
	fmt.Printf("  Written:   %d file(s)\n", result.FilesWritten)
	if result.FilesDeleted > 0 {
		fmt.Printf("  Deleted:   %d file(s)\n", result.FilesDeleted)
	}
	if result.FilesSkipped > 0 {
		fmt.Printf("  Skipped:   %d file(s)\n", result.FilesSkipped)
	}
	fmt.Printf("\nNow on workspace: %s\n", name)

	// Default-on live sync: start a detached, network-aware sync daemon for
	// this workspace. Best-effort — never blocks or fails the checkout.
	startAutoSync(kaiDir, name)

	return nil
}

func runWsCurrent(cmd *cobra.Command, args []string) error {
	// When no workspace is explicitly checked out, "main" is the
	// implicit default — that's the snapshot's root and matches the
	// kai-desktop bridge's expected display. Both text and --json
	// modes return "main" for the no-checkout case so consumers get
	// a usable name unconditionally and don't have to special-case
	// help-text or empty strings.
	current, err := getCurrentWorkspace()
	if err != nil {
		if wsCurrentJSON {
			// JSON path surfaces the error too so downstream
			// callers don't have to scrape stderr to detect it.
			fmt.Println(`{"workspace":"main","error":` + jsonStringField(err.Error()) + `}`)
			return nil
		}
		return fmt.Errorf("reading current workspace: %w", err)
	}
	if current == "" {
		current = "main"
	}
	if wsCurrentJSON {
		fmt.Println(`{"workspace":` + jsonStringField(current) + `}`)
		return nil
	}
	fmt.Println(current)
	return nil
}

// jsonStringField encodes a single string as a JSON-quoted value
// (with the surrounding quotes and proper escaping). Avoids
// pulling encoding/json just to wrap one string.
func jsonStringField(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// heldByGateHint returns the message shown when the safety gate holds an
// integration. It must name the command that actually unblocks a held change —
// `kai gate approve <id>` — not `kai review`, which errors on a held (draft)
// change with "cannot transition from draft to approved" (F-17). The held
// result snapshot id is embedded so the suggested command is runnable as-is.
func heldByGateHint(resultSnapshot []byte) string {
	id := util.BytesToHex(resultSnapshot)
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("Change held by the safety gate. Run `kai gate approve %s` to publish it, or `kai gate list` to see held changes.", id)
}

func runIntegrate(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	targetID, err := resolveSnapshotID(db, wsTarget)
	if err != nil {
		return fmt.Errorf("resolving target snapshot: %w", err)
	}

	mgr := workspace.NewManager(db)

	// Load the gate config from the kai data dir. Missing file is fine —
	// safetygate.LoadConfig returns DefaultConfig() in that case.
	gateCfg, err := safetygate.LoadConfig(kaiDir)
	if err != nil {
		return fmt.Errorf("loading gate config: %w", err)
	}
	result, err := mgr.IntegrateWithOptions(wsName, targetID, workspace.IntegrateOptions{
		GateConfig: &gateCfg,
	})
	if err != nil {
		return fmt.Errorf("integrating workspace: %w", err)
	}

	if len(result.Conflicts) > 0 {
		fmt.Printf("Integration conflicts (%d):\n", len(result.Conflicts))
		for _, c := range result.Conflicts {
			fmt.Printf("  %s: %s\n", c.Path, c.Description)
		}
		fmt.Println()
		fmt.Printf("Run 'kai resolve %s' to address them.\n", wsName)
		return fmt.Errorf("resolve conflicts before integration")
	}

	fmt.Printf("Integration successful!\n")
	fmt.Printf("  Result snapshot: %s\n", util.BytesToHex(result.ResultSnapshot))
	fmt.Printf("  Applied %d changeset(s)\n", len(result.AppliedChangeSets))
	if result.AutoResolved > 0 {
		fmt.Printf("  Auto-resolved: %d change(s)\n", result.AutoResolved)
	}
	if result.Decision != nil {
		switch result.Decision.Verdict {
		case "review":
			fmt.Printf("  Gate: review (blast radius %d)\n", result.Decision.BlastRadius)
		case "block":
			fmt.Printf("  Gate: BLOCKED (blast radius %d)\n", result.Decision.BlastRadius)
		}
		for _, r := range result.Decision.Reasons {
			fmt.Printf("    · %s\n", r)
		}
	}

	// Advance refs via the workspace publish helper. PublishToRef advances
	// the named target ref (e.g. snap.latest) and the workspace's own
	// head ref. Without advancing the target, a second integrate from a
	// parallel workspace would fast-forward past this one because the
	// target still points at the original base, tripping the no-conflict
	// shortcut in integrateInternal.
	if ws, err := mgr.Get(wsName); err == nil && ws != nil {
		report, err := mgr.PublishToRef(ws, result, wsTarget, workspace.PublishOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: publish: %v\n", err)
		} else {
			for _, name := range report.AdvancedRefs {
				fmt.Printf("  %s -> %s\n", name, util.BytesToHex(result.ResultSnapshot)[:12])
			}
			if report.HeldByGate {
				fmt.Printf("  %s\n", heldByGateHint(result.ResultSnapshot))
			}
		}
	}

	return nil
}

func runMerge(cmd *cobra.Command, args []string) error {
	baseFile := args[0]
	leftFile := args[1]
	rightFile := args[2]

	// Read file contents
	baseContent, err := os.ReadFile(baseFile)
	if err != nil {
		return fmt.Errorf("reading base file: %w", err)
	}
	leftContent, err := os.ReadFile(leftFile)
	if err != nil {
		return fmt.Errorf("reading left file: %w", err)
	}
	rightContent, err := os.ReadFile(rightFile)
	if err != nil {
		return fmt.Errorf("reading right file: %w", err)
	}

	// Detect language from extension if not specified
	lang := mergeLang
	if lang == "" {
		ext := strings.ToLower(filepath.Ext(baseFile))
		switch ext {
		case ".js":
			lang = "js"
		case ".ts", ".tsx":
			lang = "ts"
		case ".py":
			lang = "py"
		case ".go":
			lang = "go"
		default:
			lang = "js" // fallback
		}
	}

	// Perform merge
	result, err := merge.Merge3Way(baseContent, leftContent, rightContent, lang)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	// Output as JSON if requested
	if mergeJSON {
		type jsonConflict struct {
			Kind      string `json:"kind"`
			Unit      string `json:"unit"`
			Message   string `json:"message"`
			LeftDiff  string `json:"leftDiff,omitempty"`
			RightDiff string `json:"rightDiff,omitempty"`
		}
		type jsonResult struct {
			Success   bool           `json:"success"`
			Conflicts []jsonConflict `json:"conflicts,omitempty"`
			Merged    string         `json:"merged,omitempty"`
		}

		jr := jsonResult{
			Success: result.Success,
		}
		for _, c := range result.Conflicts {
			jr.Conflicts = append(jr.Conflicts, jsonConflict{
				Kind:      string(c.Kind),
				Unit:      c.UnitKey.String(),
				Message:   c.Message,
				LeftDiff:  c.LeftDiff,
				RightDiff: c.RightDiff,
			})
		}
		if merged := result.Files["file"]; merged != nil {
			jr.Merged = string(merged)
		}

		out, err := json.MarshalIndent(jr, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling result: %w", err)
		}
		fmt.Println(string(out))
		return nil
	}

	// Text output
	if !result.Success {
		fmt.Fprintf(os.Stderr, "Merge conflicts detected (%d):\n\n", len(result.Conflicts))
		for _, c := range result.Conflicts {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", c.Kind, c.Message)
			fmt.Fprintf(os.Stderr, "    Unit: %s\n", c.UnitKey.String())
			if c.LeftDiff != "" {
				fmt.Fprintf(os.Stderr, "    Left:  %s\n", c.LeftDiff)
			}
			if c.RightDiff != "" {
				fmt.Fprintf(os.Stderr, "    Right: %s\n", c.RightDiff)
			}
			fmt.Fprintln(os.Stderr)
		}
		return fmt.Errorf("merge has conflicts")
	}

	// Success - output merged content
	merged := result.Files["file"]
	if merged == nil {
		return fmt.Errorf("no merged content produced")
	}

	if mergeOutput != "" {
		if err := os.WriteFile(mergeOutput, merged, 0644); err != nil {
			return fmt.Errorf("writing output file: %w", err)
		}
		fmt.Printf("Merged successfully -> %s\n", mergeOutput)
	} else {
		fmt.Print(string(merged))
	}

	return nil
}

func runCheckout(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	snapshotID, err := resolveSnapshotID(db, args[0])
	if err != nil {
		return fmt.Errorf("resolving snapshot ID: %w", err)
	}

	// Resolve target directory to absolute path
	targetDir, err := filepath.Abs(checkoutDir)
	if err != nil {
		return fmt.Errorf("resolving target directory: %w", err)
	}

	// Create target directory if it doesn't exist
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("creating target directory: %w", err)
	}

	creator := snapshot.NewCreator(db, nil)
	result, err := creator.Checkout(snapshotID, targetDir, checkoutClean)
	if err != nil {
		return fmt.Errorf("checkout failed: %w", err)
	}

	fmt.Printf("Checkout complete!\n")
	fmt.Printf("  Target:  %s\n", result.TargetDir)
	fmt.Printf("  Written: %d file(s)\n", result.FilesWritten)
	if result.FilesDeleted > 0 {
		fmt.Printf("  Deleted: %d file(s)\n", result.FilesDeleted)
	}
	if result.FilesSkipped > 0 {
		fmt.Printf("  Skipped: %d file(s)\n", result.FilesSkipped)
	}

	return nil
}

// Ref command implementations

func runRefList(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)

	var filterKind *ref.Kind
	if refKindFilter != "" {
		k := ref.Kind(refKindFilter)
		filterKind = &k
	}

	refs, err := refMgr.List(filterKind)
	if err != nil {
		return fmt.Errorf("listing refs: %w", err)
	}

	if len(refs) == 0 {
		fmt.Println("No refs found.")
		return nil
	}

	fmt.Printf("%-30s  %-12s  %s\n", "NAME", "KIND", "TARGET")
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range refs {
		fmt.Printf("%-30s  %-12s  %s\n", r.Name, r.TargetKind, util.BytesToHex(r.TargetID)[:16]+"...")
	}

	return nil
}

func runRefSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	target := args[1]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	resolver := ref.NewResolver(db)
	refMgr := ref.NewRefManager(db)

	// Resolve the target
	result, err := resolver.Resolve(target, nil)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	// Set the ref
	if err := refMgr.Set(name, result.ID, result.Kind); err != nil {
		return fmt.Errorf("setting ref: %w", err)
	}

	fmt.Printf("Set ref '%s' -> %s (%s)\n", name, util.BytesToHex(result.ID)[:16]+"...", result.Kind)
	return nil
}

func runRefDel(cmd *cobra.Command, args []string) error {
	name := args[0]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)
	if err := refMgr.Delete(name); err != nil {
		return fmt.Errorf("deleting ref: %w", err)
	}

	fmt.Printf("Deleted ref '%s'\n", name)
	return nil
}

func runTagList(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)
	refs, err := refMgr.List(nil)
	if err != nil {
		return fmt.Errorf("listing refs: %w", err)
	}

	var tags []*ref.Ref
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "tag.") {
			tags = append(tags, r)
		}
	}
	if len(tags) == 0 {
		fmt.Println("No tags found.")
		return nil
	}

	fmt.Printf("%-30s  %-12s  %s\n", "NAME", "KIND", "TARGET")
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range tags {
		fmt.Printf("%-30s  %-12s  %s\n", strings.TrimPrefix(r.Name, "tag."), r.TargetKind, util.BytesToHex(r.TargetID)[:16]+"...")
	}
	return nil
}

func runTagCreate(cmd *cobra.Command, args []string) error {
	name := normalizeTagName(args[0])
	target := args[1]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	resolver := ref.NewResolver(db)
	refMgr := ref.NewRefManager(db)
	kind := ref.KindSnapshot

	result, err := resolver.Resolve(target, &kind)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	if err := refMgr.Set(name, result.ID, result.Kind); err != nil {
		return fmt.Errorf("setting tag: %w", err)
	}

	fmt.Printf("Set tag '%s' -> %s (%s)\n", strings.TrimPrefix(name, "tag."), util.BytesToHex(result.ID)[:16]+"...", result.Kind)
	return nil
}

func runTagDelete(cmd *cobra.Command, args []string) error {
	name := normalizeTagName(args[0])

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)
	if err := refMgr.Delete(name); err != nil {
		return fmt.Errorf("deleting tag: %w", err)
	}

	fmt.Printf("Deleted tag '%s'\n", strings.TrimPrefix(name, "tag."))
	return nil
}

func normalizeTagName(name string) string {
	if strings.HasPrefix(name, "tag.") {
		return name
	}
	return "tag." + name
}

func runPick(cmd *cobra.Command, args []string) error {
	kindArg := args[0]

	// Normalize kind
	var kind ref.Kind
	switch strings.ToLower(kindArg) {
	case "snapshot", "snap":
		kind = ref.KindSnapshot
	case "changeset", "cs":
		kind = ref.KindChangeSet
	case "workspace", "ws":
		kind = ref.KindWorkspace
	default:
		return fmt.Errorf("unknown kind: %s (use Snapshot, ChangeSet, or Workspace)", kindArg)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	nodes, err := db.GetNodesByKind(graph.NodeKind(kind))
	if err != nil {
		return fmt.Errorf("getting nodes: %w", err)
	}

	// Filter if requested
	var filtered []*graph.Node
	for _, node := range nodes {
		if pickFilter == "" {
			filtered = append(filtered, node)
			continue
		}

		// Check if filter matches ID or payload
		idHex := util.BytesToHex(node.ID)
		if strings.Contains(idHex, pickFilter) {
			filtered = append(filtered, node)
			continue
		}

		// Check payload fields
		for _, v := range node.Payload {
			if str, ok := v.(string); ok && strings.Contains(str, pickFilter) {
				filtered = append(filtered, node)
				break
			}
		}
	}

	if len(filtered) == 0 {
		fmt.Println("No matches found.")
		return nil
	}

	// Sort by created_at descending
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	// Output matches
	fmt.Printf("%-4s  %-16s  %s\n", "#", "ID", "INFO")
	fmt.Println(strings.Repeat("-", 60))
	for i, node := range filtered {
		idHex := util.BytesToHex(node.ID)[:16]

		var info string
		switch kind {
		case ref.KindSnapshot:
			sourceRef, _ := node.Payload["sourceRef"].(string)
			sourceType, _ := node.Payload["sourceType"].(string)
			if sourceRef == "" {
				sourceRef, _ = node.Payload["gitRef"].(string)
				sourceType = "git"
			}
			info = fmt.Sprintf("%s (%s)", sourceRef, sourceType)
		case ref.KindChangeSet:
			intent, _ := node.Payload["intent"].(string)
			if intent == "" {
				intent = "(no intent)"
			}
			info = intent
		case ref.KindWorkspace:
			name, _ := node.Payload["name"].(string)
			status, _ := node.Payload["status"].(string)
			info = fmt.Sprintf("%s [%s]", name, status)
		}

		fmt.Printf("%-4d  %s...  %s\n", i+1, idHex, info)
	}

	if pickNoUI {
		return nil
	}

	// Output the first match's full ID for scripting
	fmt.Printf("\nFirst match: %s\n", util.BytesToHex(filtered[0].ID))
	return nil
}

func runCherryPick(cmd *cobra.Command, args []string) error {
	csSelector := args[0]
	targetSelector := args[1]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	csID, err := resolveChangeSetID(db, csSelector)
	if err != nil {
		return fmt.Errorf("resolving changeset: %w", err)
	}
	targetID, err := resolveSnapshotID(db, targetSelector)
	if err != nil {
		return fmt.Errorf("resolving target snapshot: %w", err)
	}

	mgr := workspace.NewManager(db)
	result, err := mgr.CherryPick(csID, targetID)
	if err != nil {
		return err
	}
	if len(result.Conflicts) > 0 {
		fmt.Printf("Conflicts detected (%d):\n", len(result.Conflicts))
		for _, c := range result.Conflicts {
			fmt.Printf("  %s: %s\n", c.Path, c.Description)
		}
		return fmt.Errorf("resolve conflicts before continuing")
	}

	fmt.Printf("Cherry-pick created changeset %s\n", util.BytesToHex(result.ResultChangeSet)[:12])
	fmt.Printf("New snapshot: %s\n", util.BytesToHex(result.ResultSnapshot)[:12])
	return nil
}

func runRebase(cmd *cobra.Command, args []string) error {
	targetSelector := args[0]
	changeSetSelectors := args[1:]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	targetID, err := resolveSnapshotID(db, targetSelector)
	if err != nil {
		return fmt.Errorf("resolving target snapshot: %w", err)
	}

	changeSetIDs, err := workspace.ResolveChangesetChain(db, changeSetSelectors)
	if err != nil {
		return fmt.Errorf("resolving changesets: %w", err)
	}

	mgr := workspace.NewManager(db)
	result, err := mgr.Rebase(changeSetIDs, targetID)
	if err != nil {
		return err
	}
	if len(result.Conflicts) > 0 {
		fmt.Printf("Conflicts detected after %d changesets:\n", len(result.Applied))
		for _, c := range result.Conflicts {
			fmt.Printf("  %s: %s\n", c.Path, c.Description)
		}
		return fmt.Errorf("resolve conflicts before continuing")
	}

	fmt.Printf("Rebase complete. New snapshot: %s\n", util.BytesToHex(result.ResultSnapshot)[:12])
	return nil
}

type bisectRef struct {
	Name      string `json:"name"`
	TargetHex string `json:"target"`
	UpdatedAt int64  `json:"updatedAt"`
}

type bisectState struct {
	Refs    []bisectRef `json:"refs"`
	Low     int         `json:"low"`
	High    int         `json:"high"`
	Current int         `json:"current"`
}

func runBisectStart(cmd *cobra.Command, args []string) error {
	goodSel := args[0]
	badSel := args[1]

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	goodID, err := resolveSnapshotID(db, goodSel)
	if err != nil {
		return fmt.Errorf("resolving good snapshot: %w", err)
	}
	badID, err := resolveSnapshotID(db, badSel)
	if err != nil {
		return fmt.Errorf("resolving bad snapshot: %w", err)
	}

	refs, err := listSnapshotRefs(db)
	if err != nil {
		return err
	}
	if len(refs) < 2 {
		return fmt.Errorf("need at least two snapshots to bisect")
	}

	goodIdx := indexOfRef(refs, util.BytesToHex(goodID))
	badIdx := indexOfRef(refs, util.BytesToHex(badID))
	if goodIdx < 0 || badIdx < 0 {
		return fmt.Errorf("good/bad snapshot not found in ref list (use snap.* refs)")
	}
	low := goodIdx
	high := badIdx
	if low > high {
		low, high = high, low
	}
	if high-low < 1 {
		return fmt.Errorf("good and bad snapshots are identical")
	}
	// Route the first step through advanceBisect so the convergence
	// check (High-Low <= 1 → the bad boundary IS the first bad snapshot)
	// runs here too. Without it, an adjacent good/bad pair set
	// current = low + (high-low)/2 = low — the known-GOOD boundary — so
	// `next` proposed testing the good snapshot and `good` rejected it
	// ("already at or below good boundary"), and bisect could never
	// announce the culprit.
	return advanceBisect(bisectState{
		Refs: refs,
		Low:  low,
		High: high,
	})
}

func runBisectGood(cmd *cobra.Command, args []string) error {
	state, err := loadBisectState()
	if err != nil {
		return err
	}
	if state.Current <= state.Low {
		return fmt.Errorf("already at or below good boundary")
	}
	state.Low = state.Current
	return advanceBisect(state)
}

func runBisectBad(cmd *cobra.Command, args []string) error {
	state, err := loadBisectState()
	if err != nil {
		return err
	}
	if state.Current >= state.High {
		return fmt.Errorf("already at or above bad boundary")
	}
	state.High = state.Current
	return advanceBisect(state)
}

func runBisectSkip(cmd *cobra.Command, args []string) error {
	state, err := loadBisectState()
	if err != nil {
		return err
	}
	if state.Current < state.High {
		state.Low = state.Current + 1
	} else if state.Current > state.Low {
		state.High = state.Current - 1
	}
	return advanceBisect(state)
}

func runBisectNext(cmd *cobra.Command, args []string) error {
	state, err := loadBisectState()
	if err != nil {
		return err
	}
	// advanceBisect (not printBisectCurrent) so a converged session
	// (High-Low <= 1) re-announces the first bad snapshot instead of
	// proposing a stale test candidate. Idempotent otherwise: it
	// recomputes the same midpoint good/bad already saved.
	return advanceBisect(state)
}

func runBisectReset(cmd *cobra.Command, args []string) error {
	if err := os.Remove(bisectStatePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Bisect state cleared.")
	return nil
}

func advanceBisect(state bisectState) error {
	if state.High-state.Low <= 1 {
		bad := state.Refs[state.High]
		fmt.Printf("First bad snapshot: %s (%s)\n", bad.Name, bad.TargetHex[:12])
		return saveBisectState(state)
	}
	state.Current = state.Low + (state.High-state.Low)/2
	if err := saveBisectState(state); err != nil {
		return err
	}
	return printBisectCurrent(state)
}

func printBisectCurrent(state bisectState) error {
	if state.Current < 0 || state.Current >= len(state.Refs) {
		return fmt.Errorf("bisect state out of range")
	}
	ref := state.Refs[state.Current]
	fmt.Printf("Test snapshot: %s (%s)\n", ref.Name, ref.TargetHex[:12])
	return nil
}

func listSnapshotRefs(db *graph.DB) ([]bisectRef, error) {
	refMgr := ref.NewRefManager(db)
	kind := ref.KindSnapshot
	refs, err := refMgr.List(&kind)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []bisectRef
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "snap.") {
			continue
		}
		hexID := util.BytesToHex(r.TargetID)
		if seen[hexID] {
			continue
		}
		seen[hexID] = true
		out = append(out, bisectRef{
			Name:      r.Name,
			TargetHex: hexID,
			UpdatedAt: r.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt < out[j].UpdatedAt
	})
	return out, nil
}

func indexOfRef(refs []bisectRef, targetHex string) int {
	for i, r := range refs {
		if r.TargetHex == targetHex {
			return i
		}
	}
	return -1
}

func bisectStatePath() string {
	return filepath.Join(kaiDir, "bisect.json")
}

func loadBisectState() (bisectState, error) {
	var state bisectState
	data, err := os.ReadFile(bisectStatePath())
	if err != nil {
		return state, fmt.Errorf("read bisect state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parse bisect state: %w", err)
	}
	return state, nil
}

func saveBisectState(state bisectState) error {
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(bisectStatePath(), data, 0644)
}

func runShadowImport(cmd *cobra.Command, args []string) error {
	baseRef, headRef, err := parseGitRange(shadowGitRange)
	if err != nil {
		return err
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	fmt.Printf("Importing git range %s..%s\n", baseRef, headRef)
	baseSnap, err := createSnapshotFromGitRef(db, shadowGitRepo, baseRef)
	if err != nil {
		return fmt.Errorf("base snapshot: %w", err)
	}
	headSnap, err := createSnapshotFromGitRef(db, shadowGitRepo, headRef)
	if err != nil {
		return fmt.Errorf("head snapshot: %w", err)
	}

	changeSetID, err := createChangeSetFromSnapshots(db, baseSnap, headSnap, fmt.Sprintf("git:%s..%s", baseRef, headRef))
	if err != nil {
		return fmt.Errorf("changeset: %w", err)
	}

	if shadowUpdateRef != "" {
		refMgr := ref.NewRefManager(db)
		if err := refMgr.Set(shadowUpdateRef, headSnap, ref.KindSnapshot); err != nil {
			return fmt.Errorf("updating ref %s: %w", shadowUpdateRef, err)
		}
	}

	fmt.Printf("Imported changeset %s\n", util.BytesToHex(changeSetID)[:12])
	return nil
}

func runShadowParity(cmd *cobra.Command, args []string) error {
	baseRef, headRef, err := parseGitRange(shadowGitRange)
	if err != nil {
		return err
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	baseSnap, err := createSnapshotFromGitRef(db, shadowGitRepo, baseRef)
	if err != nil {
		return fmt.Errorf("base snapshot: %w", err)
	}
	headSnap, err := createSnapshotFromGitRef(db, shadowGitRepo, headRef)
	if err != nil {
		return fmt.Errorf("head snapshot: %w", err)
	}

	kaiFiles, err := diffSnapshotFiles(db, baseSnap, headSnap)
	if err != nil {
		return err
	}
	gitFiles, err := gitDiffNames(shadowGitRepo, baseRef, headRef)
	if err != nil {
		return err
	}

	onlyGit, onlyKai := diffSets(gitFiles, kaiFiles)
	fmt.Printf("Git changed files: %d\n", len(gitFiles))
	fmt.Printf("Kai changed files: %d\n", len(kaiFiles))
	if len(onlyGit) > 0 {
		fmt.Printf("Only in Git: %d\n", len(onlyGit))
	}
	if len(onlyKai) > 0 {
		fmt.Printf("Only in Kai: %d\n", len(onlyKai))
	}
	return nil
}

func runShadowDrift(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	gitSnap, err := createSnapshotFromGitRef(db, shadowGitRepo, shadowGitRef)
	if err != nil {
		return fmt.Errorf("git snapshot: %w", err)
	}
	snapID, err := resolveSnapshotID(db, shadowSnapRef)
	if err != nil {
		return fmt.Errorf("snapshot ref: %w", err)
	}

	if bytes.Equal(gitSnap, snapID) {
		fmt.Println("No drift detected.")
		return nil
	}
	fmt.Printf("Drift detected: git=%s ref=%s\n", util.BytesToHex(gitSnap)[:12], util.BytesToHex(snapID)[:12])
	return nil
}

func parseGitRange(input string) (string, string, error) {
	parts := strings.Split(input, "..")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid git range (use BASE..HEAD)")
	}
	return parts[0], parts[1], nil
}

func createChangeSetFromSnapshots(db *graph.DB, baseSnap, headSnap []byte, source string) ([]byte, error) {
	tx, err := db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	payload := map[string]interface{}{
		"base":        util.BytesToHex(baseSnap),
		"head":        util.BytesToHex(headSnap),
		"title":       "",
		"description": fmt.Sprintf("shadow import %s", source),
		"intent":      "",
		"createdAt":   util.NowMs(),
	}
	changeSetID, err := db.InsertNode(tx, graph.KindChangeSet, payload)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	autoRefMgr := ref.NewAutoRefManager(db)
	if err := autoRefMgr.OnChangeSetCreated(changeSetID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update refs: %v\n", err)
	}
	return changeSetID, nil
}

func diffSnapshotFiles(db *graph.DB, baseSnap, headSnap []byte) (map[string]bool, error) {
	mgr := workspace.NewManager(db)
	baseFiles, err := mgr.GetSnapshotFileMap(baseSnap)
	if err != nil {
		return nil, err
	}
	headFiles, err := mgr.GetSnapshotFileMap(headSnap)
	if err != nil {
		return nil, err
	}

	changed := make(map[string]bool)
	for path, headDigest := range headFiles {
		baseDigest, exists := baseFiles[path]
		if !exists || baseDigest != headDigest {
			changed[path] = true
		}
	}
	for path := range baseFiles {
		if _, exists := headFiles[path]; !exists {
			changed[path] = true
		}
	}
	return changed, nil
}

func gitDiffNames(repoPath, baseRef, headRef string) (map[string]bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "diff", "--name-only", fmt.Sprintf("%s..%s", baseRef, headRef))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	changed := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		changed[line] = true
	}
	return changed, nil
}

func diffSets(a, b map[string]bool) ([]string, []string) {
	var onlyA []string
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	var onlyB []string
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}

// resolveID resolves a user-provided ID string to a full ID bytes.
// It supports full hex IDs, short prefixes, refs, selectors, and slugs.
func resolveID(db *graph.DB, input string, wantKind *ref.Kind) ([]byte, error) {
	resolver := ref.NewResolver(db)
	result, err := resolver.Resolve(input, wantKind)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("not found: %s", input)
	}
	return result.ID, nil
}

// resolveSnapshotID is a convenience wrapper for resolving snapshot IDs.
func resolveSnapshotID(db *graph.DB, input string) ([]byte, error) {
	kind := ref.KindSnapshot
	return resolveID(db, input, &kind)
}

// resolveChangeSetID is a convenience wrapper for resolving changeset IDs.
func resolveChangeSetID(db *graph.DB, input string) ([]byte, error) {
	kind := ref.KindChangeSet
	return resolveID(db, input, &kind)
}

func runCompletion(cmd *cobra.Command, args []string) error {
	switch args[0] {
	case "bash":
		return cmd.Root().GenBashCompletion(os.Stdout)
	case "zsh":
		return cmd.Root().GenZshCompletion(os.Stdout)
	case "fish":
		return cmd.Root().GenFishCompletion(os.Stdout, true)
	case "powershell":
		return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
	default:
		return fmt.Errorf("unknown shell: %s", args[0])
	}
}

// completeSnapshotID provides shell completion for snapshot IDs.
func completeSnapshotID(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeNodeID(ref.KindSnapshot, toComplete)
}

// completeChangeSetID provides shell completion for changeset IDs.
func completeChangeSetID(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeNodeID(ref.KindChangeSet, toComplete)
}

// completeNodeID provides shell completion for node IDs of a specific kind.
func completeNodeID(kind ref.Kind, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Always include selectors
	selectors := []string{}
	switch kind {
	case ref.KindSnapshot:
		selectors = []string{"@snap:last", "@snap:prev"}
	case ref.KindChangeSet:
		selectors = []string{"@cs:last", "@cs:prev"}
	}

	// Try to open DB for refs
	db, err := openDB()
	if err != nil {
		return selectors, cobra.ShellCompDirectiveNoFileComp
	}
	defer db.Close()

	var completions []string
	completions = append(completions, selectors...)

	// Add matching refs
	refMgr := ref.NewRefManager(db)
	refs, err := refMgr.List(&kind)
	if err == nil {
		for _, r := range refs {
			if strings.HasPrefix(r.Name, toComplete) || toComplete == "" {
				completions = append(completions, r.Name)
			}
		}
	}

	// Add matching short IDs (up to 10)
	if len(toComplete) >= 3 {
		nodes, err := db.GetNodesByKind(graph.NodeKind(kind))
		if err == nil {
			count := 0
			for _, n := range nodes {
				if count >= 10 {
					break
				}
				idHex := util.BytesToHex(n.ID)
				if strings.HasPrefix(idHex, toComplete) {
					// Show short ID with description
					completions = append(completions, idHex[:12])
					count++
				}
			}
		}
	}

	// Add recent nodes if no specific prefix
	if toComplete == "" {
		nodes, err := db.GetNodesByKind(graph.NodeKind(kind))
		if err == nil {
			sort.Slice(nodes, func(i, j int) bool {
				return nodes[i].CreatedAt > nodes[j].CreatedAt
			})
			for i, n := range nodes {
				if i >= 5 {
					break
				}
				idHex := util.BytesToHex(n.ID)[:12]
				completions = append(completions, idHex)
			}
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// completeRefName provides shell completion for ref names.
func completeRefName(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	db, err := openDB()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer db.Close()

	refMgr := ref.NewRefManager(db)
	refs, err := refMgr.List(nil)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, r := range refs {
		if strings.HasPrefix(r.Name, toComplete) || toComplete == "" {
			completions = append(completions, r.Name)
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// Remote command implementations

func runRemoteSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	rawURL := args[1]

	tenant := remoteTenant
	repo := remoteRepo

	// Parse URL to extract tenant/repo from path if not specified via flags
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Extract tenant/repo from path if flags are at default values
	pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(pathParts) >= 2 && tenant == "default" && repo == "main" {
		tenant = pathParts[0]
		repo = pathParts[1]
		// Rebuild base URL without tenant/repo path
		parsedURL.Path = ""
		rawURL = parsedURL.String()
	}

	entry := &remote.RemoteEntry{
		URL:    rawURL,
		Tenant: tenant,
		Repo:   repo,
	}

	if err := remote.ForceSetRemote(name, entry); err != nil {
		return fmt.Errorf("setting remote: %w", err)
	}

	fmt.Printf("Remote '%s' set to: %s (tenant=%s, repo=%s)\n", name, rawURL, tenant, repo)
	return nil
}

func runRemoteGet(cmd *cobra.Command, args []string) error {
	name := args[0]

	entry, err := remote.GetRemote(name)
	if err != nil {
		return err
	}

	fmt.Printf("URL:    %s\n", entry.URL)
	fmt.Printf("Tenant: %s\n", entry.Tenant)
	fmt.Printf("Repo:   %s\n", entry.Repo)
	return nil
}

func runRemoteList(cmd *cobra.Command, args []string) error {
	remotes, err := remote.ListRemotes()
	if err != nil {
		return fmt.Errorf("loading remotes: %w", err)
	}

	if len(remotes) == 0 {
		fmt.Println("No remotes configured.")
		fmt.Println("Use 'kai remote set <name> <url>' to add a remote.")
		return nil
	}

	fmt.Printf("%-15s  %-12s  %-12s  %s\n", "NAME", "TENANT", "REPO", "URL")
	fmt.Println(strings.Repeat("-", 80))
	for name, entry := range remotes {
		fmt.Printf("%-15s  %-12s  %-12s  %s\n", name, entry.Tenant, entry.Repo, entry.URL)
	}

	return nil
}

func runRemoteDel(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := remote.DeleteRemote(name); err != nil {
		return fmt.Errorf("deleting remote: %w", err)
	}

	fmt.Printf("Deleted remote '%s'\n", name)
	return nil
}

// ── kai org list / delete ──────────────────────────────────────────

// newControlClientAuthed returns a ControlClient pointed at KAI_SERVER
// (default kaicontext.com) with the current user's access token wired
// in. Returns a friendly error if the user is not logged in.
func newControlClientAuthed() (*remote.ControlClient, error) {
	serverURL := os.Getenv("KAI_SERVER")
	if serverURL == "" {
		serverURL = remote.DefaultServer
	}
	token, err := remote.GetValidAccessToken()
	if err != nil || token == "" {
		return nil, fmt.Errorf("not logged in — run 'kai auth login' first")
	}
	c := remote.NewControlClient(serverURL)
	c.AuthToken = token
	return c, nil
}

func runOrgList(cmd *cobra.Command, args []string) error {
	c, err := newControlClientAuthed()
	if err != nil {
		return err
	}
	orgs, err := c.ListOrgs()
	if err != nil {
		return fmt.Errorf("listing orgs: %w", err)
	}
	if len(orgs) == 0 {
		fmt.Println("You don't belong to any organizations.")
		return nil
	}
	for _, o := range orgs {
		fmt.Printf("  %-20s  %s\n", o.Slug, o.Name)
	}
	return nil
}

func runOrgDelete(cmd *cobra.Command, args []string) error {
	slug := args[0]
	c, err := newControlClientAuthed()
	if err != nil {
		return err
	}

	if !orgDeleteYes {
		// List repos first so the user sees the blast radius.
		repos, err := c.ListRepos(slug)
		if err != nil {
			// A 403/404 here usually means the user isn't a member;
			// we still want to try the delete for the owner-only case,
			// so just warn and continue to the confirmation prompt.
			fmt.Fprintf(os.Stderr, "Note: couldn't list repos (%v); proceeding.\n\n", err)
		} else {
			fmt.Printf("About to delete organization %q and %d repo(s):\n", slug, len(repos))
			for _, r := range repos {
				fmt.Printf("  - %s/%s\n", slug, r.Name)
			}
			fmt.Println()
			fmt.Println("This is irreversible. Every snapshot, ref, CI run, webhook,")
			fmt.Println("secret, variable, and membership will be permanently removed.")
			fmt.Println()
		}
		fmt.Printf("Type the org slug (%s) to confirm: ", slug)
		reader := bufio.NewReader(os.Stdin)
		typed, _ := reader.ReadString('\n')
		typed = strings.TrimSpace(typed)
		if typed != slug {
			return fmt.Errorf("confirmation did not match; org NOT deleted")
		}
	}

	reposDeleted, err := c.DeleteOrg(slug)
	if err != nil {
		return fmt.Errorf("deleting org: %w", err)
	}
	fmt.Printf("✓ Deleted organization %q (%d repo(s))\n", slug, reposDeleted)
	return nil
}

// interactivePushOnboarding handles the case when a user tries to push without a remote configured.
// It walks them through authentication, org selection, repo creation, and remote setup.
func interactivePushOnboarding(remoteName string) (*remote.Client, error) {
	reader := bufio.NewReader(os.Stdin)

	// Determine server URL
	serverURL := os.Getenv("KAI_SERVER")
	if serverURL == "" {
		serverURL = remote.DefaultServer
	}

	// Detect project name
	projectName := remote.DetectProjectName()
	fmt.Printf("Detected project: %s\n", projectName)
	fmt.Printf("Server: %s\n\n", serverURL)

	// Check if user is authenticated with a valid token
	token, err := remote.GetValidAccessToken()
	if err != nil || token == "" {
		fmt.Println("You're not logged in. Let's set that up first.")
		fmt.Println()

		fmt.Print("Would you like to sign in or sign up? [Y/n]: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "n" || input == "no" {
			return nil, fmt.Errorf("push requires authentication. Run 'kai auth login' when ready")
		}

		// Run the login flow
		if err := remote.Login(serverURL); err != nil {
			return nil, fmt.Errorf("login failed: %w", err)
		}
		fmt.Println()
	} else {
		email, _, _ := remote.GetAuthStatus()
		fmt.Printf("Logged in as: %s\n", email)
	}

	// Create control client to manage orgs/repos
	ctrl := remote.NewControlClient(serverURL)

	// List user's orgs
	orgs, err := ctrl.ListOrgs()
	if err != nil {
		return nil, fmt.Errorf("listing organizations: %w", err)
	}

	var selectedOrg *remote.OrgInfo

	if len(orgs) == 0 {
		// No orgs - offer to create one
		fmt.Println("You don't have any organizations yet.")
		fmt.Println()

		// Use email username as default org name
		creds, _ := remote.LoadCredentials()
		defaultSlug := "my-org"
		if creds != nil && creds.Email != "" {
			if idx := strings.Index(creds.Email, "@"); idx > 0 {
				defaultSlug = remote.DetectProjectName() // Reuse sanitizer
				if slug := creds.Email[:idx]; len(slug) > 0 {
					defaultSlug = strings.ToLower(strings.ReplaceAll(slug, ".", "-"))
				}
			}
		}

		fmt.Printf("Organization slug [%s]: ", defaultSlug)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "" {
			input = defaultSlug
		}
		orgSlug := input

		fmt.Printf("Organization name [%s]: ", orgSlug)
		input, _ = reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "" {
			input = orgSlug
		}
		orgName := input

		fmt.Printf("Creating organization '%s'...\n", orgSlug)
		newOrg, err := ctrl.CreateOrg(orgSlug, orgName)
		if err != nil {
			return nil, fmt.Errorf("creating organization: %w", err)
		}
		selectedOrg = newOrg
		fmt.Printf("Created organization: %s (id: %s)\n\n", selectedOrg.Slug, selectedOrg.ID)
	} else if len(orgs) == 1 {
		// Single org - use it
		selectedOrg = &orgs[0]
		fmt.Printf("Using organization: %s\n", selectedOrg.Slug)
	} else {
		// Multiple orgs - let user choose
		fmt.Println("Select an organization:")
		for i, org := range orgs {
			fmt.Printf("  [%d] %s (%s)\n", i+1, org.Slug, org.Name)
		}
		fmt.Print("Enter number: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > len(orgs) {
			return nil, fmt.Errorf("invalid selection")
		}
		selectedOrg = &orgs[idx-1]
		fmt.Printf("Using organization: %s\n", selectedOrg.Slug)
	}

	// Verify we have a valid org
	if selectedOrg == nil || selectedOrg.Slug == "" {
		return nil, fmt.Errorf("no organization selected (this is a bug)")
	}

	// Create the repository
	fmt.Println()
	fmt.Printf("Repository name [%s]: ", projectName)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		input = projectName
	}
	repoName := input

	fmt.Print("Visibility (private/public) [private]: ")
	input, _ = reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))
	visibility := "private"
	if input == "public" {
		visibility = "public"
	}

	fmt.Printf("Creating repository '%s/%s'...\n", selectedOrg.Slug, repoName)
	fmt.Printf("  API: POST %s/api/v1/orgs/%s/repos\n", serverURL, selectedOrg.Slug)
	_, err = ctrl.CreateRepo(selectedOrg.Slug, repoName, visibility)
	if err != nil {
		// Check if repo already exists
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "409") {
			fmt.Printf("Repository already exists, using existing.\n")
		} else {
			return nil, fmt.Errorf("creating repository: %w", err)
		}
	} else {
		fmt.Printf("Created repository: %s/%s\n", selectedOrg.Slug, repoName)
	}

	// Set up the remote
	remoteEntry := &remote.RemoteEntry{
		URL:    serverURL,
		Tenant: selectedOrg.Slug,
		Repo:   repoName,
	}
	if err := remote.SetRemote(remoteName, remoteEntry); err != nil {
		return nil, fmt.Errorf("saving remote: %w", err)
	}
	fmt.Printf("\nRemote '%s' configured: %s/%s\n\n", remoteName, selectedOrg.Slug, repoName)

	// Create and return the client
	return remote.NewClient(serverURL, selectedOrg.Slug, repoName), nil
}

// isNonFastForwardPush reports whether advancing a ref to newTarget would
// overwrite a remote head that we never synced — i.e. the push is not a
// fast-forward and would silently clobber someone else's snapshot (F-13).
//
//   - remoteTarget  — the ref's current value on the remote (nil/empty if absent)
//   - trackedTarget — the value we last pulled/pushed (the remote/origin/* ref)
//   - newTarget     — the value we are about to push
//
// Creating a ref that doesn't exist remotely, or re-pushing the value already
// there, is always fine. Otherwise the push is rejected unless the remote head
// is exactly what we last synced (a true fast-forward from our base).
func isNonFastForwardPush(remoteTarget, trackedTarget, newTarget []byte) bool {
	if len(remoteTarget) == 0 {
		return false // not on the remote yet — creating it is fine
	}
	if bytes.Equal(remoteTarget, newTarget) {
		return false // remote already at our value — nothing to clobber
	}
	// The remote advanced past (or diverged from) what we last synced.
	return len(trackedTarget) == 0 || !bytes.Equal(trackedTarget, remoteTarget)
}

// workspaceRefSet builds the refs that publish a workspace to a remote so that
// `kai fetch --ws <name>` can reconstruct it. The ws.<name> node ref (target =
// the workspace's content-addressed digest) is the name fetch --ws looks up;
// ws.<name>.base / ws.<name>.head carry the snapshot bounds; and one
// ws.<name>.cs.<id> ref is emitted per open changeset. Keeping this contract in
// one place is what makes a workspace shareable (F-12).
func workspaceRefSet(name string, wsContentDigest, base, head []byte, changesets [][]byte) []*ref.Ref {
	refs := []*ref.Ref{
		{Name: fmt.Sprintf("ws.%s", name), TargetID: wsContentDigest, TargetKind: ref.KindWorkspace},
		{Name: fmt.Sprintf("ws.%s.base", name), TargetID: base, TargetKind: ref.KindSnapshot},
		{Name: fmt.Sprintf("ws.%s.head", name), TargetID: head, TargetKind: ref.KindSnapshot},
	}
	for _, csID := range changesets {
		refs = append(refs, &ref.Ref{
			Name:     fmt.Sprintf("ws.%s.cs.%s", name, hex.EncodeToString(csID)[:8]),
			TargetID: csID,
		})
	}
	return refs
}

// isFastForwardPull reports whether pulling remoteTarget over our localTarget is
// a clean fast-forward — i.e. everything we hold locally was already synced from
// the remote (local == the remote/origin/* tracking ref), so the remote has
// merely advanced and we hold no unpushed local snapshots that pulling would
// orphan (F-9). The already-up-to-date case (local == remote) is handled by the
// caller before this is called.
//
//   - localTarget   — our current snap.latest
//   - trackedTarget — the value we last pulled/pushed (the remote/origin/* ref)
//   - remoteTarget  — the ref's current value on the remote
//
// Like isNonFastForwardPush this is a tracking-ref heuristic, not true ancestry
// (snapshots carry no parent link — see F-14). It is exact for the common case:
// a behind clone whose head is still what it last synced is always a safe
// fast-forward, while a head that has moved past the tracking ref is treated as
// divergence and left for the user to reconcile.
func isFastForwardPull(localTarget, trackedTarget, remoteTarget []byte) bool {
	if len(localTarget) == 0 {
		return true // nothing local — any remote head is a fast-forward
	}
	if len(trackedTarget) == 0 {
		return false // never synced — can't prove our local isn't unpushed work
	}
	// Fast-forward iff our local head is exactly what we last synced; anything
	// else means we have local snapshots beyond the remote's prior state.
	return bytes.Equal(localTarget, trackedTarget)
}

// onDiskContentMatches reports whether the working-tree file at path already
// holds exactly the target content (its blake3 hex == contentDigest). Pull uses
// it to decide it can skip fetching/writing a blob when the remote can't supply
// it: if the file on disk is already the right content there is nothing to
// materialize. A missing or unreadable file returns false. (F-10/F-6)
func onDiskContentMatches(path, contentDigest string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return util.Blake3HashHex(data) == contentDigest
}

// materializeWorkingTree writes the files of snapshot `head` into worktreeRoot,
// overwriting only files whose on-disk content differs from the snapshot and
// never deleting (clean=false). It is the post-pull step that keeps the working
// tree in sync with snap.latest (F-10): without it, pull advanced the graph but
// left the files stale. It is a no-op when the tree is already current. Returns
// the number of files written.
func materializeWorkingTree(db *graph.DB, head []byte, worktreeRoot string) (int, error) {
	result, err := snapshot.NewCreator(db, nil).Checkout(head, worktreeRoot, false)
	if err != nil {
		return 0, err
	}
	return result.FilesWritten, nil
}

func runPush(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("push")
	defer te.Finish()

	// Show explain if requested
	if pushExplain {
		remoteName := "origin"
		refSpec := "*"
		if len(args) > 0 {
			remoteName = args[0]
		}
		if len(args) > 1 {
			refSpec = args[1]
		}
		ctx := explain.ExplainPush(remoteName, refSpec, 0) // 0 refs - count determined later
		ctx.Print(os.Stdout)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Determine remote name and targets
	remoteName := "origin"
	targets := []string{}

	if len(args) > 0 {
		// Check if first arg is a remote name
		if _, err := remote.GetRemote(args[0]); err == nil {
			remoteName = args[0]
			targets = args[1:]
		} else {
			// First arg is a target
			targets = args
		}
	}

	// Create client for remote
	client, err := remote.NewClientForRemote(remoteName)
	if err != nil {
		return fmt.Errorf("remote '%s' not configured (use 'kai remote set %s <url>')", remoteName, remoteName)
	}

	// Check if this is a default/unconfigured remote (tenant="default" means GetRemote fell back to defaults)
	if remoteName == "origin" && client.Tenant == "default" {
		// Offer interactive onboarding
		fmt.Println("No remote repository configured.")
		fmt.Println()
		fmt.Print("Would you like to set one up now? [Y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "n" || input == "no" {
			return fmt.Errorf("remote '%s' not configured (use 'kai remote set %s <url>')", remoteName, remoteName)
		}
		fmt.Println()

		client, err = interactivePushOnboarding(remoteName)
		if err != nil {
			return err
		}
	}

	// Get push message: prefer kai capture -m, fall back to git commit message
	msgPath := filepath.Join(kaiDir, "message")
	if msgBytes, err := os.ReadFile(msgPath); err == nil && len(msgBytes) > 0 {
		client.Message = strings.TrimSpace(string(msgBytes))
		os.Remove(msgPath) // consumed
	} else if gitMsg, err := exec.Command("git", "log", "-1", "--format=%s").Output(); err == nil {
		client.Message = strings.TrimSpace(string(gitMsg))
	}

	// Check server health
	debugf("push: connecting to %s", client.BaseURL)
	if err := client.Health(); err != nil {
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	// Get refs to push
	refMgr := ref.NewRefManager(db)
	wsMgr := workspace.NewManager(db)
	reviewMgr := review.NewManager(db)
	var refsToSync []*ref.Ref
	var workspaceToPush *workspace.Workspace
	var reviewsToPush []*review.Review

	if pushAll {
		// Legacy: push all refs
		refsToSync, err = refMgr.List(nil)
		if err != nil {
			return fmt.Errorf("listing refs: %w", err)
		}
	} else if pushWorkspace != "" {
		// Push specific workspace
		workspaceToPush, err = wsMgr.Get(pushWorkspace)
		if err != nil {
			return fmt.Errorf("getting workspace: %w", err)
		}
		if workspaceToPush == nil {
			return fmt.Errorf("workspace not found: %s", pushWorkspace)
		}
	} else if len(targets) > 0 {
		// Parse targets with prefixes
		for _, target := range targets {
			if strings.HasPrefix(target, "cs:") {
				// Changeset target
				csRef := strings.TrimPrefix(target, "cs:")
				r, err := refMgr.Get("cs." + csRef)
				if err != nil {
					return fmt.Errorf("getting changeset ref 'cs.%s': %w", csRef, err)
				}
				if r == nil {
					// Try without prefix
					r, err = refMgr.Get(csRef)
					if err != nil {
						return fmt.Errorf("getting changeset ref '%s': %w", csRef, err)
					}
				}
				if r != nil {
					refsToSync = append(refsToSync, r)
				}
			} else if strings.HasPrefix(target, "snap:") {
				// Snapshot target
				snapRef := strings.TrimPrefix(target, "snap:")
				r, err := refMgr.Get("snap." + snapRef)
				if err != nil {
					return fmt.Errorf("getting snapshot ref 'snap.%s': %w", snapRef, err)
				}
				if r == nil {
					// Try without prefix
					r, err = refMgr.Get(snapRef)
					if err != nil {
						return fmt.Errorf("getting snapshot ref '%s': %w", snapRef, err)
					}
				}
				if r != nil {
					refsToSync = append(refsToSync, r)
				}
			} else if strings.HasPrefix(target, "tag:") {
				// Tag target
				tagRef := strings.TrimPrefix(target, "tag:")
				r, err := refMgr.Get("tag." + tagRef)
				if err != nil {
					return fmt.Errorf("getting tag ref 'tag.%s': %w", tagRef, err)
				}
				if r == nil {
					// Try without prefix
					r, err = refMgr.Get(tagRef)
					if err != nil {
						return fmt.Errorf("getting tag ref '%s': %w", tagRef, err)
					}
				}
				if r != nil {
					refsToSync = append(refsToSync, r)
				}
			} else if strings.HasPrefix(target, "review:") {
				// Review target
				reviewRef := strings.TrimPrefix(target, "review:")
				rev, err := reviewMgr.GetByShortID(reviewRef)
				if err != nil {
					return fmt.Errorf("getting review '%s': %w", reviewRef, err)
				}
				reviewsToPush = append(reviewsToPush, rev)
			} else {
				// Legacy: direct ref name
				r, err := refMgr.Get(target)
				if err != nil {
					return fmt.Errorf("getting ref '%s': %w", target, err)
				}
				if r != nil {
					refsToSync = append(refsToSync, r)
				}
			}
		}
	} else {
		// No targets specified - push all snapshots, changesets, and reviews
		allRefs, err := refMgr.List(nil)
		if err != nil {
			return fmt.Errorf("listing refs: %w", err)
		}
		for _, r := range allRefs {
			if strings.HasPrefix(r.Name, "snap.") || strings.HasPrefix(r.Name, "cs.") || strings.HasPrefix(r.Name, "tag.") {
				refsToSync = append(refsToSync, r)
			}
		}

		// Also get all reviews
		allReviews, err := reviewMgr.List()
		if err != nil {
			return fmt.Errorf("listing reviews: %w", err)
		}
		reviewsToPush = allReviews
	}

	debugf("push: %d refs, %d reviews to sync", len(refsToSync), len(reviewsToPush))

	// Track pre-computed pack objects for UUID-based nodes (workspace, review)
	// Key: content-addressed digest hex, Value: pre-built PackObject
	precomputedPackObjects := make(map[string]remote.PackObject)

	// If pushing a workspace, collect its changesets and snapshots
	if workspaceToPush != nil {
		fmt.Printf("Pushing workspace '%s'...\n", workspaceToPush.Name)

		// Get workspace node to compute content-addressed digest
		wsNode, err := db.GetNode(workspaceToPush.ID)
		if err != nil || wsNode == nil {
			return fmt.Errorf("getting workspace node: %w", err)
		}

		// Compute content-addressed digest for workspace
		// Include UUID in payload so it can be reconstructed on fetch
		wsPayload := make(map[string]interface{})
		for k, v := range wsNode.Payload {
			wsPayload[k] = v
		}
		wsPayload["_uuid"] = hex.EncodeToString(workspaceToPush.ID)
		wsPayloadJSON, err := util.CanonicalJSON(wsPayload)
		if err != nil {
			return fmt.Errorf("serializing workspace payload: %w", err)
		}
		wsContent := append([]byte(string(graph.KindWorkspace)+"\n"), wsPayloadJSON...)
		wsContentDigest := util.Blake3Hash(wsContent)

		// Store pre-computed pack object for workspace
		precomputedPackObjects[hex.EncodeToString(wsContentDigest)] = remote.PackObject{
			Digest:  wsContentDigest,
			Kind:    string(graph.KindWorkspace),
			Content: wsContent,
		}

		// Build the refs that publish this workspace so `kai fetch --ws` can
		// reconstruct it: the node ref (ws.<name>), base/head snapshot refs, and
		// one ref per open changeset. Only include changesets whose node exists
		// locally.
		var openCS [][]byte
		for _, csID := range workspaceToPush.OpenChangeSets {
			if csNode, err := db.GetNode(csID); err == nil && csNode != nil {
				openCS = append(openCS, csID)
			}
		}
		refsToSync = append(refsToSync, workspaceRefSet(
			workspaceToPush.Name, wsContentDigest,
			workspaceToPush.BaseSnapshot, workspaceToPush.HeadSnapshot, openCS)...)

		debugf("  Base: %s", hex.EncodeToString(workspaceToPush.BaseSnapshot)[:12])
		debugf("  Head: %s", hex.EncodeToString(workspaceToPush.HeadSnapshot)[:12])
		debugf("  Changesets: %d", len(workspaceToPush.OpenChangeSets))
	}

	// If pushing reviews, collect them and their target changesets
	for _, reviewToPush := range reviewsToPush {
		fmt.Printf("Pushing review '%s'...\n", reviewToPush.Title)

		// Get review node to compute content-addressed digest
		reviewNode, err := db.GetNode(reviewToPush.ID)
		if err != nil || reviewNode == nil {
			return fmt.Errorf("getting review node: %w", err)
		}

		// Compute content-addressed digest for review
		// Include UUID in payload so it can be reconstructed on fetch
		reviewPayload := make(map[string]interface{})
		for k, v := range reviewNode.Payload {
			reviewPayload[k] = v
		}
		reviewPayload["_uuid"] = hex.EncodeToString(reviewToPush.ID)
		reviewPayloadJSON, err := util.CanonicalJSON(reviewPayload)
		if err != nil {
			return fmt.Errorf("serializing review payload: %w", err)
		}
		reviewContent := append([]byte(string(graph.KindReview)+"\n"), reviewPayloadJSON...)
		reviewContentDigest := util.Blake3Hash(reviewContent)

		// Store pre-computed pack object for review
		precomputedPackObjects[hex.EncodeToString(reviewContentDigest)] = remote.PackObject{
			Digest:  reviewContentDigest,
			Kind:    string(graph.KindReview),
			Content: reviewContent,
		}

		// Add review ref
		reviewShortID := hex.EncodeToString(reviewToPush.ID)[:12]
		reviewNodeRef := &ref.Ref{
			Name:       fmt.Sprintf("review.%s", reviewShortID),
			TargetID:   reviewContentDigest,
			TargetKind: ref.KindReview,
		}
		refsToSync = append(refsToSync, reviewNodeRef)

		// Also push the target changeset (if it's a changeset)
		if reviewToPush.TargetKind == graph.KindChangeSet {
			targetRef := &ref.Ref{
				Name:       fmt.Sprintf("review.%s.target", reviewShortID),
				TargetID:   reviewToPush.TargetID,
				TargetKind: ref.KindChangeSet,
			}
			refsToSync = append(refsToSync, targetRef)

			// Also create a top-level cs ref so it shows up in changesets list
			csShortID := hex.EncodeToString(reviewToPush.TargetID)[:12]
			csRef := &ref.Ref{
				Name:       fmt.Sprintf("cs.%s", csShortID),
				TargetID:   reviewToPush.TargetID,
				TargetKind: ref.KindChangeSet,
			}
			refsToSync = append(refsToSync, csRef)

			// Also push the base and head snapshots for the changeset
			// These are stored in the changeset payload as hex strings
			csNode, err := db.GetNode(reviewToPush.TargetID)
			if err == nil && csNode != nil {
				if baseHex, ok := csNode.Payload["base"].(string); ok {
					if baseID, err := hex.DecodeString(baseHex); err == nil {
						snapShortID := baseHex[:12]
						snapRef := &ref.Ref{
							Name:       fmt.Sprintf("snap.%s", snapShortID),
							TargetID:   baseID,
							TargetKind: ref.KindSnapshot,
						}
						refsToSync = append(refsToSync, snapRef)
					}
				}
				if headHex, ok := csNode.Payload["head"].(string); ok {
					if headID, err := hex.DecodeString(headHex); err == nil {
						snapShortID := headHex[:12]
						snapRef := &ref.Ref{
							Name:       fmt.Sprintf("snap.%s", snapShortID),
							TargetID:   headID,
							TargetKind: ref.KindSnapshot,
						}
						refsToSync = append(refsToSync, snapRef)
					}
				}
			}
		}

		debugf("  Review ID: %s", reviewShortID)
		debugf("  State: %s", reviewToPush.State)
		debugf("  Target: %s (%s)", hex.EncodeToString(reviewToPush.TargetID)[:12], reviewToPush.TargetKind)
	}

	if len(refsToSync) == 0 {
		fmt.Println("Nothing to push.")
		return nil
	}

	// Dry run - just show what would be pushed
	if pushDryRun {
		fmt.Printf("\nDry run - would push to %s:\n", remoteName)
		fmt.Printf("  Refs: %d\n", len(refsToSync))
		for _, r := range refsToSync {
			fmt.Printf("    %s -> %s\n", r.Name, hex.EncodeToString(r.TargetID)[:12])
		}
		return nil
	}

	// Skip push if snap.latest hasn't changed since last push. This does NOT
	// apply to a workspace push (--ws): a workspace's state lives in
	// ws.<name>.head, which advances independently of snap.latest, so this
	// snap.latest check would wrongly report "Already up to date." and transmit
	// nothing — leaving the workspace unshareable (F-12).
	if !pushForce && workspaceToPush == nil {
		localLatest, _ := refMgr.Get("snap.latest")
		lastPushed, _ := refMgr.Get("remote/origin/snap.latest")
		if localLatest != nil && lastPushed != nil && bytes.Equal(localLatest.TargetID, lastPushed.TargetID) {
			fmt.Println("Already up to date.")
			return nil
		}
	}

	// Collect all objects to push (including related objects via edges)
	var allDigests [][]byte
	digestSet := make(map[string]bool)

	// Helper to add a digest if not already seen
	addDigest := func(d []byte) {
		key := hex.EncodeToString(d)
		if !digestSet[key] {
			digestSet[key] = true
			allDigests = append(allDigests, d)
		}
	}

	// Collect all objects reachable from each ref target.
	// Uses batched edge queries (1 query per ref instead of 10+).
	var validRefs []*ref.Ref
	for _, r := range refsToSync {
		// Verify the target node exists locally before pushing
		_, _, err := db.GetNodeRawPayload(r.TargetID)
		if err != nil {
			debugf("push: skipping ref %s — target node not found locally", r.Name)
			continue
		}
		validRefs = append(validRefs, r)
		addDigest(r.TargetID)

		// Single query: get all edges from this node (replaces 10 separate GetEdges calls)
		edges, err := db.GetAllEdgesFrom(r.TargetID)
		if err == nil {
			for _, edge := range edges {
				addDigest(edge.Dst)
			}
		}

		// Single query: get all edges where this node is the context
		ctxEdges, err := db.GetAllEdgesByContext(r.TargetID)
		if err == nil {
			for _, edge := range ctxEdges {
				addDigest(edge.Src)
				addDigest(edge.Dst)
			}
		}
	}

	// Also collect content blob digests from File nodes so the negotiate
	// can detect missing blobs. Without this, a normal push after a failed
	// push would say "Already up to date" even though content blobs are missing.
	contentDigestPaths := make(map[string]string) // digest -> file path for disk fallback
	for _, d := range allDigests {
		nodeKind, rawPayloadJSON, err := db.GetNodeRawPayload(d)
		if err != nil || rawPayloadJSON == nil {
			continue
		}
		if nodeKind == graph.KindFile {
			var filePayload map[string]interface{}
			if err := json.Unmarshal(rawPayloadJSON, &filePayload); err == nil {
				if contentDigest, ok := filePayload["digest"].(string); ok {
					if filePath, ok := filePayload["path"].(string); ok {
						contentDigestPaths[contentDigest] = filePath
					}
					cd, err := hex.DecodeString(contentDigest)
					if err == nil {
						addDigest(cd)
					}
				}
			}
		}
	}

	debugf("Pushing to %s (%s)...", remoteName, client.BaseURL)
	if !initMode {
		fmt.Fprintf(os.Stderr, "\rPushing to %s... negotiating  ", remoteName)
	}

	// Skip negotiate for small pushes (< 100 objects) or force pushes
	// Server will dedupe on ingest. This saves a round-trip.
	const negotiateThreshold = 100
	var missing [][]byte

	if pushForce {
		// Force push: skip negotiate, send everything
		debugf("Force push: skipping negotiate, sending all %d objects", len(allDigests))
		missing = allDigests
	} else if len(allDigests) >= negotiateThreshold {
		// Negotiate for larger pushes
		var err error
		missing, err = client.Negotiate(allDigests)
		if err != nil {
			return fmt.Errorf("negotiating: %w", err)
		}
	} else {
		// For small pushes, send all objects (server dedupes)
		missing = allDigests
	}

	if len(missing) > 0 {
		debugf("Objects to push: %d", len(missing))

		// Build pack from missing objects
		var packObjects []remote.PackObject
		contentDigestSet := make(map[string]bool)

		for _, digest := range missing {
			digestHex := hex.EncodeToString(digest)

			// Check if we have a precomputed pack object (for UUID-based nodes like Workspace)
			if precomputed, ok := precomputedPackObjects[digestHex]; ok {
				packObjects = append(packObjects, precomputed)
				continue
			}

			// Get the raw payload JSON to avoid serialization differences
			// (JSON round-tripping can change types like int64 to float64)
			nodeKind, rawPayloadJSON, err := db.GetNodeRawPayload(digest)
			if err != nil || rawPayloadJSON == nil {
				// Not a node — might be a content blob digest
				contentDigestSet[digestHex] = true
				continue
			}

			// Content = kind + "\n" + rawPayloadJSON
			// For content-addressed nodes, digest = blake3(content) = nodeID
			content := append([]byte(string(nodeKind)+"\n"), rawPayloadJSON...)
			packDigest := digest

			// UUID-based nodes (Workspace, Review) should have been precomputed
			// but handle them here as a fallback
			if nodeKind == graph.KindWorkspace || nodeKind == graph.KindReview {
				// Need to add _uuid to payload and recompute content
				node, err := db.GetNode(digest)
				if err != nil || node == nil {
					continue
				}
				payload := make(map[string]interface{})
				for k, v := range node.Payload {
					payload[k] = v
				}
				payload["_uuid"] = hex.EncodeToString(node.ID)
				payloadJSON, err := util.CanonicalJSON(payload)
				if err != nil {
					continue
				}
				content = append([]byte(string(nodeKind)+"\n"), payloadJSON...)
				packDigest = util.Blake3Hash(content)
			}

			packObjects = append(packObjects, remote.PackObject{
				Digest:  packDigest,
				Kind:    string(nodeKind),
				Content: content,
			})

			// For File nodes, also collect the content blob digest and path
			if nodeKind == graph.KindFile {
				// Parse the raw payload to get the digest and path fields
				var filePayload map[string]interface{}
				if err := json.Unmarshal(rawPayloadJSON, &filePayload); err == nil {
					if contentDigest, ok := filePayload["digest"].(string); ok {
						contentDigestSet[contentDigest] = true
						// Track path for fallback disk read if blob not in object store
						if filePath, ok := filePayload["path"].(string); ok {
							contentDigestPaths[contentDigest] = filePath
						}
					}
				}
			}
		}

		// Push content blobs for File nodes
		// Content blobs are stored with digest = blake3(rawContent), no kind prefix.
		// For non-parseable files (svelte, json, yaml, etc.), the content blob
		// is NOT in the object store — only the digest was computed. In that case,
		// read the file from disk and push it.
		for contentDigestHex := range contentDigestSet {
			contentBytes, err := db.ReadObject(contentDigestHex)
			if err != nil {
				// Blob not in object store — try reading from disk
				if filePath, ok := contentDigestPaths[contentDigestHex]; ok {
					contentBytes, err = os.ReadFile(filePath)
					if err != nil {
						debugf("push: cannot read file %s for blob %s: %v", filePath, contentDigestHex[:12], err)
						continue
					}
					// Verify digest matches — file may have changed since capture
					actualDigest := hex.EncodeToString(util.Blake3Hash(contentBytes))
					if actualDigest != contentDigestHex {
						debugf("push: file %s changed since capture (digest mismatch), skipping", filePath)
						continue
					}
				} else {
					continue
				}
			} else {
				// Object store hit — verify the stored content still
				// matches the digest the graph thinks it has. Without
				// this check, a stale/corrupted blob silently rides the
				// push to the server, which then rejects the whole pack
				// with "digest mismatch for object at offset N" (the
				// offset varies across runs because batch ordering
				// shifts the failing object). Cheaper to drop the bad
				// blob here than fail the entire push.
				//
				// If we have a disk path for this digest, prefer that as
				// the fallback — re-read from source rather than skip.
				actualDigest := hex.EncodeToString(util.Blake3Hash(contentBytes))
				if actualDigest != contentDigestHex {
					if filePath, ok := contentDigestPaths[contentDigestHex]; ok {
						diskBytes, derr := os.ReadFile(filePath)
						if derr == nil && hex.EncodeToString(util.Blake3Hash(diskBytes)) == contentDigestHex {
							debugf("push: object store blob %s stale, falling back to disk read at %s", contentDigestHex[:12], filePath)
							contentBytes = diskBytes
						} else {
							debugf("push: object store blob %s digest mismatch (stored=%s, expected=%s), no disk fallback, skipping", contentDigestHex[:12], actualDigest[:12], contentDigestHex[:12])
							continue
						}
					} else {
						debugf("push: object store blob %s digest mismatch (stored=%s, expected=%s), no path known, skipping", contentDigestHex[:12], actualDigest[:12], contentDigestHex[:12])
						continue
					}
				}
			}
			contentDigest, err := hex.DecodeString(contentDigestHex)
			if err != nil {
				continue
			}
			packObjects = append(packObjects, remote.PackObject{
				Digest:  contentDigest,
				Kind:    "Blob",
				Content: contentBytes, // Raw content, no prefix
			})
		}

		debugf("Including %d content blobs", len(contentDigestSet))

		// Pre-flight: verify every object's content matches its
		// declared digest before sending. Without this, a single
		// mismatched object — stale blob in the store, drifted node
		// payload encoding, anything that makes blake3(content) !=
		// Digest — gets the entire pack rejected by the server with
		// "digest mismatch for object at offset N" and zero context.
		// A mismatched object here is NOT droppable. packObjects is the
		// missing-closure for the refs being pushed, so every entry is an
		// object the server needs to reconstruct a snapshot we're about to
		// publish. The old code filtered the bad ones out and printed a
		// "skipped N" warning while still reporting "Pushed to origin" — a
		// hollow green that published a HEADLESS snapshot (ref present, tree
		// incomplete). CI then 404'd at checkout ("failed to fetch file
		// list") and stayed dark for days, because the pre-push hook
		// swallowed the warning. Fail loudly instead: never advance
		// snap.latest/snap.main onto a snapshot the server can't rebuild.
		// The corrupt objects have already lost their working-file fallback
		// (see the disk-reread branch above), so the store must be repaired
		// before the push can be trusted.
		if len(packObjects) > 0 {
			var corrupt []string
			for _, obj := range packObjects {
				actual := util.Blake3Hash(obj.Content)
				if !bytes.Equal(actual, obj.Digest) {
					debugf("push: pre-flight digest mismatch — kind=%s declared=%x computed=%x len=%d",
						obj.Kind, obj.Digest, actual, len(obj.Content))
					corrupt = append(corrupt, hex.EncodeToString(obj.Digest))
				}
			}
			if len(corrupt) > 0 {
				fmt.Fprintf(os.Stderr, "\r\033[K")
				shown := corrupt
				if len(shown) > 8 {
					shown = shown[:8]
				}
				for i := range shown {
					if len(shown[i]) > 12 {
						shown[i] = shown[i][:12]
					}
				}
				more := ""
				if len(corrupt) > len(shown) {
					more = fmt.Sprintf(" (+%d more)", len(corrupt)-len(shown))
				}
				return fmt.Errorf("push aborted: %d object(s) have corrupt local digests and can't be reconstructed: %s%s\n"+
					"  these objects are part of the snapshot being pushed, so publishing would leave a headless ref that fails CI checkout (HTTP 404).\n"+
					"  repair the local store first — restore a .git/kai/db.sqlite backup or re-capture a clean tree — then run 'kai push' again",
					len(corrupt), strings.Join(shown, ", "), more)
			}
		}

		if len(packObjects) > 0 {
			// Batch packs to stay under server size limit (target 50MB per batch)
			const maxBatchSize = 50 * 1024 * 1024 // 50MB
			var batch []remote.PackObject
			var batchSize int64
			batchNum := 1

			// Count total batches accurately by simulating the batching
			totalBatches := 0
			var simBatchSize int64
			for _, obj := range packObjects {
				objSize := int64(len(obj.Content))
				if simBatchSize+objSize > maxBatchSize && simBatchSize > 0 {
					totalBatches++
					simBatchSize = 0
				}
				simBatchSize += objSize
			}
			if simBatchSize > 0 {
				totalBatches++
			}

			for _, obj := range packObjects {
				objSize := int64(len(obj.Content))

				// If adding this object would exceed limit, push current batch
				if batchSize+objSize > maxBatchSize && len(batch) > 0 {
					if !initMode {
						fmt.Fprintf(os.Stderr, "\rPushing to %s... batch %d/%d", remoteName, batchNum, totalBatches)
					}
					debugf("Pushing batch %d/%d (%d objects)...", batchNum, totalBatches, len(batch))
					result, err := client.PushPack(batch)
					if err != nil {
						fmt.Fprintf(os.Stderr, "\r\033[K")
						return fmt.Errorf("pushing pack batch %d: %w", batchNum, err)
					}
					debugf("segment %d", result.SegmentID)
					batch = nil
					batchSize = 0
					batchNum++
				}

				batch = append(batch, obj)
				batchSize += objSize
			}

			// Push remaining batch
			if len(batch) > 0 {
				if totalBatches > 1 {
					if !initMode {
						fmt.Fprintf(os.Stderr, "\rPushing to %s... batch %d/%d", remoteName, batchNum, totalBatches)
					}
					debugf("Pushing batch %d/%d (%d objects)...", batchNum, totalBatches, len(batch))
				} else {
					if !initMode {
						fmt.Fprintf(os.Stderr, "\rPushing to %s... %d objects", remoteName, len(batch))
					}
					debugf("Pushing %d objects...", len(batch))
				}
				result, err := client.PushPack(batch)
				if err != nil {
					fmt.Fprintf(os.Stderr, "\r\033[K")
					return fmt.Errorf("pushing pack: %w", err)
				}
				debugf("segment %d", result.SegmentID)
			}
		}
	} else {
		debugf("All objects already on server.")
	}

	// Fast-forward guard (F-13): refuse to overwrite snap.latest / cs.latest when
	// the remote head has advanced since we last synced. The batch update below
	// feeds the server's compare-and-swap the *current* remote value as the
	// expected "old" value, so the check always passes — which lets a contended
	// push silently clobber another user's snapshot. Here we compare the live
	// remote head against what we last pulled/pushed (the remote/origin/* tracking
	// ref); if our push is not a fast-forward, we abort and tell the user to
	// reconcile. --force overrides. The decision lives in isNonFastForwardPush so
	// it can be unit-tested without a live remote.
	if !pushForce {
		for _, r := range validRefs {
			if r.Name != "snap.latest" && r.Name != "cs.latest" {
				continue
			}
			remoteRef, _ := client.GetRef(r.Name)
			var remoteTarget []byte
			if remoteRef != nil {
				remoteTarget = remoteRef.Target
			}
			var trackedTarget []byte
			if tracked, _ := refMgr.Get("remote/origin/" + r.Name); tracked != nil {
				trackedTarget = tracked.TargetID
			}
			if isNonFastForwardPush(remoteTarget, trackedTarget, r.TargetID) {
				fmt.Fprintf(os.Stderr, "\r\033[K")
				return fmt.Errorf(
					"push rejected: remote %s has advanced to %s since you last synced — your push is not a fast-forward.\n"+
						"  Run 'kai pull' to reconcile, or 'kai push --force' to overwrite the remote.",
					r.Name, hex.EncodeToString(remoteTarget)[:12])
			}
		}
	}

	// Batch update refs (single round-trip instead of N)
	// Falls back to individual updates if server doesn't support batch endpoint
	var batchUpdates []remote.BatchRefUpdate
	for _, r := range validRefs {
		// Get old value from remote
		remoteRef, _ := client.GetRef(r.Name)
		var oldTarget []byte
		if remoteRef != nil {
			oldTarget = remoteRef.Target
		}
		batchUpdates = append(batchUpdates, remote.BatchRefUpdate{
			Name:  r.Name,
			Old:   oldTarget,
			New:   r.TargetID,
			Force: pushForce,
			Meta:  r.Meta,
		})
	}

	var pushUsage *remote.UsageInfo
	if len(batchUpdates) > 0 {
		result, err := client.BatchUpdateRefs(batchUpdates)
		if err != nil {
			// Check for usage limit error. Current server only meters live-sync
			// pushes from MCP sessions; plain 'kai push' should no longer hit
			// this. Left in place for older servers that still meter ref
			// updates.
			if limErr, ok := err.(*remote.CommitLimitError); ok {
				fmt.Fprintf(os.Stderr, "\r\033[K")
				fmt.Println()
				fmt.Println("  Push blocked: usage limit reached for this billing period.")
				fmt.Println()
				fmt.Printf("    Plan:    %s\n", limErr.Tier)
				fmt.Printf("    Used:    %d / %d events\n", limErr.Used, limErr.Limit)
				fmt.Println()
				if limErr.UpgradeURL != "" {
					fmt.Printf("  Upgrade for a higher limit: %s\n", limErr.UpgradeURL)
				}
				fmt.Println()
				return fmt.Errorf("usage limit reached")
			}
			// Fallback to individual updates if batch not supported (405 or other error)
			if strings.Contains(err.Error(), "405") || strings.Contains(err.Error(), "Method Not Allowed") {
				for _, upd := range batchUpdates {
					res, err := client.UpdateRef(upd.Name, upd.Old, upd.New, upd.Force)
					if err != nil {
						debugf("Failed to update ref %s: %v", upd.Name, err)
						continue
					}
					if res.OK {
						debugf("%s -> %s (push %s)", upd.Name, hex.EncodeToString(upd.New)[:12], res.PushID[:8])
					} else {
						debugf("%s: %s", upd.Name, res.Error)
					}
				}
			} else {
				return fmt.Errorf("updating refs: %w", err)
			}
		} else {
			pushUsage = result.Usage
			for _, res := range result.Results {
				if res.OK {
					debugf("%s -> updated (push %s)", res.Name, result.PushID[:8])
				} else {
					debugf("%s: %s", res.Name, res.Error)
				}
			}
		}
	}

	// Push edges for pushed snapshots
	// Collect edges for all snapshot refs we just pushed
	var edgesToPush []remote.EdgeData
	for _, r := range validRefs {
		// Only push edges for snapshots (where import/test analysis is scoped)
		if r.TargetKind != ref.KindSnapshot {
			continue
		}

		// Get edges scoped to this snapshot (IMPORTS, TESTS, CALLS, etc.)
		for _, edgeType := range []graph.EdgeType{
			graph.EdgeImports,
			graph.EdgeTests,
			graph.EdgeCalls,
		} {
			edges, err := db.GetEdgesByContext(r.TargetID, edgeType)
			if err != nil {
				continue
			}
			for _, edge := range edges {
				edgesToPush = append(edgesToPush, remote.EdgeData{
					Src:  hex.EncodeToString(edge.Src),
					Type: string(edge.Type),
					Dst:  hex.EncodeToString(edge.Dst),
					At:   hex.EncodeToString(edge.At),
				})
			}
		}
	}

	if len(edgesToPush) > 0 {
		if !initMode {
			fmt.Fprintf(os.Stderr, "\rPushing to %s... %d edges", remoteName, len(edgesToPush))
		}
		debugf("Pushing %d edges...", len(edgesToPush))
		result, err := client.PushEdges(edgesToPush)
		if err != nil {
			// Don't fail the push if edge push fails - edges are supplementary
			debugf("edge push warning: %v", err)
		} else {
			debugf("%d edges inserted", result.Inserted)
		}
	}

	// Push authorship data for all snapshots being pushed
	var authorshipData []remote.AuthorshipData
	for _, r := range validRefs {
		if r.TargetKind != ref.KindSnapshot {
			continue
		}
		ranges, err := db.GetAllAuthorshipRanges(r.TargetID)
		if err != nil || len(ranges) == 0 {
			continue
		}
		snapHex := hex.EncodeToString(r.TargetID)
		for _, ar := range ranges {
			authorshipData = append(authorshipData, remote.AuthorshipData{
				SnapshotID: snapHex,
				FilePath:   ar.FilePath,
				StartLine:  ar.StartLine,
				EndLine:    ar.EndLine,
				AuthorType: ar.AuthorType,
				Agent:      ar.Agent,
				Model:      ar.Model,
				SessionID:  ar.SessionID,
			})
		}
	}
	if len(authorshipData) > 0 {
		debugf("Pushing %d authorship ranges...", len(authorshipData))
		result, err := client.PushAuthorship(authorshipData)
		if err != nil {
			// Don't fail the push if authorship push fails - it's supplementary
			debugf("authorship push warning: %v", err)
		} else {
			debugf("%d authorship ranges inserted", result.Inserted)
		}
	}

	// Keep snap.main in sync with snap.latest (CI checkouts use snap.main)
	latestRef, _ := refMgr.Get("snap.latest")
	mainRef, _ := refMgr.Get("snap.main")
	if latestRef != nil && (mainRef == nil || !bytes.Equal(latestRef.TargetID, mainRef.TargetID)) {
		refMgr.Set("snap.main", latestRef.TargetID, ref.KindSnapshot)
		// Push the updated snap.main ref to remote
		client.UpdateRef("snap.main", nil, latestRef.TargetID, true)
	}

	// Track what we pushed so duplicate pushes are skipped
	for _, r := range validRefs {
		remoteRefName := fmt.Sprintf("remote/%s/%s", remoteName, r.Name)
		refMgr.Set(remoteRefName, r.TargetID, r.TargetKind)
	}

	if !initMode {
		fmt.Fprintf(os.Stderr, "\r\033[K")
		fmt.Printf("Pushed to %s.\n", remoteName)
	}

	// CI dedup + edge creation: check if CI runs exist for the pushed snapshot
	if localLatest, _ := refMgr.Get("snap.latest"); localLatest != nil {
		snapHex := util.BytesToHex(localLatest.TargetID)
		// Drop a marker so `kai despawn` can tell whether the workspace
		// has unpushed snapshots without having to round-trip the server.
		_ = os.WriteFile(filepath.Join(kaiDir, spawnpkg.LastPushFile), []byte(snapHex), 0644)
		ctrl := remote.NewControlClient(client.BaseURL)
		if runs, _, err := ctrl.ListCIRuns(client.Tenant, client.Repo, 10); err == nil {
			for _, r := range runs {
				if r.SnapshotID != snapHex && r.TriggerSHA != snapHex {
					continue
				}
				if r.Conclusion == "success" {
					fmt.Fprintf(os.Stderr, "CI: prior success for this snapshot (run #%d), server will skip re-run\n", r.RunNumber)
				}
				// Write HAS_CI_RUN edge locally so kai ci status can query it
				runIDBytes, _ := hex.DecodeString(r.ID)
				if len(runIDBytes) == 0 {
					// Run ID might not be hex — derive a deterministic ID from the run metadata
					h := sha256.Sum256([]byte("cirun:" + r.ID))
					runIDBytes = h[:]
				}
				tx, txErr := db.BeginTx()
				if txErr == nil {
					db.InsertEdge(tx, localLatest.TargetID, graph.EdgeHasCIRun, runIDBytes, nil)
					tx.Commit()
				}
				break
			}
		}
	}

	// Display billing usage warnings
	if pushUsage != nil {
		pct := 0
		if pushUsage.Limit > 0 {
			pct = pushUsage.Used * 100 / pushUsage.Limit
		}
		// Usage reflects live-sync events from MCP sessions, not 'kai push'
		// itself — the latter is always free. We still surface these warnings
		// on kai push because it's the natural moment to tell a user about
		// their monthly sync budget.
		switch {
		case pushUsage.Plan == "pro":
			// Pro = unlimited sync events in the post-overage-billing
			// world. Show a quiet stat without the overage line.
			fmt.Printf("\n  Pro plan: %d sync events this period (unlimited)\n\n", pushUsage.Used)
		case pct >= 90:
			fmt.Printf("\n  Usage: %d / %d agent sync events (%d%%) this period\n", pushUsage.Used, pushUsage.Limit, pct)
			if pushUsage.UpgradeURL != "" {
				fmt.Printf("  Upgrade to Pro: %s\n", pushUsage.UpgradeURL)
			}
			fmt.Println()
		case pct >= 80:
			fmt.Printf("\n  Usage: %d / %d agent sync events (%d%%) this period\n\n", pushUsage.Used, pushUsage.Limit, pct)
		}
	}

	return nil
}

func runFetch(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("fetch")
	defer te.Finish()

	// Show explain if requested
	if fetchExplain {
		remoteName := "origin"
		if len(args) > 0 {
			remoteName = args[0]
		}
		ctx := explain.ExplainFetch(remoteName, 0) // 0 refs - count determined later
		ctx.Print(os.Stdout)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Determine remote name
	remoteName := "origin"
	refsToFetch := []string{}

	if len(args) > 0 {
		// Check if first arg is a remote name
		if _, err := remote.GetRemote(args[0]); err == nil {
			remoteName = args[0]
			refsToFetch = args[1:]
		} else {
			// First arg is a ref
			refsToFetch = args
		}
	}

	// Create client for remote
	client, err := remote.NewClientForRemote(remoteName)
	if err != nil {
		return fmt.Errorf("remote '%s' not configured (use 'kai remote set %s <url>')", remoteName, remoteName)
	}

	// Check server health
	debugf("fetch: connecting to %s", client.BaseURL)
	if err := client.Health(); err != nil {
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	fmt.Printf("Fetching from %s (%s)...\n", remoteName, client.BaseURL)

	// Handle workspace fetch if --ws flag is set
	if fetchWorkspace != "" {
		return fetchWorkspaceFromRemote(db, client, remoteName, fetchWorkspace)
	}

	// Handle review fetch if --review flag is set
	if fetchReview != "" {
		return fetchReviewFromRemote(db, client, remoteName, fetchReview)
	}

	// Get refs from remote
	var remoteRefs []*remote.RefEntry
	if len(refsToFetch) > 0 {
		for _, name := range refsToFetch {
			r, err := client.GetRef(name)
			if err != nil {
				fmt.Printf("  Warning: failed to get ref %s: %v\n", name, err)
				continue
			}
			if r != nil {
				remoteRefs = append(remoteRefs, r)
			}
		}
	} else {
		// Fetch all refs
		remoteRefs, err = client.ListRefs("")
		if err != nil {
			return fmt.Errorf("listing refs: %w", err)
		}
	}

	debugf("fetch: %d refs from remote", len(remoteRefs))
	if len(remoteRefs) == 0 {
		fmt.Println("No refs to fetch.")
		return nil
	}

	fmt.Printf("  Found %d ref(s)\n", len(remoteRefs))

	// Collect objects to fetch
	var objectsToFetch [][]byte
	for _, r := range remoteRefs {
		exists, _ := db.HasNode(r.Target)
		if !exists {
			objectsToFetch = append(objectsToFetch, r.Target)
		}
	}

	if len(objectsToFetch) > 0 {
		fmt.Printf("  Objects to fetch: %d\n", len(objectsToFetch))

		for _, digest := range objectsToFetch {
			content, kind, err := client.GetObject(digest)
			if err != nil {
				fmt.Printf("  Warning: failed to get object %s: %v\n", hex.EncodeToString(digest)[:12], err)
				continue
			}

			if content != nil {
				// Parse and store the object
				var payload map[string]interface{}
				if err := json.Unmarshal(content, &payload); err != nil {
					fmt.Printf("  Warning: failed to parse object %s: %v\n", hex.EncodeToString(digest)[:12], err)
					continue
				}

				// Insert node directly
				tx, err := db.BeginTx()
				if err != nil {
					continue
				}
				_, err = db.InsertNode(tx, graph.NodeKind(kind), payload)
				if err != nil {
					tx.Rollback()
					continue
				}
				tx.Commit()
			}
		}
	}

	// Update local refs (prefixed with remote name)
	refMgr := ref.NewRefManager(db)
	for _, r := range remoteRefs {
		// Store as remote/origin/snap.main style
		localName := fmt.Sprintf("remote/%s/%s", remoteName, r.Name)
		kind := ref.KindSnapshot // Default
		if strings.HasPrefix(r.Name, "cs.") {
			kind = ref.KindChangeSet
		} else if strings.HasPrefix(r.Name, "ws.") {
			kind = ref.KindWorkspace
		} else if strings.HasPrefix(r.Name, "tag.") {
			kind = ref.KindSnapshot
		}

		if err := refMgr.Set(localName, r.Target, kind); err != nil {
			fmt.Printf("  Warning: failed to set ref %s: %v\n", localName, err)
			continue
		}
		fmt.Printf("  %s -> %s\n", localName, hex.EncodeToString(r.Target)[:12])
	}

	fmt.Println("Fetch complete.")
	return nil
}

// pullTagsAndReviews brings tag.* and review.* refs across during `kai pull`
// (F-15). Pull historically synced only snap.latest/cs.latest, so a teammate who
// pulled never saw release tags or code reviews — even though `kai push` sends
// them. Tags become usable LOCAL tag.* refs (not just remote/origin tracking
// refs); reviews are reconstructed via the UUID-preserving review sync and
// skipped when already present locally, so repeated pulls are idempotent (no
// duplicate Review nodes). Best-effort: failures are logged, not fatal — a tag
// or review that won't fetch must not abort the snapshot pull.
func pullTagsAndReviews(db *graph.DB, client *remote.Client, refMgr *ref.RefManager, remoteName string) (int, int) {
	remoteRefs, err := client.ListRefs("")
	if err != nil {
		debugf("pull: listing remote refs for tags/reviews: %v", err)
		return 0, 0
	}
	tags, reviews := 0, 0
	for _, r := range remoteRefs {
		switch {
		case strings.HasPrefix(r.Name, "tag."):
			// Ensure the tagged object is present locally.
			if exists, _ := db.HasNode(r.Target); !exists {
				if content, kind, gerr := client.GetObject(r.Target); gerr == nil && content != nil {
					var payload map[string]interface{}
					if json.Unmarshal(content, &payload) == nil {
						if tx, terr := db.BeginTx(); terr == nil {
							if _, ierr := db.InsertNode(tx, graph.NodeKind(kind), payload); ierr == nil {
								tx.Commit()
							} else {
								tx.Rollback()
							}
						}
					}
				}
			}
			// Create/update a usable LOCAL tag (the whole point: `kai tag list`
			// must show it), plus the remote tracking ref.
			existing, _ := refMgr.Get(r.Name)
			if existing == nil || !bytes.Equal(existing.TargetID, r.Target) {
				if serr := refMgr.Set(r.Name, r.Target, ref.KindSnapshot); serr != nil {
					debugf("pull: setting tag %s: %v", r.Name, serr)
					continue
				}
				tags++
			}
			refMgr.Set("remote/"+remoteName+"/"+r.Name, r.Target, ref.KindSnapshot)
		case strings.HasPrefix(r.Name, "review.") && !strings.Contains(strings.TrimPrefix(r.Name, "review."), "."):
			// Only real review refs (`review.<hex-uuid>`); skip companion refs
			// like `review.<id>.target`, which point at the review's changeset,
			// not a Review node. (Tags legitimately contain dots, e.g. v1.0, so
			// this guard is review-only.)
			// Idempotent: once we have this review locally, skip re-syncing it.
			if existing, _ := refMgr.Get(r.Name); existing != nil {
				continue
			}
			reviewID := strings.TrimPrefix(r.Name, "review.")
			if serr := syncReviewFromRemote(db, client, remoteName, reviewID); serr != nil {
				debugf("pull: syncing review %s: %v", reviewID, serr)
				continue
			}
			reviews++
		}
	}
	if tags > 0 {
		fmt.Printf("  Pulled %d tag(s)\n", tags)
	}
	if reviews > 0 {
		fmt.Printf("  Pulled %d review(s)\n", reviews)
	}
	return tags, reviews
}

func runPull(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("pull")
	defer te.Finish()

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Determine remote name
	remoteName := "origin"
	if len(args) > 0 {
		remoteName = args[0]
	}

	// Create client for remote
	client, err := remote.NewClientForRemote(remoteName)
	if err != nil {
		return fmt.Errorf("remote '%s' not configured (use 'kai remote set %s <url>')", remoteName, remoteName)
	}

	// Check server health
	if err := client.Health(); err != nil {
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	fmt.Printf("Pulling from %s (%s)...\n", remoteName, client.BaseURL)

	// Get snap.latest ref from remote
	snapRef, err := client.GetRef("snap.latest")
	if err != nil {
		return fmt.Errorf("getting remote snap.latest: %w", err)
	}
	if snapRef == nil {
		fmt.Println("No snap.latest on remote — nothing to pull.")
		return nil
	}

	remoteDigest := hex.EncodeToString(snapRef.Target)
	fmt.Printf("  Remote snap.latest: %s\n", remoteDigest[:12])

	// Check if we already have this snapshot locally
	refMgr := ref.NewRefManager(db)
	localRef, _ := refMgr.Get("snap.latest")
	if localRef != nil && hex.EncodeToString(localRef.TargetID) == remoteDigest {
		// snap.latest is already current, but tags and reviews still need to be
		// synced — they're independent of the snapshot head (F-15).
		pullTagsAndReviews(db, client, refMgr, remoteName)
		fmt.Println("Already up to date.")
		return nil
	}

	// Reconcile against the remote (F-9): the remote head differs from ours
	// (the equal case returned above). Distinguish a clean fast-forward — our
	// local head is exactly what we last synced, so we hold no unpushed work and
	// the remote has merely advanced — from a real divergence. A behind clone is
	// the former and must be allowed to catch up without --force; only the latter
	// aborts. Compares against the remote/origin/* tracking ref (what we last
	// pulled/pushed), mirroring the push-side guard, since snapshots carry no
	// ancestry to compute a true merge-base (F-14).
	if localRef != nil && !pullForce {
		localDigest := hex.EncodeToString(localRef.TargetID)
		var trackedTarget []byte
		if tracked, _ := refMgr.Get("remote/origin/snap.latest"); tracked != nil {
			trackedTarget = tracked.TargetID
		}
		if !isFastForwardPull(localRef.TargetID, trackedTarget, snapRef.Target) {
			// Local has snapshots beyond what we last synced; pulling would
			// orphan them. Make the user reconcile explicitly.
			fmt.Printf("  Warning: local snap.latest (%s) differs from remote (%s)\n",
				localDigest[:12], remoteDigest[:12])
			fmt.Println("  You have unpushed local snapshots that will become orphaned.")
			fmt.Println()
			fmt.Println("  To pull anyway:  kai pull --force")
			fmt.Println("  To push first:   kai push")
			return fmt.Errorf("pull aborted: local and remote have diverged")
		}
		// Clean fast-forward: fall through to fetch and advance to the remote head.
	}

	// Fetch the snapshot node if we don't have it
	exists, _ := db.HasNode(snapRef.Target)
	if !exists {
		content, kind, err := client.GetObject(snapRef.Target)
		if err != nil {
			return fmt.Errorf("fetching snapshot object: %w", err)
		}
		if content == nil {
			return fmt.Errorf("snapshot object not found on remote")
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(content, &payload); err != nil {
			return fmt.Errorf("parsing snapshot: %w", err)
		}

		tx, err := db.BeginTx()
		if err != nil {
			return fmt.Errorf("starting transaction: %w", err)
		}
		if _, err := db.InsertNode(tx, graph.NodeKind(kind), payload); err != nil {
			tx.Rollback()
			return fmt.Errorf("inserting snapshot node: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing snapshot: %w", err)
		}
	}

	// List all files in the remote snapshot
	filesResp, err := client.ListSnapshotFiles("snap.latest")
	if err != nil {
		return fmt.Errorf("listing remote files: %w", err)
	}

	fmt.Printf("  Remote snapshot has %d file(s)\n", len(filesResp.Files))

	// Fetch file nodes and content blobs
	fetched := 0
	for _, f := range filesResp.Files {
		// Check if we have the file node
		fileDigest, err := hex.DecodeString(f.Digest)
		if err != nil {
			continue
		}
		exists, _ := db.HasNode(fileDigest)
		if !exists {
			// Fetch file node
			content, kind, err := client.GetObject(fileDigest)
			if err != nil {
				fmt.Printf("  Warning: failed to fetch file node %s: %v\n", f.Path, err)
				continue
			}
			if content != nil {
				var payload map[string]interface{}
				if err := json.Unmarshal(content, &payload); err == nil {
					tx, _ := db.BeginTx()
					if _, err := db.InsertNode(tx, graph.NodeKind(kind), payload); err == nil {
						tx.Commit()
					} else {
						tx.Rollback()
					}
				}
			}
		}

		// Check if we have the content blob. The /v1/raw/ endpoint dereferences
		// the file NODE digest (f.Digest) to its content — fetching by the content
		// digest 404s — so request by f.Digest (matching clone). The blob must land
		// in the object store for the post-pull working-tree materialization below
		// (F-10) to write it. The old code fetched by the wrong digest and then,
		// on the resulting failure, skipped whenever any file existed on disk —
		// even a stale one — which is exactly why the tree was left out of date.
		_, readErr := db.ReadObject(f.ContentDigest)
		if readErr != nil {
			// Fetch raw content from remote
			rawContent, err := client.GetRawContent(f.Digest)
			if err != nil {
				// Remote couldn't supply the blob. If the on-disk file already
				// matches the target content we don't need it; otherwise warn —
				// the working tree will be left stale for this file.
				if onDiskContentMatches(f.Path, f.ContentDigest) {
					continue // on-disk content already matches — no blob needed
				}
				if os.Getenv("KAI_PULL_QUIET") == "" {
					fmt.Printf("  Warning: failed to fetch content for %s: %v\n", f.Path, err)
				}
				continue
			}
			if _, err := db.WriteObject(rawContent); err != nil {
				fmt.Printf("  Warning: failed to store content for %s: %v\n", f.Path, err)
				continue
			}
			fetched++
		}

		// Ensure HAS_FILE edge exists
		db.InsertEdgeDirect(snapRef.Target, graph.EdgeHasFile, fileDigest, nil)
	}

	if fetched > 0 {
		fmt.Printf("  Fetched %d content blob(s)\n", fetched)
	}

	// Also fetch cs.latest if available
	csRef, err := client.GetRef("cs.latest")
	if err == nil && csRef != nil {
		csExists, _ := db.HasNode(csRef.Target)
		if !csExists {
			content, kind, err := client.GetObject(csRef.Target)
			if err == nil && content != nil {
				var payload map[string]interface{}
				if err := json.Unmarshal(content, &payload); err == nil {
					tx, _ := db.BeginTx()
					if _, err := db.InsertNode(tx, graph.NodeKind(kind), payload); err == nil {
						tx.Commit()
					} else {
						tx.Rollback()
					}
				}
			}
		}
		refMgr.Set("cs.latest", csRef.Target, ref.KindChangeSet)
	}

	// Update local snap.latest
	if err := refMgr.Set("snap.latest", snapRef.Target, ref.KindSnapshot); err != nil {
		return fmt.Errorf("updating local snap.latest: %w", err)
	}

	// Advance the remote-tracking ref to the head we just synced so a later pull
	// short-circuits cleanly and the fast-forward guard reflects reality.
	_ = refMgr.Set("remote/origin/snap.latest", snapRef.Target, ref.KindSnapshot)

	// Bring tags and reviews across too — pull is the documented sync verb (F-15).
	pullTagsAndReviews(db, client, refMgr, remoteName)

	// Materialize the working tree to the pulled snapshot (F-10). Pull fetched
	// the blobs into the object store but never wrote the files, leaving the
	// graph at the new head while the working tree kept the old content — a
	// silent ref-vs-disk drift, acute on kai-only clones with no git tree to
	// fall back on. Checkout(clean=false) overwrites only files whose on-disk
	// content differs from the snapshot and never deletes, so it is a no-op when
	// the tree is already current (e.g. a git-backed repo git already updated).
	worktreeRoot, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}
	if written, cerr := materializeWorkingTree(db, snapRef.Target, worktreeRoot); cerr != nil {
		fmt.Fprintf(os.Stderr, "  Warning: pulled the graph but failed to update the working tree: %v\n", cerr)
	} else if written > 0 {
		fmt.Printf("  Updated %d file(s) in the working tree\n", written)
	}

	fmt.Printf("  snap.latest -> %s\n", remoteDigest[:12])
	fmt.Println("Pull complete.")
	return nil
}

// runCloneKaiOnly clones a repository that exists only on kaicontext.com,
// with no git backing. Creates a bare .kai/ directory, fetches the snapshot
// graph from the server, and materializes files from snap.main (falling back
// to snap.latest) into the working directory.
func runCloneKaiOnly(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("URL required")
	}
	rawURL := args[0]

	// Parse URL into serverURL/tenant/repo.
	// Accepts: https://kaicontext.com/org/repo
	//          http://localhost:8080/org/repo
	//          kaicontext.com/org/repo (scheme inferred)
	//          org/repo (defaults to DefaultServer)
	serverURL := remote.DefaultServer
	var tenant, repo string
	if cloneTenant != "" && cloneRepo != "" {
		tenant = cloneTenant
		repo = cloneRepo
		if !strings.HasPrefix(rawURL, "http") && !strings.Contains(rawURL, "/") {
			// URL was actually just a shorthand that should be used as-is
		} else {
			serverURL = rawURL
		}
	} else {
		u := rawURL
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			// Distinguish 'org/repo' shorthand from 'host/path' input. If the
			// first path segment looks like a hostname (contains '.' or ':',
			// or matches the localhost special cases), treat the whole thing
			// as a server URL. Otherwise it's shorthand against DefaultServer.
			firstSeg := strings.SplitN(u, "/", 2)[0]
			isHostnameLike := strings.Contains(firstSeg, ".") ||
				strings.Contains(firstSeg, ":") ||
				firstSeg == "localhost" ||
				firstSeg == "127.0.0.1"
			if !isHostnameLike {
				u = strings.TrimRight(remote.DefaultServer, "/") + "/" + u
			} else if strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") {
				u = "http://" + u
			} else {
				u = "https://" + u
			}
		}
		parsed, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("parsing URL: %w", err)
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) < 2 {
			return fmt.Errorf("URL must include /<tenant>/<repo>")
		}
		tenant, repo = parts[0], parts[1]
		serverURL = parsed.Scheme + "://" + parsed.Host
	}

	// Determine target directory
	dirName := repo
	if len(args) > 1 {
		dirName = args[1]
	}
	absDir, err := filepath.Abs(dirName)
	if err != nil {
		return fmt.Errorf("resolving target directory: %w", err)
	}

	// Refuse to overwrite an existing non-empty directory
	if entries, err := os.ReadDir(absDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("directory %q already exists and is not empty", dirName)
	}
	if err := os.MkdirAll(absDir, 0755); err != nil {
		return fmt.Errorf("creating target directory: %w", err)
	}

	fmt.Printf("Cloning %s/%s from %s into %s...\n", tenant, repo, serverURL, dirName)

	// Set up the kai data directory and DB inside the target
	kaiPath := kaipath.Resolve(absDir)
	objPath := filepath.Join(kaiPath, objectsDir)
	if err := os.MkdirAll(objPath, 0755); err != nil {
		return fmt.Errorf("creating kai data directory: %w", err)
	}
	dbPath := filepath.Join(kaiPath, dbFile)
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		return fmt.Errorf("opening local database: %w", err)
	}
	defer db.Close()
	if err := applyDBSchema(db); err != nil {
		return fmt.Errorf("initializing schema: %w", err)
	}

	// We need to run subsequent operations from the new directory so that
	// "origin" remote config and fetch write to the right .kai/.
	origDir, _ := os.Getwd()
	if err := os.Chdir(absDir); err != nil {
		return fmt.Errorf("chdir %s: %w", absDir, err)
	}
	defer os.Chdir(origDir)

	// Set the remote
	if err := remote.ForceSetRemote("origin", &remote.RemoteEntry{
		URL:    serverURL,
		Tenant: tenant,
		Repo:   repo,
	}); err != nil {
		return fmt.Errorf("setting remote: %w", err)
	}

	// Build remote client and verify it's reachable + that the repo exists
	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}
	if err := client.Health(); err != nil {
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	// Pick the head ref: prefer snap.main, fall back to snap.latest
	fmt.Println("Resolving head snapshot...")
	var headRef *remote.RefEntry
	for _, name := range []string{"snap.main", "snap.latest"} {
		r, err := client.GetRef(name)
		if err == nil && r != nil {
			headRef = r
			fmt.Printf("  Using %s -> %s\n", name, hex.EncodeToString(r.Target)[:12])
			break
		}
	}
	if headRef == nil {
		return fmt.Errorf("no snap.main or snap.latest on remote — nothing to clone")
	}

	// Fetch the snapshot node + all file nodes and content blobs.
	// This mirrors the pull logic (duplicated inline so we don't need to
	// rely on refs existing in a particular order).
	fmt.Println("Fetching snapshot...")
	snapContent, snapKind, err := client.GetObject(headRef.Target)
	if err != nil {
		return fmt.Errorf("fetching snapshot object: %w", err)
	}
	if snapContent == nil {
		return fmt.Errorf("snapshot not found on remote")
	}
	var snapPayload map[string]interface{}
	if err := json.Unmarshal(snapContent, &snapPayload); err != nil {
		return fmt.Errorf("parsing snapshot: %w", err)
	}
	tx, err := db.BeginTx()
	if err != nil {
		return err
	}
	if _, err := db.InsertNode(tx, graph.NodeKind(snapKind), snapPayload); err != nil {
		tx.Rollback()
		return fmt.Errorf("inserting snapshot node: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// List files and fetch each blob
	filesResp, err := client.ListSnapshotFiles(hex.EncodeToString(headRef.Target))
	if err != nil {
		return fmt.Errorf("listing files: %w", err)
	}
	fmt.Printf("Fetching %d files...\n", len(filesResp.Files))
	fetched := 0
	for _, f := range filesResp.Files {
		fileDigest, err := hex.DecodeString(f.Digest)
		if err != nil {
			continue
		}
		// File node
		if exists, _ := db.HasNode(fileDigest); !exists {
			nodeContent, nodeKind, err := client.GetObject(fileDigest)
			if err == nil && nodeContent != nil {
				var payload map[string]interface{}
				if err := json.Unmarshal(nodeContent, &payload); err == nil {
					t, _ := db.BeginTx()
					if _, err := db.InsertNode(t, graph.NodeKind(nodeKind), payload); err == nil {
						t.Commit()
					} else {
						t.Rollback()
					}
				}
			}
		}
		// Content blob — the /v1/raw/ endpoint takes the file NODE digest
		// (not the content blob digest) and dereferences through the file
		// node to return the raw bytes.
		if _, err := db.ReadObject(f.ContentDigest); err != nil {
			raw, err := client.GetRawContent(f.Digest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %s: %v\n", f.Path, err)
				continue
			}
			if _, err := db.WriteObject(raw); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %s: %v\n", f.Path, err)
				continue
			}
			fetched++
		}
		// HAS_FILE edge
		db.InsertEdgeDirect(headRef.Target, graph.EdgeHasFile, fileDigest, nil)
	}
	fmt.Printf("  Fetched %d content blobs\n", fetched)

	// Set local snap.latest and snap.main to the same target so subsequent
	// commands behave as expected.
	refMgr := ref.NewRefManager(db)
	_ = refMgr.Set("snap.latest", headRef.Target, ref.KindSnapshot)
	_ = refMgr.Set("snap.main", headRef.Target, ref.KindSnapshot)
	// Also store as remote-tracking ref so kai pull recognizes no divergence
	_ = refMgr.Set("remote/origin/snap.latest", headRef.Target, ref.KindSnapshot)

	// Materialize files to disk
	fmt.Println("Writing files...")
	creator := snapshot.NewCreator(db, nil)
	result, err := creator.Checkout(headRef.Target, absDir, false)
	if err != nil {
		return fmt.Errorf("checkout failed: %w", err)
	}
	fmt.Printf("  Wrote %d files\n", result.FilesWritten)

	// Add the data dir to .gitignore so a future `git init` plays nicely.
	// Only needed when the data dir lives in the worktree (.kai); when it
	// lands in .git/kai, git ignores it automatically.
	if kaipath.NeedsGitignore(kaiPath) {
		ensureGitignore(filepath.Base(kaiPath))
	}

	fmt.Println()
	fmt.Printf("✓ Cloned %s/%s into %s\n", tenant, repo, dirName)
	fmt.Println("  Run 'cd " + dirName + "' to start working.")
	return nil
}

// gitCloneTargetDir returns the directory `git clone <url> [dir]` writes into:
// the explicit second arg if given, otherwise the final path segment of the URL
// with any trailing ".git", scheme, and host stripped — mirroring git's own
// naming rule. Returns "" when there are no args. Used to locate the directory
// to clean up after a failed clone and to set up Kai afterwards.
func gitCloneTargetDir(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) > 1 && args[1] != "" {
		return args[1]
	}
	name := strings.TrimSuffix(args[0], ".git")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	} else if idx := strings.LastIndex(name, ":"); idx >= 0 {
		name = name[idx+1:]
		if idx2 := strings.LastIndex(name, "/"); idx2 >= 0 {
			name = name[idx2+1:]
		}
	}
	return name
}

// shouldCleanPartialClone reports whether, after a failed `git clone`, it is
// safe to remove targetDir before retrying as a Kai-only clone. It is safe only
// when the directory did not already exist with content before the clone — i.e.
// git created it (and usually cleans it up itself, but not always), so removing
// it destroys nothing the user had. existedNonEmpty is sampled BEFORE git runs;
// when it is true, git refused to touch the directory and we must never delete
// the user's data.
func shouldCleanPartialClone(targetDir string, existedNonEmpty bool) bool {
	return targetDir != "" && !existedNonEmpty
}

// dirExistsNonEmpty reports whether path is an existing directory with at least
// one entry.
func dirExistsNonEmpty(path string) bool {
	if path == "" {
		return false
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func runClone(cmd *cobra.Command, args []string) error {
	if cloneKaiOnly {
		return runCloneKaiOnly(args)
	}

	// The directory git will create, and whether it already held content before
	// we touched it — captured up front so a failed clone can be cleaned up
	// safely before the Kai-only fallback (which refuses a non-empty target).
	dirName := gitCloneTargetDir(args)
	existedNonEmpty := dirExistsNonEmpty(dirName)

	// Forward to git clone, then set up kai
	gitArgs := append([]string{"clone"}, args...)
	gitCmd := exec.Command("git", gitArgs...)
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr
	gitCmd.Stdin = os.Stdin
	if err := gitCmd.Run(); err != nil {
		// The remote may be Kai-only (no git backing) — exactly the repos that
		// `kai init` + push produce. Rather than failing, fall back to the
		// Kai-only clone path. Remove any partial directory git left behind
		// first (a no-op when git already cleaned up), but only when it is safe
		// to do so — never delete a directory the user already had.
		if shouldCleanPartialClone(dirName, existedNonEmpty) {
			_ = os.RemoveAll(dirName)
		}
		fmt.Fprintln(os.Stderr, "git clone failed; trying Kai-only clone (the remote may have no git backing)...")
		if kaiErr := runCloneKaiOnly(args); kaiErr != nil {
			return fmt.Errorf("git clone failed (%v); Kai-only clone also failed: %w", err, kaiErr)
		}
		return nil
	}

	if dirName == "" || dirName == "." {
		fmt.Println("Run 'kai capture' to build the semantic graph.")
		return nil
	}

	origDir, _ := os.Getwd()
	os.Chdir(dirName)
	defer os.Chdir(origDir)

	// Build the semantic graph from the cloned files
	fmt.Println("\nBuilding semantic graph...")
	captureCmd := exec.Command("kai", "capture")
	captureCmd.Stdout = os.Stdout
	captureCmd.Stderr = os.Stderr
	captureCmd.Run()

	// Detect tenant/repo from git remote and set kai remote
	tenant := cloneTenant
	repo := cloneRepo
	if tenant == "" || repo == "" {
		if out, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
			gitURL := strings.TrimSpace(string(out))
			gitURL = strings.TrimSuffix(gitURL, ".git")
			if idx := strings.LastIndex(gitURL, ":"); idx >= 0 {
				parts := strings.Split(gitURL[idx+1:], "/")
				if len(parts) == 2 {
					tenant = parts[0]
					repo = parts[1]
				}
			} else if idx := strings.Index(gitURL, ".com/"); idx >= 0 {
				parts := strings.Split(gitURL[idx+5:], "/")
				if len(parts) == 2 {
					tenant = parts[0]
					repo = parts[1]
				}
			}
		}
	}

	if tenant != "" && repo != "" {
		serverURL := os.Getenv("KAI_SERVER")
		if serverURL == "" {
			serverURL = remote.DefaultServer
		}
		remote.ForceSetRemote("origin", &remote.RemoteEntry{
			URL: serverURL, Tenant: tenant, Repo: repo,
		})
		fmt.Printf("Kai remote: %s/%s\n", tenant, repo)
	}

	fmt.Printf("\nDone! cd %s\n", dirName)
	return nil
}

func runCloneLegacy(cmd *cobra.Command, args []string) error {
	rawURL := args[0]
	tenant := cloneTenant
	repo := cloneRepo

	// Check if input is shorthand format: org/repo (no scheme)
	if !strings.Contains(rawURL, "://") && strings.Count(rawURL, "/") == 1 {
		// Shorthand format: org/repo — git clone via SSH, then set up kai
		parts := strings.Split(rawURL, "/")
		tenant = parts[0]
		repo = parts[1]

		dirName := repo
		if len(args) > 1 {
			dirName = args[1]
		}

		// Git clone — use the repo's git origin URL from the server,
		// or fall back to common providers
		fmt.Printf("Cloning %s/%s into '%s'...\n", tenant, repo, dirName)

		// Try to get git URL from the server's repo metadata
		// For now, try GitHub (most common for kaicontext repos)
		cloneURLs := []string{
			fmt.Sprintf("git@github.com:%s/%s.git", tenant, repo),
			fmt.Sprintf("https://github.com/%s/%s.git", tenant, repo),
			fmt.Sprintf("ssh://git@git.kaicontext.com:2222/%s/%s.git", tenant, repo),
		}

		var cloned bool
		for _, cloneURL := range cloneURLs {
			gitCmd := exec.Command("git", "clone", cloneURL, dirName)
			gitCmd.Stdout = os.Stdout
			gitCmd.Stderr = os.Stderr
			if err := gitCmd.Run(); err == nil {
				cloned = true
				break
			}
			os.RemoveAll(dirName) // clean up failed attempt
		}
		if !cloned {
			return fmt.Errorf("git clone failed — tried GitHub and Kai SSH")
		}

		// Checkout main branch if not already
		checkoutCmd := exec.Command("git", "checkout", "main")
		checkoutCmd.Dir = dirName
		checkoutCmd.Stdout = os.Stdout
		checkoutCmd.Stderr = os.Stderr
		checkoutCmd.Run() // best effort — main might not exist

		// Set up kai in the cloned directory
		origDir, _ := os.Getwd()
		os.Chdir(dirName)
		defer os.Chdir(origDir)

		// Build graph from the cloned files
		fmt.Println("Building semantic graph...")
		captureCmd := exec.Command("kai", "capture")
		captureCmd.Stdout = os.Stdout
		captureCmd.Stderr = os.Stderr
		if err := captureCmd.Run(); err != nil {
			fmt.Printf("Warning: kai capture failed: %v\n", err)
		}

		// Set up kai remote
		serverURL := os.Getenv("KAI_SERVER")
		if serverURL == "" {
			serverURL = remote.DefaultServer
		}
		entry := &remote.RemoteEntry{
			URL:    serverURL,
			Tenant: tenant,
			Repo:   repo,
		}
		remote.ForceSetRemote("origin", entry)

		fmt.Printf("\nDone! cd %s to start working.\n", dirName)
		return nil
	}

	// Full URL format: http://server/tenant/repo
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Extract tenant/repo from path if not specified via flags
	pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(pathParts) >= 2 && tenant == "" && repo == "" {
		tenant = pathParts[0]
		repo = pathParts[1]
		parsedURL.Path = ""
		rawURL = parsedURL.String()
	}

	// Validate we have tenant and repo
	if tenant == "" {
		return fmt.Errorf("tenant not specified (use --tenant or include in URL path)")
	}
	if repo == "" {
		return fmt.Errorf("repo not specified (use --repo or include in URL path)")
	}

	// Determine directory name
	dirName := repo
	if len(args) > 1 {
		dirName = args[1]
	}

	// Check if directory already exists
	if _, err := os.Stat(dirName); err == nil {
		return fmt.Errorf("directory '%s' already exists", dirName)
	}

	fmt.Printf("Cloning into '%s'...\n", dirName)

	// Create the directory
	if err := os.MkdirAll(dirName, 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	// Change to the new directory
	origDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	if err := os.Chdir(dirName); err != nil {
		return fmt.Errorf("changing to directory: %w", err)
	}
	defer os.Chdir(origDir)

	// Initialize Kai
	fmt.Println("Initializing Kai...")
	skipModulesFile = true
	if err := runInit(cmd, nil); err != nil {
		skipModulesFile = false
		os.Chdir(origDir)
		os.RemoveAll(dirName)
		return fmt.Errorf("initializing: %w", err)
	}
	skipModulesFile = false

	// Set up the remote
	fmt.Printf("Setting remote 'origin' to %s (tenant=%s, repo=%s)...\n", rawURL, tenant, repo)
	entry := &remote.RemoteEntry{
		URL:    rawURL,
		Tenant: tenant,
		Repo:   repo,
	}
	if err := remote.SetRemote("origin", entry); err != nil {
		os.Chdir(origDir)
		os.RemoveAll(dirName)
		return fmt.Errorf("setting remote: %w", err)
	}

	// Open database for fetch
	db, err := openDB()
	if err != nil {
		os.Chdir(origDir)
		os.RemoveAll(dirName)
		return err
	}
	defer db.Close()

	// Create client for remote
	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		os.Chdir(origDir)
		os.RemoveAll(dirName)
		return fmt.Errorf("creating client: %w", err)
	}

	// Check server health
	if err := client.Health(); err != nil {
		os.Chdir(origDir)
		os.RemoveAll(dirName)
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	fmt.Println("Fetching refs...")

	// Fetch all refs from remote
	remoteRefs, err := client.ListRefs("")
	if err != nil {
		fmt.Printf("Warning: could not list refs: %v\n", err)
		fmt.Println("Clone complete (empty repository).")
		return nil
	}

	if len(remoteRefs) == 0 {
		fmt.Println("Clone complete (empty repository).")
		return nil
	}

	fmt.Printf("  Found %d ref(s)\n", len(remoteRefs))

	// Collect objects to fetch
	var objectsToFetch [][]byte
	for _, r := range remoteRefs {
		exists, _ := db.HasNode(r.Target)
		if !exists {
			objectsToFetch = append(objectsToFetch, r.Target)
		}
	}

	if len(objectsToFetch) > 0 {
		fmt.Printf("  Fetching %d object(s)...\n", len(objectsToFetch))

		for _, digest := range objectsToFetch {
			content, kind, err := client.GetObject(digest)
			if err != nil {
				fmt.Printf("  Warning: failed to get object %s: %v\n", hex.EncodeToString(digest)[:12], err)
				continue
			}

			if content == nil {
				fmt.Printf("  Warning: object %s not found on server\n", hex.EncodeToString(digest)[:12])
				continue
			}

			var payload map[string]interface{}
			if err := json.Unmarshal(content, &payload); err != nil {
				fmt.Printf("  Warning: failed to parse object %s: %v\n", hex.EncodeToString(digest)[:12], err)
				continue
			}

			tx, err := db.BeginTx()
			if err != nil {
				fmt.Printf("  Warning: failed to start transaction: %v\n", err)
				continue
			}
			_, err = db.InsertNode(tx, graph.NodeKind(kind), payload)
			if err != nil {
				tx.Rollback()
				fmt.Printf("  Warning: failed to insert object %s: %v\n", hex.EncodeToString(digest)[:12], err)
				continue
			}
			tx.Commit()
		}
	}

	// Update local refs
	refMgr := ref.NewRefManager(db)
	for _, r := range remoteRefs {
		localName := fmt.Sprintf("remote/origin/%s", r.Name)
		kind := ref.KindSnapshot
		if strings.HasPrefix(r.Name, "cs.") {
			kind = ref.KindChangeSet
		} else if strings.HasPrefix(r.Name, "ws.") {
			kind = ref.KindWorkspace
		} else if strings.HasPrefix(r.Name, "tag.") {
			kind = ref.KindSnapshot
		}

		if err := refMgr.Set(localName, r.Target, kind); err != nil {
			continue
		}
		fmt.Printf("  %s -> %s\n", localName, hex.EncodeToString(r.Target)[:12])
	}

	fmt.Printf("\nClone complete. Repository cloned into '%s'\n", dirName)
	return nil
}

func runPrune(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Dry-run is the default unless --yes is specified
	// --dry-run can also be explicitly set
	isDryRun := !pruneYes || pruneDryRun

	opts := graph.GCOptions{
		SinceDays:  pruneSinceDays,
		Aggressive: pruneAggressive,
		DryRun:     isDryRun,
		Keep:       pruneKeep,
	}

	plan, err := db.BuildGCPlan(opts)
	if err != nil {
		return fmt.Errorf("building GC plan: %w", err)
	}

	// Show summary
	totalNodes := len(plan.NodesToDelete)
	if totalNodes == 0 && len(plan.ObjectsToDelete) == 0 {
		fmt.Println("Nothing to prune. All content is reachable.")
		return nil
	}

	if isDryRun {
		fmt.Println("Would sweep:")
	} else {
		fmt.Println("Sweeping:")
	}

	if plan.SnapshotCount > 0 {
		fmt.Printf("  Snapshots:  %d\n", plan.SnapshotCount)
	}
	if plan.ChangeSetCount > 0 {
		fmt.Printf("  ChangeSets: %d\n", plan.ChangeSetCount)
	}
	if plan.FileCount > 0 {
		fmt.Printf("  Files:      %d\n", plan.FileCount)
	}
	if plan.SymbolCount > 0 {
		fmt.Printf("  Symbols:    %d\n", plan.SymbolCount)
	}
	if plan.ModuleCount > 0 {
		fmt.Printf("  Modules:    %d\n", plan.ModuleCount)
	}

	if len(plan.ObjectsToDelete) > 0 {
		fmt.Printf("  Objects:    %d (~%.2f MiB)\n", len(plan.ObjectsToDelete), float64(plan.BytesReclaimed)/(1024*1024))
	}

	if isDryRun {
		fmt.Println("\nRun `kai prune --yes` to proceed.")
		return nil
	}

	// Actually execute
	if err := db.ExecuteGC(plan); err != nil {
		return fmt.Errorf("executing GC: %w", err)
	}

	fmt.Printf("\nPrune complete. Reclaimed %.2f MiB.\n", float64(plan.BytesReclaimed)/(1024*1024))

	// Clean up stale session refs (session.*.base) older than retention window (30 days)
	const sessionRefRetentionDays = 30
	refMgr := ref.NewRefManager(db)
	allRefs, _ := refMgr.List(nil)
	cutoff := time.Now().Add(-time.Duration(sessionRefRetentionDays) * 24 * time.Hour).UnixMilli()
	sessionRefsPruned := 0
	for _, r := range allRefs {
		if !strings.HasPrefix(r.Name, "session.") || !strings.HasSuffix(r.Name, ".base") {
			continue
		}
		if r.CreatedAt < cutoff {
			refMgr.Delete(r.Name)
			sessionRefsPruned++
		}
	}
	if sessionRefsPruned > 0 {
		fmt.Printf("Pruned %d stale session ref(s).\n", sessionRefsPruned)
	}

	return nil
}

func runPurge2(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	isDryRun := !purgeYes || purgeDryRun

	plan, err := db.BuildPurgePlan(args)
	if err != nil {
		return fmt.Errorf("building purge plan: %w", err)
	}

	if plan.FileCount == 0 {
		fmt.Println("No files matched. Nothing to purge.")
		return nil
	}

	if isDryRun {
		fmt.Println("Would purge from history:")
	} else {
		fmt.Println("Purging from history:")
	}

	fmt.Println()
	fmt.Printf("  Files:     %d\n", plan.FileCount)
	for _, p := range plan.Paths {
		fmt.Printf("             %s\n", p)
	}
	if plan.SnapshotCount > 0 {
		fmt.Printf("  Snapshots: %d (payload will be updated)\n", plan.SnapshotCount)
	}
	if plan.SymbolCount > 0 {
		fmt.Printf("  Symbols:   %d\n", plan.SymbolCount)
	}
	if len(plan.ObjectsToDelete) > 0 {
		fmt.Printf("  Objects:   %d (~%.2f MiB)\n", len(plan.ObjectsToDelete), float64(plan.BytesReclaimed)/(1024*1024))
	}

	if isDryRun {
		fmt.Printf("\nRun `kai purge %s --yes` to proceed.\nThis is irreversible.\n", args[0])
		return nil
	}

	if err := db.ExecutePurge(plan); err != nil {
		return fmt.Errorf("executing purge: %w", err)
	}

	fmt.Printf("\nPurge complete.")
	if plan.BytesReclaimed > 0 {
		fmt.Printf(" Reclaimed ~%.2f MiB.", float64(plan.BytesReclaimed)/(1024*1024))
	}
	fmt.Println()
	fmt.Println("Note: Run `kai push --force` to propagate to remote.")
	return nil
}

func runRemoteLog(cmd *cobra.Command, args []string) error {
	// Determine remote name
	remoteName := "origin"
	if len(args) > 0 {
		remoteName = args[0]
	}

	// Create client for remote
	client, err := remote.NewClientForRemote(remoteName)
	if err != nil {
		return fmt.Errorf("remote '%s' not configured (use 'kai remote set %s <url>')", remoteName, remoteName)
	}

	// Check server health
	if err := client.Health(); err != nil {
		return fmt.Errorf("cannot connect to %s: %w", client.BaseURL, err)
	}

	// Get log entries
	entries, err := client.GetLogEntries(remoteLogRef, 0, remoteLogLimit)
	if err != nil {
		return fmt.Errorf("getting log: %w", err)
	}

	if len(entries) == 0 {
		fmt.Println("No log entries found.")
		return nil
	}

	// Get current head
	head, _ := client.GetLogHead()

	fmt.Printf("Remote log from %s (%s)\n", remoteName, client.BaseURL)
	if head != nil {
		fmt.Printf("Head: %s\n", hex.EncodeToString(head)[:16])
	}
	fmt.Println()

	for _, e := range entries {
		timestamp := time.UnixMilli(e.Time).Format("2006-01-02 15:04:05")

		switch e.Kind {
		case "REF_UPDATE":
			oldStr := "(new)"
			if len(e.Old) > 0 {
				oldStr = hex.EncodeToString(e.Old)[:12]
			}
			newStr := hex.EncodeToString(e.New)[:12]
			fmt.Printf("%s  %-10s  %-20s  %s -> %s\n",
				timestamp, e.Actor, e.Ref, oldStr, newStr)
		case "NODE_PUBLISH":
			fmt.Printf("%s  %-10s  published %s (%s)\n",
				timestamp, e.Actor, hex.EncodeToString(e.NodeID)[:12], e.NodeKind)
		default:
			fmt.Printf("%s  %-10s  %s\n", timestamp, e.Actor, e.Kind)
		}
	}

	return nil
}

// Auth command implementations

func runAuthLogin(cmd *cobra.Command, args []string) error {
	var serverURL string

	if len(args) > 0 {
		serverURL = args[0]
	} else {
		// Try to get URL from origin remote
		entry, err := remote.GetRemote("origin")
		if err != nil {
			if authLoginToken == "" {
				return fmt.Errorf("no server URL provided and no 'origin' remote configured\n\nUsage: kai auth login <server-url>\nExample: kai auth login http://localhost:8080")
			}
			serverURL = remote.DefaultServer
		} else {
			serverURL = entry.URL
		}
	}

	// Non-interactive token login (for CI)
	if authLoginToken != "" {
		creds := &remote.Credentials{
			AccessToken: authLoginToken,
			ServerURL:   serverURL,
		}
		if err := remote.SaveCredentials(creds); err != nil {
			return fmt.Errorf("saving credentials: %w", err)
		}
		fmt.Printf("Authenticated with token to %s\n", serverURL)
		printDailyCapHint(serverURL)
		return nil
	}

	if err := remote.Login(serverURL); err != nil {
		return err
	}
	printDailyCapHint(serverURL)
	return nil
}

// printDailyCapHint surfaces the user's per-day kailab cost cap
// right after a successful login. This is the user's first
// encounter with the cap; printing it now eliminates the "wait,
// what cap?" support touch when they hit the 429 a week later.
//
// Best-effort: if the kailab call fails (server old enough not
// to ship /api/v1/usage/daily, network blip, etc.) we print
// nothing rather than a confusing partial line.
func printDailyCapHint(serverURL string) {
	tok, err := remote.GetValidAccessToken()
	if err != nil {
		return
	}
	ac := remote.NewAuthClient(serverURL)
	u, err := ac.GetDailyUsage(tok)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "  Daily kailab usage cap: $%.2f (resets midnight UTC)\n",
		float64(u.DailyCapCents)/100)
	fmt.Fprintf(os.Stderr, "  Run `kai auth status` to see current usage.\n")
}

func runAuthLogout(cmd *cobra.Command, args []string) error {
	if err := remote.Logout(); err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	fmt.Println("Logged out successfully.")
	return nil
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	// Two sections: the LLM provider (which may or may not be
	// kailab) and the kailab services (sync, gates, storage).
	// They're independent — a BYOM user might have one without
	// the other — so they need to be displayed as such instead of
	// being conflated under a single "logged in" / "not logged
	// in" status as the previous version did.
	printLLMProviderStatus()
	fmt.Println()
	printKailabServicesStatus()
	return nil
}

func printLLMProviderStatus() {
	// Resolve the same way the TUI does: kailab creds (if any) +
	// env vars decide. We don't need a working bearer here — just
	// the configured kind — so a missing login isn't an error.
	creds, _ := remote.LoadCredentials()
	var kailabBase, kailabToken string
	if creds != nil {
		kailabBase = creds.ServerURL
		kailabToken = creds.AccessToken // raw, not refreshed; presence is enough
	}
	cfg := provider.FromEnv(kailabBase, kailabToken, "z-ai/glm-5.1")

	fmt.Println("LLM provider:")
	switch cfg.Kind {
	case provider.KindAnthropic:
		fmt.Println("  Kind:    anthropic (direct)")
		fmt.Printf("  Key:     %s\n", maskKey("ANTHROPIC_API_KEY", cfg.AuthToken))
		fmt.Printf("  Model:   %s\n", cfg.Model)
		fmt.Println("  Cache:   supported")
	case provider.KindOpenAI:
		fmt.Println("  Kind:    openai (direct)")
		base := cfg.BaseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		fmt.Printf("  URL:     %s\n", base)
		if cfg.AuthToken == "" {
			fmt.Println("  Key:     (none — local server)")
		} else {
			fmt.Printf("  Key:     %s\n", maskKey("OPENAI_API_KEY", cfg.AuthToken))
		}
		fmt.Printf("  Model:   %s\n", cfg.Model)
		fmt.Println("  Cache:   not supported (OpenAI protocol)")
	default:
		fmt.Println("  Kind:    kailab (proxied)")
		if kailabBase == "" {
			fmt.Println("  Status:  not configured (run `kai auth login`)")
		} else {
			fmt.Printf("  Server:  %s\n", kailabBase)
		}
		// No single "Model:" line here: kailab routes each kind of
		// work (planner / chat / executor / classifier / review) to a
		// different model, so naming one would misrepresent what runs.
		fmt.Println("  Models:  per-role (planner, chat, executor) — see `kai config`")
		fmt.Println("  Cache:   supported (server-side)")
		// Daily-cap snapshot. Best-effort: if the kailab call
		// fails (offline, server old enough not to ship the
		// endpoint, expired token) we omit the line rather than
		// blocking auth status. The spec wants visibility when
		// possible; "I can't tell you the cap right now" is
		// fine, "auth status crashed" is not.
		if kailabBase != "" {
			if tok, terr := remote.GetValidAccessToken(); terr == nil {
				ac := remote.NewAuthClient(kailabBase)
				if u, uerr := ac.GetDailyUsage(tok); uerr == nil {
					fmt.Printf("  Daily:   $%.2f / $%.2f used today (resets midnight UTC)\n",
						float64(u.DailyCostCents)/100, float64(u.DailyCapCents)/100)
				}
			}
		}
	}
}

func printKailabServicesStatus() {
	email, serverURL, loggedIn := remote.GetAuthStatus()
	fmt.Println("Kailab services (sync, shared gates, repo storage):")
	if !loggedIn {
		fmt.Println("  Status:  not authenticated")
		fmt.Println("  Note:    sync and team gates unavailable")
		fmt.Println("           run `kai auth login` to enable")
		return
	}
	if _, err := remote.GetValidAccessToken(); err != nil {
		fmt.Printf("  Account: %s\n", email)
		fmt.Printf("  Server:  %s\n", serverURL)
		fmt.Println("  Status:  token invalid or expired")
		fmt.Println("           run `kai auth login` to re-authenticate")
		return
	}
	fmt.Printf("  Account: %s\n", email)
	fmt.Printf("  Server:  %s\n", serverURL)
	fmt.Println("  Status:  authenticated")
}

// maskKey shows the env-var name plus the last 4 characters of the
// secret so the user can confirm "yes that's the key I set" without
// the full value landing in their terminal scrollback.
func maskKey(envName, val string) string {
	if val == "" {
		return envName + " (not set)"
	}
	tail := val
	if len(val) > 4 {
		tail = val[len(val)-4:]
	}
	return fmt.Sprintf("%s (set, ...%s)", envName, tail)
}

func runUsage(cmd *cobra.Command, args []string) error {
	client, err := remote.NewClientForRemote("origin")
	if err != nil {
		return fmt.Errorf("no remote configured (use 'kai remote set origin <url>')")
	}

	usage, err := client.GetUsage(client.Tenant)
	if err != nil {
		return fmt.Errorf("getting usage: %w", err)
	}

	fmt.Println()
	fmt.Printf("  Org:     %s\n", client.Tenant)
	fmt.Printf("  Plan:    %s\n", usage.Tier)
	fmt.Printf("  Period:  %s\n", usage.Period)
	fmt.Println()

	pct := 0
	if usage.CommitsLimit > 0 {
		pct = usage.CommitsUsed * 100 / usage.CommitsLimit
	}

	if usage.CommitsLimit < 0 {
		// Pro = unlimited commits. Render the count, omit the
		// progress bar (no denominator to compare against).
		fmt.Printf("  Commits: %d (unlimited on Pro)\n", usage.CommitsUsed)
	} else {
		bar := renderUsageBar(pct, 30)
		fmt.Printf("  Commits: %d / %d  %s  %d%%\n", usage.CommitsUsed, usage.CommitsLimit, bar, pct)
	}

	if usage.UpgradeURL != nil {
		fmt.Println()
		fmt.Printf("  Upgrade to Pro: %s\n", *usage.UpgradeURL)
	}

	fmt.Println()
	return nil
}

func renderUsageBar(pct, width int) string {
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	empty := width - filled

	bar := strings.Repeat("=", filled) + strings.Repeat("-", empty)

	// Color based on usage
	if pct >= 90 {
		return "\033[31m[" + bar + "]\033[0m" // red
	} else if pct >= 80 {
		return "\033[33m[" + bar + "]\033[0m" // yellow
	}
	return "\033[32m[" + bar + "]\033[0m" // green
}

// Review command implementations

func runReviewOpen(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var targetID []byte
	var targetKind string

	if len(args) == 0 {
		// Check if there's a current workspace
		currentWsName, err := getCurrentWorkspace()
		if err != nil {
			return fmt.Errorf("checking current workspace: %w", err)
		}

		if currentWsName != "" {
			// Use the current workspace
			wsMgr := workspace.NewManager(db)
			ws, err := wsMgr.Get(currentWsName)
			if err != nil {
				return fmt.Errorf("getting workspace %q: %w", currentWsName, err)
			}
			if ws == nil {
				return fmt.Errorf("workspace %q not found (clear with 'kai ws checkout')", currentWsName)
			}

			// Create changeset from workspace's head to latest snapshot
			// Try @snap:working first, fall back to @snap:last
			headSnapID, err := resolveSnapshotID(db, "@snap:working")
			if err != nil {
				headSnapID, err = resolveSnapshotID(db, "@snap:last")
				if err != nil {
					return fmt.Errorf("no snapshot found — run 'kai capture' first")
				}
			}

			// Check if there are changes
			if string(ws.HeadSnapshot) == string(headSnapID) {
				return fmt.Errorf("no changes to review in workspace %q\n  Make changes and run 'kai capture' to capture them", currentWsName)
			}

			fmt.Printf("Using workspace: %s\n", currentWsName)
			fmt.Printf("Creating changeset from workspace head to @snap:working...\n")

			changeSetID, err := createChangesetFromSnapshots(db, ws.HeadSnapshot, headSnapID, reviewTitle)
			if err != nil {
				return fmt.Errorf("creating changeset: %w", err)
			}
			fmt.Printf("Changeset created: %s\n", util.BytesToHex(changeSetID)[:12])

			// Add changeset to workspace
			if err := wsMgr.AddChangeSet(ws.ID, changeSetID); err != nil {
				return fmt.Errorf("adding changeset to workspace: %w", err)
			}

			// Update workspace head
			if err := wsMgr.UpdateHead(ws.ID, headSnapID); err != nil {
				return fmt.Errorf("updating workspace head: %w", err)
			}

			// Review targets the workspace (which contains the changeset stack)
			targetID = ws.ID
			targetKind = string(ref.KindWorkspace)
			fmt.Printf("Changeset added to workspace stack\n\n")
		} else {
			// No workspace - auto-create standalone changeset from base to snap.latest
			// Determine base: --base flag > @snap:prev (previous snapshot)
			var baseSnapID []byte
			var baseLabel string

			if reviewBase != "" {
				// User specified --base
				baseSnapID, err = resolveSnapshotID(db, reviewBase)
				if err != nil {
					return fmt.Errorf("resolving --base %q: %w", reviewBase, err)
				}
				baseLabel = reviewBase
			} else {
				// Try session base first: find oldest session.*.base ref
				refMgr := ref.NewRefManager(db)
				allRefs, _ := refMgr.List(nil)
				var sessionBaseRef *ref.Ref
				for _, r := range allRefs {
					if !strings.HasPrefix(r.Name, "session.") || !strings.HasSuffix(r.Name, ".base") {
						continue
					}
					if sessionBaseRef == nil || r.CreatedAt < sessionBaseRef.CreatedAt {
						sessionBaseRef = r
					}
				}

				if sessionBaseRef != nil {
					baseSnapID = sessionBaseRef.TargetID
					baseLabel = sessionBaseRef.Name
				} else {
					// Fall back to @snap:prev
					baseSnapID, err = resolveSnapshotID(db, "@snap:prev")
					if err != nil {
						return fmt.Errorf("resolving @snap:prev: %w (need at least 2 snapshots, run 'kai capture' twice)", err)
					}
					baseLabel = "@snap:prev"
					fmt.Fprintf(os.Stderr, "Note: no session base found, using %s as review base\n", baseLabel)
				}
			}

			// Use snap.latest as head (updated by kai capture)
			headSnapID, err := resolveSnapshotID(db, "snap.latest")
			if err != nil {
				return fmt.Errorf("resolving snap.latest: %w (run 'kai capture' to capture your changes)", err)
			}

			// Check if baseline and head are the same (no changes)
			if string(baseSnapID) == string(headSnapID) {
				return fmt.Errorf("no changes to review: %s and snap.latest are the same\n  Make changes and run 'kai capture' to capture them", baseLabel)
			}

			fmt.Printf("Creating changeset from %s to snap.latest...\n", baseLabel)
			changeSetID, err := createChangesetFromSnapshots(db, baseSnapID, headSnapID, reviewTitle)
			if err != nil {
				return fmt.Errorf("creating changeset: %w", err)
			}

			targetID = changeSetID
			targetKind = string(ref.KindChangeSet)
			fmt.Printf("Changeset created: %s\n", util.BytesToHex(changeSetID)[:12])
			fmt.Println()
		}
	} else {
		// Resolve the target (changeset or workspace)
		resolver := ref.NewResolver(db)
		result, err := resolver.Resolve(args[0], nil)
		if err != nil {
			return fmt.Errorf("resolving target: %w", err)
		}

		// Validate target kind
		if result.Kind != ref.KindChangeSet && result.Kind != ref.KindWorkspace {
			return fmt.Errorf("target must be a changeset or workspace, got %s", result.Kind)
		}

		targetID = result.ID
		targetKind = string(result.Kind)
	}

	// Get author (use system user for now)
	author := os.Getenv("USER")
	if author == "" {
		author = "unknown"
	}

	// Auto-generate title from intent if not provided
	autoTitle := reviewTitle == ""
	if autoTitle {
		gen := intent.NewGenerator(db)
		generatedTitle, err := gen.RenderIntent(targetID, "", false)
		if err != nil {
			// Fall back to generic title if intent generation fails
			generatedTitle = "Review of changes"
		}
		reviewTitle = generatedTitle
		fmt.Printf("Auto-generated title: %s\n", reviewTitle)
	}

	// Show explain if requested
	if reviewExplain {
		targetRef := "@cs:last"
		if len(args) > 0 {
			targetRef = args[0]
		}
		// Check if we have a workspace
		currentWsName, _ := getCurrentWorkspace()
		hasWorkspace := currentWsName != "" && len(args) == 0
		ctx := explain.ExplainReviewOpenFull(targetRef, reviewTitle, hasWorkspace, currentWsName, autoTitle)
		ctx.Print(os.Stdout)
	}

	mgr := review.NewManager(db)
	rev, err := mgr.Open(targetID, reviewTitle, reviewDesc, author, reviewReviewers, nil)
	if err != nil {
		return fmt.Errorf("opening review: %w", err)
	}

	reviewID := review.IDToHex(rev.ID)[:12]
	fmt.Printf("Review opened: %s\n", reviewID)
	fmt.Printf("Title:         %s\n", rev.Title)
	fmt.Printf("State:         %s\n", rev.State)
	fmt.Printf("Target:        %s (%s)\n", util.BytesToHex(rev.TargetID)[:12], targetKind)
	if len(rev.Reviewers) > 0 {
		fmt.Printf("Reviewers:     %s\n", strings.Join(rev.Reviewers, ", "))
	}

	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  kai review view %s       # View the review\n", reviewID)
	fmt.Printf("  kai review approve %s    # Approve the review\n", reviewID)
	fmt.Printf("  kai review close %s      # Close (--state merged|abandoned)\n", reviewID)
	fmt.Println()
	fmt.Println("Other commands:")
	fmt.Println("  kai ci plan              # See which tests to run")
	fmt.Println("  kai review export <id>   # Export as markdown/HTML")

	// Clean up session refs covered by this review.
	// Session refs whose CreatedAt is ≤ now are considered reviewed.
	// This makes "oldest remaining session ref" = "oldest unreviewed."
	refMgr := ref.NewRefManager(db)
	allRefs, _ := refMgr.List(nil)
	for _, r := range allRefs {
		if strings.HasPrefix(r.Name, "session.") && strings.HasSuffix(r.Name, ".base") {
			refMgr.Delete(r.Name)
		}
	}

	return nil
}

func runReviewList(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	reviews, err := mgr.List()
	if err != nil {
		return fmt.Errorf("listing reviews: %w", err)
	}

	if len(reviews) == 0 {
		fmt.Println("No reviews found.")
		return nil
	}

	fmt.Printf("%-12s  %-10s  %-30s  %s\n", "ID", "STATE", "TITLE", "TARGET")
	fmt.Println(strings.Repeat("-", 80))

	for _, r := range reviews {
		title := r.Title
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		fmt.Printf("%-12s  %-10s  %-30s  %s\n",
			review.IDToHex(r.ID)[:12],
			r.State,
			title,
			util.BytesToHex(r.TargetID)[:12])
	}

	return nil
}

func runReviewView(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	// Get changeset data if target is a changeset
	var csNode *graph.Node
	var baseSnapID, headSnapID []byte
	var fileChanges []fileChangeInfo
	var symbolChanges []symbolChangeInfo
	var changeTypes []string

	if rev.TargetKind == graph.KindChangeSet {
		csNode, _ = db.GetNode(rev.TargetID)
		if csNode != nil {
			// Extract base and head snapshot IDs
			if baseHex, ok := csNode.Payload["base"].(string); ok {
				baseSnapID, _ = util.HexToBytes(baseHex)
			}
			if headHex, ok := csNode.Payload["head"].(string); ok {
				headSnapID, _ = util.HexToBytes(headHex)
			}

			// Collect file and symbol changes
			csData, err := db.GetAllNodesAndEdgesForChangeSet(rev.TargetID)
			if err == nil {
				if nodes, ok := csData["nodes"].([]map[string]interface{}); ok {
					for _, node := range nodes {
						kind, _ := node["kind"].(string)
						payload, _ := node["payload"].(map[string]interface{})
						nodeID, _ := node["id"].(string)

						switch kind {
						case "File":
							// Deduplicate by node ID
							isDuplicateFile := false
							for _, existing := range fileChanges {
								if existing.ID == nodeID {
									isDuplicateFile = true
									break
								}
							}
							if !isDuplicateFile {
								path, _ := payload["path"].(string)
								digest, _ := payload["digest"].(string)
								fileChanges = append(fileChanges, fileChangeInfo{
									ID:     nodeID,
									Path:   path,
									Digest: digest,
								})
							}
						case "Symbol":
							// Deduplicate by node ID
							isDuplicate := false
							for _, existing := range symbolChanges {
								if existing.ID == nodeID {
									isDuplicate = true
									break
								}
							}
							if !isDuplicate {
								fqName, _ := payload["fqName"].(string)
								symKind, _ := payload["kind"].(string)
								sig, _ := payload["signature"].(string)
								symbolChanges = append(symbolChanges, symbolChangeInfo{
									ID:        nodeID,
									FQName:    fqName,
									Kind:      symKind,
									Signature: sig,
								})
							}
						case "ChangeType":
							if category, ok := payload["category"].(string); ok {
								changeTypes = append(changeTypes, category)
							}
						}
					}
				}
			}
		}
	}

	// JSON output
	if reviewJSON {
		data := map[string]interface{}{
			"id":          review.IDToHex(rev.ID),
			"title":       rev.Title,
			"description": rev.Description,
			"state":       rev.State,
			"author":      rev.Author,
			"reviewers":   rev.Reviewers,
			"targetId":    util.BytesToHex(rev.TargetID),
			"targetKind":  rev.TargetKind,
			"createdAt":   rev.CreatedAt,
			"updatedAt":   rev.UpdatedAt,
		}

		if csNode != nil {
			// Build semantic hunks for JSON
			var units []map[string]interface{}
			for _, sym := range symbolChanges {
				units = append(units, map[string]interface{}{
					"kind":   sym.Kind,
					"fqName": sym.FQName,
					"after":  map[string]interface{}{"signature": sym.Signature},
				})
			}

			var files []map[string]interface{}
			for _, f := range fileChanges {
				// Get file content if available
				var afterContent string
				if f.Digest != "" {
					if content, err := db.ReadObject(f.Digest); err == nil {
						afterContent = string(content)
					}
				}
				files = append(files, map[string]interface{}{
					"path":  f.Path,
					"after": afterContent,
				})
			}

			data["units"] = units
			data["files"] = files
			data["changeTypes"] = changeTypes
			if csNode.Payload["intent"] != nil {
				data["intent"] = csNode.Payload["intent"]
			}
		}

		output, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(output))
		return nil
	}

	// Build semantic diff from changeset
	var semanticDiff *semanticdiff.SemanticDiff
	if csNode != nil {
		semanticDiff, _ = semanticdiff.FromChangeSet(db, csNode)
	}

	// Progressive disclosure summary mode (default)
	if reviewSummary && semanticDiff != nil {
		summary := review.BuildReviewSummary(semanticDiff, loadModuleCategorizer())
		if rev.Title != "" {
			summary.Title = rev.Title
		}
		summary.Description = rev.Description

		// Show the summary
		fmt.Println(summary.FormatSummary())

		// Show review metadata
		fmt.Println("─────────────────────────────────────────")
		fmt.Printf("Review:    %s\n", review.IDToHex(rev.ID)[:12])
		fmt.Printf("State:     %s\n", rev.State)
		fmt.Printf("Author:    %s\n", rev.Author)
		if len(rev.Reviewers) > 0 {
			fmt.Printf("Reviewers: %s\n", strings.Join(rev.Reviewers, ", "))
		}
		fmt.Println()

		// Interactive mode: drill down into changes
		if reviewInteractive && len(summary.Changes) > 0 {
			fmt.Println("Enter a number to inspect a change group, or 'q' to quit:")
			reader := bufio.NewReader(os.Stdin)
			for {
				fmt.Print("> ")
				input, _ := reader.ReadString('\n')
				input = strings.TrimSpace(input)

				if input == "q" || input == "quit" || input == "" {
					break
				}

				idx, err := strconv.Atoi(input)
				if err != nil || idx < 1 || idx > len(summary.Changes) {
					fmt.Printf("Enter a number between 1 and %d\n", len(summary.Changes))
					continue
				}

				// Show detailed view for this change group
				fmt.Println()
				fmt.Println(summary.FormatChange(idx - 1))
				fmt.Println()
				fmt.Println("Enter another number, or 'q' to quit:")
			}
		}

		return nil
	}

	// Fallback: original text output if no semantic diff
	fmt.Printf("Review: %s\n", review.IDToHex(rev.ID)[:12])
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Title:       %s\n", rev.Title)
	if rev.Description != "" {
		fmt.Printf("Description: %s\n", rev.Description)
	}
	fmt.Printf("State:       %s\n", rev.State)
	fmt.Printf("Author:      %s\n", rev.Author)
	if len(rev.Reviewers) > 0 {
		fmt.Printf("Reviewers:   %s\n", strings.Join(rev.Reviewers, ", "))
	}
	fmt.Printf("Target:      %s (%s)\n", util.BytesToHex(rev.TargetID)[:12], rev.TargetKind)
	fmt.Printf("Created:     %s\n", time.UnixMilli(rev.CreatedAt).Format(time.RFC3339))
	fmt.Printf("Updated:     %s\n", time.UnixMilli(rev.UpdatedAt).Format(time.RFC3339))

	// Show intent
	if csNode != nil {
		if intentStr, ok := csNode.Payload["intent"].(string); ok && intentStr != "" {
			fmt.Println()
			fmt.Println("Intent:")
			fmt.Printf("  %s\n", intentStr)
		}
	}

	// Show change types grouped into 3 buckets
	if len(changeTypes) > 0 {
		fmt.Println()
		fmt.Println("Changes:")

		// Group raw categories into buckets
		bucketCounts := make(map[ChangeBucket]int)
		for _, ct := range changeTypes {
			bucket := categorizeToBucket(ct)
			bucketCounts[bucket]++
		}

		// Display in consistent order
		bucketOrder := []ChangeBucket{BucketStructural, BucketBehavioral, BucketAPIContract}
		for _, bucket := range bucketOrder {
			count := bucketCounts[bucket]
			if count > 0 {
				fmt.Printf("  • %s: %d\n", bucket, count)
			}
		}
	}

	// Show diffs based on view mode
	showSemantic := reviewViewMode == "semantic" || reviewViewMode == "mixed"
	showText := reviewViewMode == "text" || reviewViewMode == "mixed"

	if showSemantic && len(symbolChanges) > 0 {
		fmt.Println()
		fmt.Println("Semantic Diff:")
		fmt.Println(strings.Repeat("-", 60))

		// Group symbols by file path (approximate from fqName)
		for _, sym := range symbolChanges {
			fmt.Printf("\n  %s %s\n", sym.Kind, sym.FQName)
			if sym.Signature != "" {
				fmt.Printf("    + %s\n", sym.Signature)
			}
		}
	}

	if showText && len(fileChanges) > 0 {
		fmt.Println()
		fmt.Println("File Contents:")
		fmt.Println(strings.Repeat("-", 60))

		snapshotCreator := snapshot.NewCreator(db, nil)

		for _, f := range fileChanges {
			fmt.Printf("\n--- %s\n", f.Path)

			// Get before content from base snapshot
			var beforeContent, afterContent string

			if baseSnapID != nil {
				beforeContent = getFileContentFromSnapshot(db, snapshotCreator, baseSnapID, f.Path)
			}
			if headSnapID != nil {
				afterContent = getFileContentFromSnapshot(db, snapshotCreator, headSnapID, f.Path)
			}

			// Show unified diff
			if beforeContent == "" && afterContent != "" {
				// New file
				fmt.Println("+++ (new file)")
				lines := strings.Split(afterContent, "\n")
				for i, line := range lines {
					if i < 20 { // Limit preview
						fmt.Printf("+ %s\n", line)
					}
				}
				if len(lines) > 20 {
					fmt.Printf("  ... (%d more lines)\n", len(lines)-20)
				}
			} else if beforeContent != "" && afterContent == "" {
				// Deleted file
				fmt.Println("--- (deleted)")
			} else if beforeContent != afterContent {
				// Modified - show unified diff
				fmt.Println("+++ (modified)")
				showUnifiedDiff(beforeContent, afterContent)
			} else {
				fmt.Println("  (unchanged)")
			}
		}
	}

	return nil
}

// Helper types for review view
type fileChangeInfo struct {
	ID     string
	Path   string
	Digest string
}

type symbolChangeInfo struct {
	ID        string
	FQName    string
	Kind      string
	Signature string
}

// getFileContentFromSnapshot retrieves file content from a snapshot by path
func getFileContentFromSnapshot(db *graph.DB, sc *snapshot.Creator, snapID []byte, path string) string {
	files, err := sc.GetSnapshotFiles(snapID)
	if err != nil {
		return ""
	}

	for _, f := range files {
		if fPath, ok := f.Payload["path"].(string); ok && fPath == path {
			if digest, ok := f.Payload["digest"].(string); ok {
				content, err := db.ReadObject(digest)
				if err == nil {
					return string(content)
				}
			}
		}
	}
	return ""
}

// showUnifiedDiff displays a unified diff using pure Go (no system dependency)
func showUnifiedDiff(before, after string) {
	dmp := diffmatchpatch.New()

	// Convert to line-based diff for better unified output
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")

	// Use line mode for cleaner diffs
	chars1, chars2, lineArray := dmp.DiffLinesToChars(before, after)
	diffs := dmp.DiffMain(chars1, chars2, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)
	diffs = dmp.DiffCleanupSemantic(diffs)

	// ANSI color codes
	const (
		colorReset = "\033[0m"
		colorRed   = "\033[31m"
		colorGreen = "\033[32m"
		colorCyan  = "\033[36m"
	)

	// Track line numbers for hunk headers
	oldLine := 1
	newLine := 1

	// Collect hunks
	type hunk struct {
		oldStart, oldCount int
		newStart, newCount int
		lines              []string
	}
	var hunks []hunk
	var currentHunk *hunk

	for _, d := range diffs {
		lines := strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n")
		if d.Text == "" {
			continue
		}

		switch d.Type {
		case diffmatchpatch.DiffEqual:
			// Context lines - start new hunk if needed, include up to 3 lines
			contextLines := lines
			if len(contextLines) > 6 && currentHunk != nil {
				// End current hunk with 3 trailing context lines
				for i := 0; i < 3 && i < len(contextLines); i++ {
					currentHunk.lines = append(currentHunk.lines, " "+contextLines[i])
					currentHunk.oldCount++
					currentHunk.newCount++
				}
				hunks = append(hunks, *currentHunk)
				currentHunk = nil
				// Skip middle, advance line counters
				oldLine += len(contextLines) - 3
				newLine += len(contextLines) - 3
				contextLines = contextLines[len(contextLines)-3:]
			}
			if currentHunk != nil {
				for _, line := range contextLines {
					currentHunk.lines = append(currentHunk.lines, " "+line)
					currentHunk.oldCount++
					currentHunk.newCount++
				}
			}
			oldLine += len(lines)
			newLine += len(lines)

		case diffmatchpatch.DiffDelete:
			if currentHunk == nil {
				// Start new hunk with up to 3 lines of previous context
				currentHunk = &hunk{oldStart: oldLine, newStart: newLine}
			}
			for _, line := range lines {
				currentHunk.lines = append(currentHunk.lines, colorRed+"-"+line+colorReset)
				currentHunk.oldCount++
			}
			oldLine += len(lines)

		case diffmatchpatch.DiffInsert:
			if currentHunk == nil {
				currentHunk = &hunk{oldStart: oldLine, newStart: newLine}
			}
			for _, line := range lines {
				currentHunk.lines = append(currentHunk.lines, colorGreen+"+"+line+colorReset)
				currentHunk.newCount++
			}
			newLine += len(lines)
		}
	}

	// Flush last hunk
	if currentHunk != nil {
		hunks = append(hunks, *currentHunk)
	}

	// Print hunks
	for _, h := range hunks {
		// Print hunk header
		fmt.Printf("%s@@ -%d,%d +%d,%d @@%s\n", colorCyan, h.oldStart, h.oldCount, h.newStart, h.newCount, colorReset)
		for _, line := range h.lines {
			fmt.Println(line)
		}
	}

	// Handle edge case: no differences
	if len(hunks) == 0 && len(beforeLines) != len(afterLines) {
		// Fallback for edge cases
		fmt.Printf("%s@@ -1,%d +1,%d @@%s\n", colorCyan, len(beforeLines), len(afterLines), colorReset)
	}
}

func runReviewStatus(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("Review %s: %s\n", review.IDToHex(rev.ID)[:12], rev.State)
	return nil
}

func runReviewApprove(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	if err := mgr.Approve(rev.ID); err != nil {
		return fmt.Errorf("approving review: %w", err)
	}

	fmt.Printf("Review %s approved.\n", review.IDToHex(rev.ID)[:12])
	return nil
}

func runReviewRequestChanges(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	if err := mgr.RequestChanges(rev.ID, "", ""); err != nil {
		return fmt.Errorf("requesting changes: %w", err)
	}

	fmt.Printf("Review %s: changes requested.\n", review.IDToHex(rev.ID)[:12])
	return nil
}

func runReviewClose(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	state := review.State(reviewCloseState)
	if state != review.StateMerged && state != review.StateAbandoned {
		return fmt.Errorf("--state must be 'merged' or 'abandoned'")
	}

	if err := mgr.Close(rev.ID, state); err != nil {
		return fmt.Errorf("closing review: %w", err)
	}

	// If merging, update snap.main to the changeset's head snapshot
	if state == review.StateMerged && rev.TargetKind == graph.KindChangeSet {
		target, err := mgr.GetTarget(rev.ID)
		if err == nil && target != nil {
			if headHex, ok := target.Payload["head"].(string); ok && headHex != "" {
				headID, err := util.HexToBytes(headHex)
				if err == nil {
					refMgr := ref.NewRefManager(db)
					if err := refMgr.Set("snap.main", headID, ref.KindSnapshot); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not update snap.main: %v\n", err)
					} else {
						fmt.Printf("Updated snap.main to merged head.\n")
					}
				}
			}
		}
	}

	fmt.Printf("Review %s closed as %s.\n", review.IDToHex(rev.ID)[:12], state)
	return nil
}

func runReviewEdit(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	var title, desc *string
	if cmd.Flags().Changed("title") {
		title = &reviewEditTitle
	}
	if cmd.Flags().Changed("desc") {
		desc = &reviewEditDesc
	}

	var assignees []string
	if cmd.Flags().Changed("assignees") {
		assignees = reviewEditAssignees
	}

	if title == nil && desc == nil && assignees == nil {
		return fmt.Errorf("nothing to update (use --title, --desc, or --assignees)")
	}

	if err := mgr.Update(rev.ID, title, desc, assignees); err != nil {
		return fmt.Errorf("updating review: %w", err)
	}

	fmt.Printf("Review %s updated.\n", review.IDToHex(rev.ID)[:12])
	return nil
}

func runReviewReady(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	if err := mgr.MarkReady(rev.ID); err != nil {
		return fmt.Errorf("marking ready: %w", err)
	}

	fmt.Printf("Review %s is now open for review.\n", review.IDToHex(rev.ID)[:12])
	return nil
}

func runReviewComment(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	author := os.Getenv("USER")
	if author == "" {
		author = "unknown"
	}

	var anchor *review.CommentAnchor
	if reviewCommentFile != "" {
		anchor = &review.CommentAnchor{
			FilePath: reviewCommentFile,
			Line:     reviewCommentLine,
		}
	}

	comment, err := mgr.AddComment(rev.ID, author, reviewCommentBody, "", anchor)
	if err != nil {
		return fmt.Errorf("adding comment: %w", err)
	}

	fmt.Printf("Comment added to review %s\n", review.IDToHex(rev.ID)[:12])
	if comment.FilePath != "" {
		if comment.Line > 0 {
			fmt.Printf("  Anchored to %s:%d\n", comment.FilePath, comment.Line)
		} else {
			fmt.Printf("  Anchored to %s\n", comment.FilePath)
		}
	}

	return nil
}

func runReviewComments(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	comments, err := mgr.ListComments(rev.ID)
	if err != nil {
		return fmt.Errorf("listing comments: %w", err)
	}

	if len(comments) == 0 {
		fmt.Printf("No comments on review %s\n", review.IDToHex(rev.ID)[:12])
		return nil
	}

	fmt.Printf("Comments on review %s (%d):\n\n", review.IDToHex(rev.ID)[:12], len(comments))
	for _, c := range comments {
		t := time.UnixMilli(c.CreatedAt).Format("2006-01-02 15:04")
		if c.FilePath != "" {
			if c.Line > 0 {
				fmt.Printf("  [%s] %s on %s:%d\n", t, c.Author, c.FilePath, c.Line)
			} else {
				fmt.Printf("  [%s] %s on %s\n", t, c.Author, c.FilePath)
			}
		} else {
			fmt.Printf("  [%s] %s\n", t, c.Author)
		}
		fmt.Printf("    %s\n\n", c.Body)
	}

	return nil
}

func runReviewExport(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}

	// Get target for more context
	target, _ := mgr.GetTarget(rev.ID)

	if reviewExportMD || (!reviewExportMD && !reviewExportHTML) {
		// Default to markdown
		fmt.Printf("# %s\n\n", rev.Title)
		if rev.Description != "" {
			fmt.Printf("%s\n\n", rev.Description)
		}
		fmt.Printf("**State:** %s  \n", rev.State)
		fmt.Printf("**Author:** %s  \n", rev.Author)
		if len(rev.Reviewers) > 0 {
			fmt.Printf("**Reviewers:** %s  \n", strings.Join(rev.Reviewers, ", "))
		}
		fmt.Printf("**Target:** `%s` (%s)  \n\n", util.BytesToHex(rev.TargetID)[:12], rev.TargetKind)

		// If target is a changeset, show details
		if target != nil && target.Kind == graph.KindChangeSet {
			if intentStr, ok := target.Payload["intent"].(string); ok && intentStr != "" {
				fmt.Printf("## Intent\n\n%s\n\n", intentStr)
			}

			// Show affected symbols if available
			edges, _ := db.GetEdges(target.ID, graph.EdgeAffects)
			if len(edges) > 0 {
				fmt.Println("## Affected Symbols")
				fmt.Println()
				for _, edge := range edges {
					sym, _ := db.GetNode(edge.Dst)
					if sym != nil {
						name, _ := sym.Payload["name"].(string)
						kind, _ := sym.Payload["kind"].(string)
						fmt.Printf("- `%s` (%s)\n", name, kind)
					}
				}
				fmt.Println()
			}
		}

		fmt.Println("---")
		fmt.Println("*Generated by [Kai](https://github.com/kaicontext/kai-cli)*")
		return nil
	}

	if reviewExportHTML {
		fmt.Println("<!DOCTYPE html>")
		fmt.Println("<html><head><title>Review: " + rev.Title + "</title>")
		fmt.Println("<style>body{font-family:sans-serif;max-width:800px;margin:0 auto;padding:20px}</style>")
		fmt.Println("</head><body>")
		fmt.Printf("<h1>%s</h1>\n", rev.Title)
		if rev.Description != "" {
			fmt.Printf("<p>%s</p>\n", rev.Description)
		}
		fmt.Printf("<p><strong>State:</strong> %s</p>\n", rev.State)
		fmt.Printf("<p><strong>Author:</strong> %s</p>\n", rev.Author)
		fmt.Printf("<p><strong>Target:</strong> <code>%s</code> (%s)</p>\n", util.BytesToHex(rev.TargetID)[:12], rev.TargetKind)
		fmt.Println("</body></html>")
	}

	return nil
}

func runReviewSummary(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Resolve changeset selector
	var csID []byte
	selector := "@cs:last"
	if len(args) > 0 {
		selector = args[0]
	}

	// Parse the selector
	resolver := ref.NewResolver(db)
	resolved, err := resolver.Resolve(selector, nil)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", selector, err)
	}
	if resolved == nil {
		return fmt.Errorf("changeset not found: %s", selector)
	}
	csID = resolved.ID

	// Get the changeset node
	csNode, err := db.GetNode(csID)
	if err != nil {
		return fmt.Errorf("getting changeset: %w", err)
	}
	if csNode == nil || csNode.Kind != graph.KindChangeSet {
		return fmt.Errorf("not a changeset: %s", selector)
	}

	// Build semantic diff
	semanticDiff, err := semanticdiff.FromChangeSet(db, csNode)
	if err != nil {
		return fmt.Errorf("building semantic diff: %w", err)
	}
	if semanticDiff == nil {
		return fmt.Errorf("no changes in changeset")
	}

	// Build the summary
	summary := review.BuildReviewSummary(semanticDiff, loadModuleCategorizer())

	// Use intent as title if available
	if intentStr, ok := csNode.Payload["intent"].(string); ok && intentStr != "" {
		summary.Title = intentStr
	}

	// Run AI review if requested
	if reviewAI {
		if !ai.IsConfigured() {
			fmt.Println("⚠ AI review requires ANTHROPIC_API_KEY environment variable")
		} else {
			fmt.Println("Running AI review...")
			reviewer, err := ai.NewReviewer()
			if err != nil {
				fmt.Printf("⚠ AI review setup failed: %v\n", err)
			} else {
				result, err := reviewer.Review(semanticDiff)
				if err != nil {
					fmt.Printf("⚠ AI review failed: %v\n", err)
				} else {
					summary.Suggestions = result.Suggestions
					if result.RiskLevel != "" {
						fmt.Printf("Risk Level: %s\n\n", result.RiskLevel)
					}
				}
			}
		}
	}

	// Persist summary to any review targeting this changeset
	reviews, _ := review.NewManager(db).List()
	for _, rev := range reviews {
		if util.BytesToHex(rev.TargetID) == util.BytesToHex(csID) {
			summaryData := map[string]interface{}{
				"text":          summary.FormatSummary(),
				"totalFiles":    summary.TotalFiles,
				"apiChanges":    summary.APIChanges,
				"breakingCount": summary.BreakingCount,
			}
			// Build change groups for structured display
			var changeGroups []map[string]interface{}
			for _, c := range summary.Changes {
				group := map[string]interface{}{
					"summary": c.Summary,
					"kind":    string(c.Kind),
					"files":   c.Files,
				}
				var symbols []map[string]string
				for _, s := range c.Symbols {
					symbols = append(symbols, map[string]string{
						"name":   s.Name,
						"action": string(s.Action),
						"file":   s.File,
						"kind":   string(s.Kind),
					})
				}
				if len(symbols) > 0 {
					group["symbols"] = symbols
				}
				changeGroups = append(changeGroups, group)
			}
			summaryData["changes"] = changeGroups

			node, _ := db.GetNode(rev.ID)
			if node != nil {
				node.Payload["summary"] = summaryData
				node.Payload["updatedAt"] = util.NowMs()
				db.UpdateNodePayload(rev.ID, node.Payload)
				debugf("Persisted summary to review %s", review.IDToHex(rev.ID)[:12])
			}
		}
	}

	// Display the summary
	fmt.Println(summary.FormatSummary())

	// Show changeset metadata
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("Changeset: %s\n", util.BytesToHex(csID)[:12])
	if baseHex, ok := csNode.Payload["base"].(string); ok {
		fmt.Printf("Base:      %s\n", baseHex[:12])
	}
	if headHex, ok := csNode.Payload["head"].(string); ok {
		fmt.Printf("Head:      %s\n", headHex[:12])
	}
	fmt.Println()

	// Interactive mode
	if reviewInteractive && len(summary.Changes) > 0 {
		fmt.Println("Enter a number to inspect a change group, or 'q' to quit:")
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("> ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)

			if input == "q" || input == "quit" || input == "" {
				break
			}

			idx, err := strconv.Atoi(input)
			if err != nil || idx < 1 || idx > len(summary.Changes) {
				fmt.Printf("Enter a number between 1 and %d\n", len(summary.Changes))
				continue
			}

			// Show detailed view for this change group
			fmt.Println()
			fmt.Println(summary.FormatChange(idx - 1))
			fmt.Println()
			fmt.Println("Enter another number, or 'q' to quit:")
		}
	}

	return nil
}

// fetchWorkspaceFromRemote fetches a workspace and all its dependencies from a remote.
func fetchWorkspaceFromRemote(db *graph.DB, client *remote.Client, remoteName, wsName string) error {
	// Construct the workspace ref name
	refName := "ws." + wsName

	// Fetch the workspace ref
	wsRef, err := client.GetRef(refName)
	if err != nil {
		return fmt.Errorf("getting workspace ref: %w", err)
	}
	if wsRef == nil {
		return fmt.Errorf("workspace '%s' not found on remote", wsName)
	}

	fmt.Printf("  Found workspace ref: %s -> %s\n", refName, hex.EncodeToString(wsRef.Target)[:12])

	// Check if workspace already exists locally
	wsMgr := workspace.NewManager(db)
	existingWs, err := wsMgr.Get(wsName)
	if err != nil {
		return fmt.Errorf("checking existing workspace: %w", err)
	}
	if existingWs != nil {
		return fmt.Errorf("workspace '%s' already exists locally (delete it first with 'kai ws delete --ws %s')", wsName, wsName)
	}

	// Fetch the workspace node
	wsContent, wsKind, err := client.GetObject(wsRef.Target)
	if err != nil {
		return fmt.Errorf("fetching workspace object: %w", err)
	}
	if wsContent == nil {
		return fmt.Errorf("workspace object not found on remote")
	}
	if wsKind != "Workspace" {
		return fmt.Errorf("expected Workspace, got %s", wsKind)
	}

	var wsPayload map[string]interface{}
	if err := json.Unmarshal(wsContent, &wsPayload); err != nil {
		return fmt.Errorf("parsing workspace payload: %w", err)
	}

	fmt.Printf("  Fetching workspace: %s\n", wsName)

	// Extract references to fetch
	var objectsToFetch [][]byte
	seenObjects := make(map[string]bool)

	// Add base snapshot
	if baseHex, ok := wsPayload["baseSnapshot"].(string); ok && baseHex != "" {
		if baseID, err := util.HexToBytes(baseHex); err == nil {
			objectsToFetch = append(objectsToFetch, baseID)
			seenObjects[string(baseID)] = true
		}
	}

	// Add head snapshot
	if headHex, ok := wsPayload["headSnapshot"].(string); ok && headHex != "" {
		if headID, err := util.HexToBytes(headHex); err == nil {
			if !seenObjects[string(headID)] {
				objectsToFetch = append(objectsToFetch, headID)
				seenObjects[string(headID)] = true
			}
		}
	}

	// Add open changesets
	if csArr, ok := wsPayload["openChangeSets"].([]interface{}); ok {
		for _, csHex := range csArr {
			if hexStr, ok := csHex.(string); ok {
				if csID, err := util.HexToBytes(hexStr); err == nil {
					if !seenObjects[string(csID)] {
						objectsToFetch = append(objectsToFetch, csID)
						seenObjects[string(csID)] = true
					}
				}
			}
		}
	}

	// Fetch all related objects (BFS to get dependencies)
	fetchedCount := 0
	for len(objectsToFetch) > 0 {
		objID := objectsToFetch[0]
		objectsToFetch = objectsToFetch[1:]

		// Skip if already exists locally
		exists, _ := db.HasNode(objID)
		if exists {
			continue
		}

		content, kind, err := client.GetObject(objID)
		if err != nil {
			fmt.Printf("  Warning: failed to fetch object %s: %v\n", hex.EncodeToString(objID)[:12], err)
			continue
		}
		if content == nil {
			fmt.Printf("  Warning: object %s not found on remote\n", hex.EncodeToString(objID)[:12])
			continue
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(content, &payload); err != nil {
			fmt.Printf("  Warning: failed to parse object %s: %v\n", hex.EncodeToString(objID)[:12], err)
			continue
		}

		// Insert the node
		tx, err := db.BeginTx()
		if err != nil {
			continue
		}
		_, err = db.InsertNode(tx, graph.NodeKind(kind), payload)
		if err != nil {
			tx.Rollback()
			fmt.Printf("  Warning: failed to insert object %s: %v\n", hex.EncodeToString(objID)[:12], err)
			continue
		}
		tx.Commit()
		fetchedCount++

		// For Snapshot nodes, queue their parent if present
		if kind == "Snapshot" {
			if parentHex, ok := payload["parent"].(string); ok && parentHex != "" {
				if parentID, err := util.HexToBytes(parentHex); err == nil {
					if !seenObjects[string(parentID)] {
						objectsToFetch = append(objectsToFetch, parentID)
						seenObjects[string(parentID)] = true
					}
				}
			}
		}

		// For ChangeSet nodes, queue their before/after snapshots
		if kind == "ChangeSet" {
			if beforeHex, ok := payload["beforeSnapshot"].(string); ok && beforeHex != "" {
				if beforeID, err := util.HexToBytes(beforeHex); err == nil {
					if !seenObjects[string(beforeID)] {
						objectsToFetch = append(objectsToFetch, beforeID)
						seenObjects[string(beforeID)] = true
					}
				}
			}
			if afterHex, ok := payload["afterSnapshot"].(string); ok && afterHex != "" {
				if afterID, err := util.HexToBytes(afterHex); err == nil {
					if !seenObjects[string(afterID)] {
						objectsToFetch = append(objectsToFetch, afterID)
						seenObjects[string(afterID)] = true
					}
				}
			}
		}
	}

	if fetchedCount > 0 {
		fmt.Printf("  Fetched %d object(s)\n", fetchedCount)
	}

	// Now create the workspace locally
	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Extract the original UUID from the payload (added during push for transport)
	// If not present (legacy), fall back to using the content-addressed ref target
	var wsID []byte
	if uuidHex, ok := wsPayload["_uuid"].(string); ok && uuidHex != "" {
		wsID, err = util.HexToBytes(uuidHex)
		if err != nil {
			return fmt.Errorf("parsing workspace UUID: %w", err)
		}
		// Remove _uuid from payload - it was only for transport
		delete(wsPayload, "_uuid")
	} else {
		// Legacy fallback: use content-addressed ref target
		wsID = wsRef.Target
	}

	// Insert workspace node with the original UUID
	if err := db.InsertWorkspace(tx, wsID, wsPayload); err != nil {
		return fmt.Errorf("inserting workspace: %w", err)
	}

	// Create BASED_ON edge
	if baseHex, ok := wsPayload["baseSnapshot"].(string); ok && baseHex != "" {
		if baseID, err := util.HexToBytes(baseHex); err == nil {
			if err := db.InsertEdge(tx, wsID, graph.EdgeBasedOn, baseID, nil); err != nil {
				return fmt.Errorf("inserting BASED_ON edge: %w", err)
			}
		}
	}

	// Create HEAD_AT edge
	if headHex, ok := wsPayload["headSnapshot"].(string); ok && headHex != "" {
		if headID, err := util.HexToBytes(headHex); err == nil {
			if err := db.InsertEdge(tx, wsID, graph.EdgeHeadAt, headID, nil); err != nil {
				return fmt.Errorf("inserting HEAD_AT edge: %w", err)
			}
		}
	}

	// Create HAS_CHANGESET edges
	if csArr, ok := wsPayload["openChangeSets"].([]interface{}); ok {
		for _, csHex := range csArr {
			if hexStr, ok := csHex.(string); ok {
				if csID, err := util.HexToBytes(hexStr); err == nil {
					if err := db.InsertEdge(tx, wsID, graph.EdgeHasChangeSet, csID, nil); err != nil {
						fmt.Printf("  Warning: failed to insert HAS_CHANGESET edge: %v\n", err)
					}
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	// Create local workspace ref
	refMgr := ref.NewRefManager(db)
	if err := refMgr.Set(refName, wsRef.Target, ref.KindWorkspace); err != nil {
		return fmt.Errorf("setting local ref: %w", err)
	}

	// Also store remote tracking ref
	remoteRefName := fmt.Sprintf("remote/%s/%s", remoteName, refName)
	if err := refMgr.Set(remoteRefName, wsRef.Target, ref.KindWorkspace); err != nil {
		fmt.Printf("  Warning: failed to set remote tracking ref: %v\n", err)
	}

	fmt.Printf("  Created workspace: %s\n", wsName)
	fmt.Printf("  %s -> %s\n", refName, hex.EncodeToString(wsRef.Target)[:12])
	fmt.Println("Fetch complete.")
	return nil
}

// syncReviewFromRemote reconstructs review <reviewID> from the remote into the
// local graph: it fetches the Review node (preserving its transport _uuid so
// repeated syncs are idempotent), its target changeset and dependencies, creates
// the local review + ref, and syncs comments. Used by `kai fetch --review` and
// by `kai pull` (F-15). It prints per-review progress but no command-completion
// line, so callers can drive it in a loop.
func syncReviewFromRemote(db *graph.DB, client *remote.Client, remoteName, reviewID string) error {
	// Construct the review ref name
	refName := "review." + reviewID

	// Fetch the review ref
	reviewRef, err := client.GetRef(refName)
	if err != nil {
		return fmt.Errorf("getting review ref: %w", err)
	}
	if reviewRef == nil {
		return fmt.Errorf("review '%s' not found on remote", reviewID)
	}

	fmt.Printf("  Found review ref: %s -> %s\n", refName, hex.EncodeToString(reviewRef.Target)[:12])

	// Fetch the review object
	reviewContent, reviewKind, err := client.GetObject(reviewRef.Target)
	if err != nil {
		return fmt.Errorf("fetching review object: %w", err)
	}
	if reviewContent == nil {
		return fmt.Errorf("review object not found on remote")
	}
	if reviewKind != "Review" {
		return fmt.Errorf("expected Review, got %s", reviewKind)
	}

	var reviewPayload map[string]interface{}
	if err := json.Unmarshal(reviewContent, &reviewPayload); err != nil {
		return fmt.Errorf("parsing review payload: %w", err)
	}

	fmt.Printf("  Fetching review: %s\n", reviewPayload["title"])

	// Extract target changeset to fetch
	var objectsToFetch [][]byte
	seenObjects := make(map[string]bool)

	// Add target (changeset or workspace)
	if targetHex, ok := reviewPayload["targetId"].(string); ok && targetHex != "" {
		if targetID, err := util.HexToBytes(targetHex); err == nil {
			objectsToFetch = append(objectsToFetch, targetID)
			seenObjects[string(targetID)] = true
		}
	}

	// Fetch the target and its dependencies (BFS)
	fetchedCount := 0
	for len(objectsToFetch) > 0 {
		objID := objectsToFetch[0]
		objectsToFetch = objectsToFetch[1:]

		// Skip if already exists locally
		exists, _ := db.HasNode(objID)
		if exists {
			continue
		}

		content, kind, err := client.GetObject(objID)
		if err != nil {
			fmt.Printf("  Warning: failed to fetch object %s: %v\n", hex.EncodeToString(objID)[:12], err)
			continue
		}
		if content == nil {
			fmt.Printf("  Warning: object %s not found on remote\n", hex.EncodeToString(objID)[:12])
			continue
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(content, &payload); err != nil {
			fmt.Printf("  Warning: failed to parse object %s: %v\n", hex.EncodeToString(objID)[:12], err)
			continue
		}

		// Insert the node
		tx, err := db.BeginTx()
		if err != nil {
			continue
		}
		_, err = db.InsertNode(tx, graph.NodeKind(kind), payload)
		if err != nil {
			tx.Rollback()
			continue
		}
		tx.Commit()
		fetchedCount++

		// If this is a ChangeSet, queue its base/head snapshots
		if kind == "ChangeSet" {
			if baseHex, ok := payload["base"].(string); ok && baseHex != "" {
				if baseID, err := util.HexToBytes(baseHex); err == nil {
					if !seenObjects[string(baseID)] {
						objectsToFetch = append(objectsToFetch, baseID)
						seenObjects[string(baseID)] = true
					}
				}
			}
			if headHex, ok := payload["head"].(string); ok && headHex != "" {
				if headID, err := util.HexToBytes(headHex); err == nil {
					if !seenObjects[string(headID)] {
						objectsToFetch = append(objectsToFetch, headID)
						seenObjects[string(headID)] = true
					}
				}
			}
		}

		// If this is a Snapshot, queue its files (via HAS edges on server, or payload)
		// Snapshots store file references that may need to be fetched
	}

	if fetchedCount > 0 {
		fmt.Printf("  Fetched %d object(s)\n", fetchedCount)
	}

	// Create the review locally
	tx, err := db.BeginTx()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Extract the original UUID from the payload
	var revID []byte
	if uuidHex, ok := reviewPayload["_uuid"].(string); ok && uuidHex != "" {
		revID, err = util.HexToBytes(uuidHex)
		if err != nil {
			return fmt.Errorf("parsing review UUID: %w", err)
		}
		// Remove _uuid from payload - it was only for transport
		delete(reviewPayload, "_uuid")
	} else {
		// Legacy fallback: generate new UUID
		revID = make([]byte, 16)
		if _, err := rand.Read(revID); err != nil {
			return fmt.Errorf("generating review ID: %w", err)
		}
	}

	// Insert or update review node
	reviewExists := false
	if err := db.InsertReview(tx, revID, reviewPayload); err != nil {
		// Review already exists locally
		tx.Rollback()
		reviewExists = true
		fmt.Printf("  Review exists locally, syncing comments...\n")
	} else {
		// Create REVIEW_OF edge to target
		if targetHex, ok := reviewPayload["targetId"].(string); ok && targetHex != "" {
			if targetID, err := util.HexToBytes(targetHex); err == nil {
				if err := db.InsertEdge(tx, revID, graph.EdgeReviewOf, targetID, nil); err != nil {
					fmt.Printf("  Warning: failed to insert REVIEW_OF edge: %v\n", err)
				}
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing transaction: %w", err)
		}

		// Create local review ref
		refMgr := ref.NewRefManager(db)
		if err := refMgr.Set(refName, reviewRef.Target, ref.KindReview); err != nil {
			return fmt.Errorf("setting local ref: %w", err)
		}

		// Also store remote tracking ref
		remoteRefName := fmt.Sprintf("remote/%s/%s", remoteName, refName)
		if err := refMgr.Set(remoteRefName, reviewRef.Target, ref.KindReview); err != nil {
			fmt.Printf("  Warning: failed to set remote tracking ref: %v\n", err)
		}

		title, _ := reviewPayload["title"].(string)
		fmt.Printf("  Created review: %s (%s)\n", hex.EncodeToString(revID)[:12], title)
		fmt.Printf("  %s -> %s\n", refName, hex.EncodeToString(reviewRef.Target)[:12])
	}
	_ = reviewExists
	// Fetch comments from the server
	commentsResp, err := client.GetReviewComments(reviewID)
	if err == nil && len(commentsResp) > 0 {
		fmt.Printf("  Fetching %d comment(s)...\n", len(commentsResp))
		mgr := review.NewManager(db)
		for _, c := range commentsResp {
			body, _ := c["body"].(string)
			author, _ := c["author"].(string)
			parentID, _ := c["parentId"].(string)
			if body == "" {
				continue
			}
			var anchor *review.CommentAnchor
			if fp, ok := c["filePath"].(string); ok && fp != "" {
				line, _ := c["line"].(float64)
				anchor = &review.CommentAnchor{FilePath: fp, Line: int(line)}
			}
			_, err := mgr.AddComment(revID, author, body, parentID, anchor)
			if err != nil {
				debugf("warning: failed to add comment: %v", err)
			}
		}
	}

	return nil
}

// fetchReviewFromRemote is the `kai fetch --review <id>` entry point: sync the
// review, then print the command's completion line.
func fetchReviewFromRemote(db *graph.DB, client *remote.Client, remoteName, reviewID string) error {
	if err := syncReviewFromRemote(db, client, remoteName, reviewID); err != nil {
		return err
	}
	fmt.Println("Fetch complete.")
	return nil
}

// Modules command implementations

var modulesRulesPath = filepath.Join(kaiDir, "rules", "modules.yaml")

// loadModuleCategorizer returns a FileCategorizer that groups files by module name.
// Falls back to nil (use heuristic) if no modules are configured.
func loadModuleCategorizer() review.FileCategorizer {
	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil || len(matcher.GetAllModules()) == 0 {
		// Try legacy path
		matcher, err = module.LoadRulesOrEmpty("kai.modules.yaml")
		if err != nil || len(matcher.GetAllModules()) == 0 {
			return nil
		}
	}

	return func(path string) string {
		modules := matcher.MatchPath(path)
		if len(modules) > 0 {
			return modules[0]
		}
		// Fall back to heuristic for unmatched files
		return categorizeFileHeuristic(path)
	}
}

// categorizeFileHeuristic is the path-based fallback when modules aren't available.
func categorizeFileHeuristic(path string) string {
	lower := strings.ToLower(path)

	if strings.Contains(lower, "_test.") || strings.Contains(lower, ".test.") ||
		strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") ||
		strings.HasSuffix(lower, "_test.go") || strings.HasSuffix(lower, ".spec.ts") {
		return "test"
	}
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".txt") ||
		strings.Contains(lower, "/docs/") {
		return "docs"
	}
	if strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".yaml") ||
		strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".toml") ||
		strings.Contains(lower, "config") || lower == "package.json" ||
		lower == "go.mod" || lower == "go.sum" {
		return "config"
	}
	if strings.Contains(lower, "/api/") || strings.Contains(lower, "/handler") ||
		strings.Contains(lower, "/route") || strings.Contains(lower, "/endpoint") ||
		strings.Contains(lower, "controller") {
		return "api"
	}
	return "internal"
}

func runModulesInit(cmd *cobra.Command, args []string) error {
	if !modulesInfer {
		// Just show current config or prompt
		matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
		if err != nil {
			return err
		}
		modules := matcher.GetAllModules()
		if len(modules) == 0 {
			fmt.Println("No modules configured.")
			fmt.Println()
			fmt.Println("To auto-detect modules from your codebase:")
			fmt.Println("  kai modules init --infer --write")
			fmt.Println()
			fmt.Println("To add modules manually:")
			fmt.Println("  kai modules add App src/app.js")
			fmt.Println("  kai modules add Utils \"src/utils/**\"")
			return nil
		}
		fmt.Printf("Found %d module(s) in %s\n", len(modules), modulesRulesPath)
		for _, m := range modules {
			fmt.Printf("  %s: %v\n", m.Name, m.Paths)
		}
		return nil
	}

	// Infer modules from directory structure
	fmt.Println("Scanning for modules...")

	var modules []module.ModuleRule

	// Look for common source root directories
	sourceRoots := []string{"src", "lib", "pkg", "internal", "app", "core"}
	testRoots := []string{"tests", "test", "__tests__", "spec"}

	// Find which source root exists
	var sourceRoot string
	for _, dir := range sourceRoots {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			sourceRoot = dir
			break
		}
	}

	if sourceRoot != "" {
		// Look inside the source root for subdirectories (these become modules)
		entries, err := os.ReadDir(sourceRoot)
		if err != nil {
			return err
		}

		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				// Capitalize first letter for module name
				name := strings.ToUpper(e.Name()[:1]) + e.Name()[1:]
				modules = append(modules, module.ModuleRule{
					Name:  name,
					Paths: []string{filepath.Join(sourceRoot, e.Name()) + "/**"},
				})
			}
		}

		// Also check for top-level files in source root (e.g., src/app.js)
		for _, e := range entries {
			if !e.IsDir() {
				ext := filepath.Ext(e.Name())
				if ext == ".js" || ext == ".ts" || ext == ".jsx" || ext == ".tsx" || ext == ".go" || ext == ".py" {
					baseName := strings.TrimSuffix(e.Name(), ext)
					name := strings.ToUpper(baseName[:1]) + baseName[1:]
					// Check if module already exists
					exists := false
					for _, m := range modules {
						if m.Name == name {
							exists = true
							break
						}
					}
					if !exists {
						modules = append(modules, module.ModuleRule{
							Name:  name,
							Paths: []string{filepath.Join(sourceRoot, e.Name())},
						})
					}
				}
			}
		}
	} else {
		// No standard source root, look for top-level directories
		entries, err := os.ReadDir(".")
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") &&
				e.Name() != "node_modules" && e.Name() != "vendor" &&
				!contains(testRoots, e.Name()) {
				name := strings.ToUpper(e.Name()[:1]) + e.Name()[1:]
				modules = append(modules, module.ModuleRule{
					Name:  name,
					Paths: []string{e.Name() + "/**"},
				})
			}
		}
	}

	// Add tests module if specified or auto-detect
	if modulesTestsGlob != "" {
		modules = append(modules, module.ModuleRule{
			Name:  "Tests",
			Paths: []string{modulesTestsGlob},
		})
	} else {
		for _, dir := range testRoots {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				modules = append(modules, module.ModuleRule{
					Name:  "Tests",
					Paths: []string{dir + "/**"},
				})
				break
			}
		}
	}

	if len(modules) == 0 {
		fmt.Println("No modules detected. Add modules manually:")
		fmt.Println("  kai modules add App src/app.js")
		return nil
	}

	// Print inferred modules
	fmt.Printf("\nInferred %d module(s):\n", len(modules))
	for _, m := range modules {
		fmt.Printf("  %s:\n", m.Name)
		for _, p := range m.Paths {
			fmt.Printf("    - %s\n", p)
		}
	}

	if modulesDryRun || !modulesWrite {
		fmt.Println()
		if !modulesWrite {
			fmt.Println("Run with --write to save to", modulesRulesPath)
		}
		return nil
	}

	// Save modules
	matcher := module.NewMatcher(modules)
	if err := matcher.SaveRules(modulesRulesPath); err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("Saved to %s\n", modulesRulesPath)
	return nil
}

func runModulesAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	paths := args[1:]

	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return err
	}

	existing := matcher.GetModule(name)
	if existing != nil {
		// Update existing module
		matcher.AddModule(name, paths)
		fmt.Printf("Updated module: %s\n", name)
	} else {
		matcher.AddModule(name, paths)
		fmt.Printf("Added module: %s\n", name)
	}

	for _, p := range paths {
		fmt.Printf("  - %s\n", p)
	}

	if err := matcher.SaveRules(modulesRulesPath); err != nil {
		return err
	}
	fmt.Printf("Saved to %s\n", modulesRulesPath)
	return nil
}

func runModulesList(cmd *cobra.Command, args []string) error {
	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return err
	}

	modules := matcher.GetAllModules()
	if len(modules) == 0 {
		fmt.Println("No modules configured.")
		fmt.Println("Run 'kai modules init --infer --write' or 'kai modules add <name> <glob>'")
		return nil
	}

	fmt.Printf("Modules (%d):\n", len(modules))
	for _, m := range modules {
		fmt.Printf("  %s\n", m.Name)
		for _, p := range m.Paths {
			fmt.Printf("    - %s\n", p)
		}
	}
	return nil
}

func runModulesPreview(cmd *cobra.Command, args []string) error {
	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return err
	}

	modules := matcher.GetAllModules()
	if len(modules) == 0 {
		fmt.Println("No modules configured.")
		return nil
	}

	// Get all files in the current directory
	var allFiles []string
	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// Skip hidden directories (but not "." itself), node_modules, and vendor
			if (strings.HasPrefix(name, ".") && name != ".") || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		allFiles = append(allFiles, path)
		return nil
	})
	if err != nil {
		return err
	}

	// Filter by module name if specified
	var targetModules []module.ModuleRule
	if len(args) > 0 {
		moduleName := args[0]
		for _, m := range modules {
			if m.Name == moduleName {
				targetModules = append(targetModules, m)
				break
			}
		}
		if len(targetModules) == 0 {
			return fmt.Errorf("module %q not found", moduleName)
		}
	} else {
		targetModules = modules
	}

	// Match files to modules
	matchedFiles := matcher.MatchPaths(allFiles)

	for _, m := range targetModules {
		files := matchedFiles[m.Name]
		fmt.Printf("%s (%d files):\n", m.Name, len(files))
		if len(files) == 0 {
			fmt.Println("  (no files matched)")
		} else {
			for _, f := range files {
				fmt.Printf("  %s\n", f)
			}
		}
		fmt.Println()
	}

	// Show unmatched files
	if len(args) == 0 {
		var unmatched []string
		for _, f := range allFiles {
			mods := matcher.MatchPath(f)
			if len(mods) == 0 {
				unmatched = append(unmatched, f)
			}
		}
		if len(unmatched) > 0 {
			fmt.Printf("Unmatched (%d files):\n", len(unmatched))
			for _, f := range unmatched {
				fmt.Printf("  %s\n", f)
			}
		}
	}

	return nil
}

func runModulesShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return err
	}

	mod := matcher.GetModule(name)
	if mod == nil {
		return fmt.Errorf("module %q not found", name)
	}

	fmt.Printf("Module: %s\n", mod.Name)
	fmt.Println("Patterns:")
	for _, p := range mod.Paths {
		fmt.Printf("  - %s\n", p)
	}
	return nil
}

func runModulesRm(cmd *cobra.Command, args []string) error {
	name := args[0]

	matcher, err := module.LoadRulesOrEmpty(modulesRulesPath)
	if err != nil {
		return err
	}

	if !matcher.RemoveModule(name) {
		return fmt.Errorf("module %q not found", name)
	}

	if err := matcher.SaveRules(modulesRulesPath); err != nil {
		return err
	}

	fmt.Printf("Removed module: %s\n", name)
	return nil
}

// contains checks if a string slice contains a value
func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// ========== Coverage Ingestion ==========

var coverageMapFile = filepath.Join(kaiDir, "coverage-map.json")

// runCIIngestCoverage ingests coverage reports to build file→test mappings
func runCIIngestCoverage(cmd *cobra.Command, args []string) error {
	// Read coverage file
	data, err := os.ReadFile(ciCoverageFrom)
	if err != nil {
		return fmt.Errorf("reading coverage file: %w", err)
	}

	// Detect format if auto
	format := ciCoverageFormat
	if format == "auto" {
		format = detectCoverageFormat(ciCoverageFrom, data)
	}

	// Parse coverage based on format
	var entries map[string][]CoverageEntry
	switch format {
	case "nyc":
		entries, err = parseNYCCoverage(data)
	case "coveragepy":
		entries, err = parseCoveragePyCoverage(data)
	case "jacoco":
		entries, err = parseJaCoCoCoverage(data)
	default:
		return fmt.Errorf("unknown coverage format: %s (use --format nyc|coveragepy|jacoco)", format)
	}

	if err != nil {
		return fmt.Errorf("parsing coverage (%s): %w", format, err)
	}

	// Normalize paths to repo-relative POSIX format
	normalizedEntries := make(map[string][]CoverageEntry)
	for filePath, testEntries := range entries {
		normPath := normalizePath(filePath)
		normalizedEntries[normPath] = testEntries
	}
	entries = normalizedEntries

	// Load or create coverage map
	coverageMap := loadOrCreateCoverageMap()

	// Load policy for retention settings
	ciPolicy, _, _ := loadCIPolicy()
	retentionDays := ciPolicy.Coverage.RetentionDays
	if retentionDays == 0 {
		retentionDays = 90 // Default
	}

	// Merge new entries
	timestamp := time.Now().UTC().Format(time.RFC3339)
	for filePath, testEntries := range entries {
		for i := range testEntries {
			testEntries[i].LastSeenAt = timestamp
		}
		// Merge with existing entries
		coverageMap.Entries[filePath] = mergeTestEntries(coverageMap.Entries[filePath], testEntries)
	}

	// Prune old entries based on retention policy
	pruneCount := pruneCoverageMap(coverageMap, retentionDays)

	// Update metadata
	coverageMap.IngestedAt = timestamp
	if ciCoverageBranch != "" {
		coverageMap.Branch = ciCoverageBranch
	}
	if ciCoverageTag != "" {
		coverageMap.Tag = ciCoverageTag
	}

	// Save coverage map
	if err := saveCoverageMap(coverageMap); err != nil {
		return fmt.Errorf("saving coverage map: %w", err)
	}

	// Output summary
	fmt.Println("Coverage Ingestion Complete")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("Format:      %s\n", format)
	fmt.Printf("Files:       %d\n", len(entries))
	fmt.Printf("Total pairs: %d\n", countTotalPairs(coverageMap.Entries))
	if pruneCount > 0 {
		fmt.Printf("Pruned:      %d old entries (>%d days)\n", pruneCount, retentionDays)
	}
	if ciCoverageBranch != "" {
		fmt.Printf("Branch:      %s\n", ciCoverageBranch)
	}
	if ciCoverageTag != "" {
		fmt.Printf("Tag:         %s\n", ciCoverageTag)
	}
	fmt.Printf("Saved to:    %s\n", coverageMapFile)

	return nil
}

// normalizePath converts a file path to repo-relative POSIX format
func normalizePath(path string) string {
	// Convert Windows backslashes to forward slashes
	path = strings.ReplaceAll(path, "\\", "/")

	// Remove common absolute path prefixes
	if idx := strings.Index(path, "/src/"); idx >= 0 {
		path = path[idx+1:]
	} else if idx := strings.Index(path, "/app/"); idx >= 0 {
		path = path[idx+1:]
	}

	// Strip leading ./
	path = strings.TrimPrefix(path, "./")

	return path
}

// pruneCoverageMap removes entries older than retentionDays
func pruneCoverageMap(cm *CoverageMap, retentionDays int) int {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	pruneCount := 0

	for filePath, entries := range cm.Entries {
		var kept []CoverageEntry
		for _, e := range entries {
			if e.LastSeenAt != "" {
				lastSeen, err := time.Parse(time.RFC3339, e.LastSeenAt)
				if err == nil && lastSeen.Before(cutoff) {
					pruneCount++
					continue
				}
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(cm.Entries, filePath)
		} else {
			cm.Entries[filePath] = kept
		}
	}

	return pruneCount
}

// detectCoverageFormat auto-detects coverage format from filename and content
func detectCoverageFormat(path string, data []byte) string {
	name := filepath.Base(path)

	if strings.Contains(name, "coverage-final") || strings.Contains(name, "nyc") {
		return "nyc"
	}
	if strings.HasSuffix(name, ".xml") {
		return "jacoco"
	}

	content := string(data)
	if strings.Contains(content, "statementMap") {
		return "nyc"
	}
	if strings.Contains(content, "<jacoco") || strings.Contains(content, "<report") {
		return "jacoco"
	}
	if strings.Contains(content, "executed_lines") || strings.Contains(content, "missing_lines") {
		return "coveragepy"
	}

	return "nyc"
}

// parseNYCCoverage parses NYC/Istanbul coverage-final.json
func parseNYCCoverage(data []byte) (map[string][]CoverageEntry, error) {
	var nycData map[string]struct {
		Path         string `json:"path"`
		StatementMap map[string]struct {
			Start struct{ Line int } `json:"start"`
			End   struct{ Line int } `json:"end"`
		} `json:"statementMap"`
		S map[string]int `json:"s"`
	}

	if err := json.Unmarshal(data, &nycData); err != nil {
		return nil, err
	}

	entries := make(map[string][]CoverageEntry)
	for filePath, coverage := range nycData {
		var coveredLines []int
		for stmtID, hits := range coverage.S {
			if hits > 0 {
				if stmt, ok := coverage.StatementMap[stmtID]; ok {
					coveredLines = append(coveredLines, stmt.Start.Line)
				}
			}
		}

		if len(coveredLines) > 0 {
			entries[filePath] = []CoverageEntry{
				{TestID: "aggregate", HitCount: 1, LinesCovered: coveredLines},
			}
		}
	}

	return entries, nil
}

// parseCoveragePyCoverage parses coverage.py JSON output
func parseCoveragePyCoverage(data []byte) (map[string][]CoverageEntry, error) {
	var coverageData struct {
		Files map[string]struct {
			ExecutedLines []int `json:"executed_lines"`
		} `json:"files"`
	}

	if err := json.Unmarshal(data, &coverageData); err != nil {
		return nil, err
	}

	entries := make(map[string][]CoverageEntry)
	for filePath, coverage := range coverageData.Files {
		if len(coverage.ExecutedLines) > 0 {
			entries[filePath] = []CoverageEntry{
				{TestID: "aggregate", HitCount: 1, LinesCovered: coverage.ExecutedLines},
			}
		}
	}

	return entries, nil
}

// parseJaCoCoCoverage parses JaCoCo XML format
func parseJaCoCoCoverage(data []byte) (map[string][]CoverageEntry, error) {
	entries := make(map[string][]CoverageEntry)

	sourceFileRe := regexp.MustCompile(`<sourcefile[^>]*name="([^"]+)"`)
	lineRe := regexp.MustCompile(`<line[^>]*nr="(\d+)"[^>]*ci="(\d+)"`)

	content := string(data)
	sourceMatches := sourceFileRe.FindAllStringSubmatchIndex(content, -1)

	for i, match := range sourceMatches {
		fileName := content[match[2]:match[3]]

		endIdx := len(content)
		if i+1 < len(sourceMatches) {
			endIdx = sourceMatches[i+1][0]
		}

		section := content[match[0]:endIdx]
		lineMatches := lineRe.FindAllStringSubmatch(section, -1)

		var coveredLines []int
		for _, lm := range lineMatches {
			if len(lm) >= 3 {
				lineNum := 0
				ci := 0
				fmt.Sscanf(lm[1], "%d", &lineNum)
				fmt.Sscanf(lm[2], "%d", &ci)
				if ci > 0 {
					coveredLines = append(coveredLines, lineNum)
				}
			}
		}

		if len(coveredLines) > 0 {
			entries[fileName] = []CoverageEntry{
				{TestID: "aggregate", HitCount: 1, LinesCovered: coveredLines},
			}
		}
	}

	return entries, nil
}

func loadOrCreateCoverageMap() *CoverageMap {
	data, err := os.ReadFile(coverageMapFile)
	if err != nil {
		return &CoverageMap{Version: 1, Entries: make(map[string][]CoverageEntry)}
	}

	var cm CoverageMap
	if json.Unmarshal(data, &cm) != nil {
		return &CoverageMap{Version: 1, Entries: make(map[string][]CoverageEntry)}
	}

	if cm.Entries == nil {
		cm.Entries = make(map[string][]CoverageEntry)
	}
	return &cm
}

func saveCoverageMap(cm *CoverageMap) error {
	os.MkdirAll(filepath.Dir(coverageMapFile), 0755)
	data, err := json.MarshalIndent(cm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(coverageMapFile, data, 0644)
}

func mergeTestEntries(existing, new []CoverageEntry) []CoverageEntry {
	entryMap := make(map[string]*CoverageEntry)
	for i := range existing {
		entryMap[existing[i].TestID] = &existing[i]
	}
	for _, e := range new {
		if ex, ok := entryMap[e.TestID]; ok {
			ex.HitCount += e.HitCount
			ex.LastSeenAt = e.LastSeenAt
		} else {
			entryCopy := e
			entryMap[e.TestID] = &entryCopy
		}
	}

	result := make([]CoverageEntry, 0, len(entryMap))
	for _, e := range entryMap {
		result = append(result, *e)
	}
	return result
}

func countTotalPairs(entries map[string][]CoverageEntry) int {
	count := 0
	for _, tests := range entries {
		count += len(tests)
	}
	return count
}

// mapKeysToSortedSlice converts a map[string]bool to a sorted slice of keys
func mapKeysToSortedSlice(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

// ========== Contract Ingestion ==========

var contractsFile = filepath.Join(kaiDir, "contracts.json")

// runCIIngestContracts registers contract schemas and their tests
func runCIIngestContracts(cmd *cobra.Command, args []string) error {
	validTypes := map[string]bool{"openapi": true, "protobuf": true, "graphql": true}
	if !validTypes[ciContractType] {
		return fmt.Errorf("invalid contract type: %s (use openapi, protobuf, or graphql)", ciContractType)
	}

	if _, err := os.Stat(ciContractPath); os.IsNotExist(err) {
		return fmt.Errorf("schema file not found: %s", ciContractPath)
	}

	schemaData, err := os.ReadFile(ciContractPath)
	if err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}

	// Canonicalize schema before hashing to avoid noisy re-runs on non-semantic edits
	canonicalData := canonicalizeSchema(schemaData, ciContractType)
	digest := util.Blake3HashHex(canonicalData)

	registry := loadOrCreateContractRegistry()

	found := false
	for i, c := range registry.Contracts {
		if c.Path == ciContractPath {
			registry.Contracts[i].Type = ciContractType
			registry.Contracts[i].Service = ciContractService
			registry.Contracts[i].Tests = strings.Split(ciContractTests, ",")
			registry.Contracts[i].Digest = digest
			if ciContractGenerated != "" {
				registry.Contracts[i].Generated = strings.Split(ciContractGenerated, ",")
			}
			found = true
			break
		}
	}

	if !found {
		binding := ContractBinding{
			Type:    ciContractType,
			Path:    ciContractPath,
			Service: ciContractService,
			Tests:   strings.Split(ciContractTests, ","),
			Digest:  digest,
		}
		if ciContractGenerated != "" {
			binding.Generated = strings.Split(ciContractGenerated, ",")
		}
		registry.Contracts = append(registry.Contracts, binding)
	}

	if err := saveContractRegistry(registry); err != nil {
		return fmt.Errorf("saving contract registry: %w", err)
	}

	fmt.Println("Contract Registration Complete")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("Type:     %s\n", ciContractType)
	fmt.Printf("Path:     %s\n", ciContractPath)
	fmt.Printf("Service:  %s\n", ciContractService)
	fmt.Printf("Tests:    %s\n", ciContractTests)
	fmt.Printf("Digest:   %s\n", digest[:16]+"...")
	if ciContractGenerated != "" {
		fmt.Printf("Generated: %s\n", ciContractGenerated)
	}
	fmt.Printf("Saved to: %s\n", contractsFile)

	return nil
}

func loadOrCreateContractRegistry() *ContractRegistry {
	data, err := os.ReadFile(contractsFile)
	if err != nil {
		return &ContractRegistry{Version: 1, Contracts: []ContractBinding{}}
	}

	var cr ContractRegistry
	if json.Unmarshal(data, &cr) != nil {
		return &ContractRegistry{Version: 1, Contracts: []ContractBinding{}}
	}
	return &cr
}

func saveContractRegistry(cr *ContractRegistry) error {
	os.MkdirAll(filepath.Dir(contractsFile), 0755)
	data, err := json.MarshalIndent(cr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(contractsFile, data, 0644)
}

// canonicalizeSchema normalizes schema content before hashing to avoid noisy re-runs
// on non-semantic changes (comments, whitespace, key ordering)
func canonicalizeSchema(data []byte, schemaType string) []byte {
	content := string(data)

	switch schemaType {
	case "openapi":
		// For YAML/JSON OpenAPI: try to parse and re-serialize with sorted keys
		// Fallback to basic normalization if parsing fails
		return canonicalizeYAMLorJSON(data)

	case "graphql":
		// For GraphQL: strip comments and normalize whitespace
		return canonicalizeGraphQL(content)

	case "protobuf":
		// For Protobuf: strip comments and normalize whitespace
		return canonicalizeProtobuf(content)
	}

	return data
}

// canonicalizeYAMLorJSON attempts to parse and re-serialize with sorted keys
func canonicalizeYAMLorJSON(data []byte) []byte {
	// Try JSON first
	var jsonObj interface{}
	if err := json.Unmarshal(data, &jsonObj); err == nil {
		if canonical, err := json.Marshal(jsonObj); err == nil {
			return canonical
		}
	}

	// Try YAML
	var yamlObj interface{}
	if err := yaml.Unmarshal(data, &yamlObj); err == nil {
		if canonical, err := yaml.Marshal(yamlObj); err == nil {
			return canonical
		}
	}

	// Fallback: basic whitespace normalization
	return normalizeWhitespace(data)
}

// canonicalizeGraphQL strips GraphQL comments and normalizes whitespace
func canonicalizeGraphQL(content string) []byte {
	// Strip single-line comments (# ...)
	lines := strings.Split(content, "\n")
	var cleaned []string
	for _, line := range lines {
		// Remove comment portion
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return []byte(strings.Join(cleaned, "\n"))
}

// canonicalizeProtobuf strips Protobuf comments and normalizes whitespace
func canonicalizeProtobuf(content string) []byte {
	// Strip // comments
	var result strings.Builder
	lines := strings.Split(content, "\n")
	inBlockComment := false

	for _, line := range lines {
		// Handle block comments /* ... */
		for {
			if inBlockComment {
				if idx := strings.Index(line, "*/"); idx >= 0 {
					line = line[idx+2:]
					inBlockComment = false
				} else {
					line = ""
					break
				}
			} else {
				if idx := strings.Index(line, "/*"); idx >= 0 {
					prefix := line[:idx]
					rest := line[idx+2:]
					if endIdx := strings.Index(rest, "*/"); endIdx >= 0 {
						line = prefix + rest[endIdx+2:]
					} else {
						line = prefix
						inBlockComment = true
					}
				} else {
					break
				}
			}
		}

		// Strip // comments
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}

		line = strings.TrimSpace(line)
		if line != "" {
			result.WriteString(line)
			result.WriteString("\n")
		}
	}

	return []byte(result.String())
}

// normalizeWhitespace collapses multiple whitespace to single space
func normalizeWhitespace(data []byte) []byte {
	// Replace all whitespace sequences with single space
	re := regexp.MustCompile(`\s+`)
	return re.ReplaceAll(data, []byte(" "))
}

// ========== Plan Annotation ==========

// runCIAnnotatePlan annotates a plan with fallback information
func runCIAnnotatePlan(cmd *cobra.Command, args []string) error {
	planFile := args[0]

	data, err := os.ReadFile(planFile)
	if err != nil {
		return fmt.Errorf("reading plan: %w", err)
	}

	var plan CIPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return fmt.Errorf("parsing plan: %w", err)
	}

	if ciFallbackUsed {
		plan.Fallback.Used = true
	}
	if ciFallbackReason != "" {
		plan.Fallback.Reason = ciFallbackReason
	}
	if ciFallbackTrigger != "" {
		plan.Fallback.Trigger = ciFallbackTrigger
	}
	if ciFallbackExitCode != 0 {
		plan.Fallback.ExitCode = ciFallbackExitCode
	}

	output, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}

	if err := os.WriteFile(planFile, output, 0644); err != nil {
		return fmt.Errorf("writing plan: %w", err)
	}

	fmt.Printf("Plan annotated: %s\n", planFile)
	if plan.Fallback.Used {
		fmt.Printf("  Fallback: used=true reason=%s\n", plan.Fallback.Reason)
		if plan.Fallback.Trigger != "" {
			fmt.Printf("  Trigger: %s\n", plan.Fallback.Trigger)
		}
		if plan.Fallback.ExitCode != 0 {
			fmt.Printf("  Exit code: %d\n", plan.Fallback.ExitCode)
		}
	}

	return nil
}

// runCIValidatePlan validates plan JSON schema and required fields
func runCIValidatePlan(cmd *cobra.Command, args []string) error {
	planFile := args[0]

	data, err := os.ReadFile(planFile)
	if err != nil {
		return fmt.Errorf("reading plan: %w", err)
	}

	var plan CIPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return fmt.Errorf("parsing plan: %w", err)
	}

	var errors []string

	// Required fields
	if plan.Mode == "" {
		errors = append(errors, "missing required field: mode")
	}
	if plan.Risk == "" {
		errors = append(errors, "missing required field: risk")
	}
	if plan.SafetyMode == "" {
		errors = append(errors, "missing required field: safetyMode")
	}

	// Provenance fields
	if plan.Provenance.KaiVersion == "" {
		errors = append(errors, "missing provenance.kaiVersion")
	}
	if plan.Provenance.DetectorVersion == "" {
		errors = append(errors, "missing provenance.detectorVersion")
	}
	if plan.Provenance.GeneratedAt == "" {
		errors = append(errors, "missing provenance.generatedAt")
	}

	// Strict mode: validate optional fields
	if ciValidateStrict {
		if plan.Provenance.PolicyHash == "" && plan.Mode != "skip" {
			errors = append(errors, "strict: missing provenance.policyHash")
		}
		if len(plan.Provenance.Analyzers) == 0 && plan.Mode != "skip" {
			errors = append(errors, "strict: missing provenance.analyzers")
		}
	}

	// Validate mode values
	validModes := map[string]bool{"selective": true, "expanded": true, "full": true, "shadow": true, "skip": true}
	if plan.Mode != "" && !validModes[plan.Mode] {
		errors = append(errors, fmt.Sprintf("invalid mode value: %s", plan.Mode))
	}

	// Validate risk values
	validRisks := map[string]bool{"low": true, "medium": true, "high": true}
	if plan.Risk != "" && !validRisks[plan.Risk] {
		errors = append(errors, fmt.Sprintf("invalid risk value: %s", plan.Risk))
	}

	// Validate safety mode values
	validSafetyModes := map[string]bool{"shadow": true, "guarded": true, "strict": true}
	if plan.SafetyMode != "" && !validSafetyModes[plan.SafetyMode] {
		errors = append(errors, fmt.Sprintf("invalid safetyMode value: %s", plan.SafetyMode))
	}

	if len(errors) > 0 {
		fmt.Println("Plan validation FAILED")
		fmt.Println(strings.Repeat("-", 40))
		for _, e := range errors {
			fmt.Printf("  - %s\n", e)
		}
		return fmt.Errorf("validation failed with %d errors", len(errors))
	}

	fmt.Println("Plan validation PASSED")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("Mode:       %s\n", plan.Mode)
	fmt.Printf("Risk:       %s\n", plan.Risk)
	fmt.Printf("Safety:     %s\n", plan.SafetyMode)
	fmt.Printf("Confidence: %.0f%%\n", plan.Safety.Confidence*100)
	fmt.Printf("Targets:    %d run, %d skip\n", len(plan.Targets.Run), len(plan.Targets.Skip))
	fmt.Printf("Kai:        %s\n", plan.Provenance.KaiVersion)
	fmt.Printf("Detector:   %s\n", plan.Provenance.DetectorVersion)

	return nil
}

// runUpdate downloads and installs the latest kai binary from GitHub releases.
func runUpdate(cmd *cobra.Command, args []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")

	// Determine OS and architecture
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	binaryName := fmt.Sprintf("kai-%s-%s", goos, goarch)

	// GitHub releases URL
	downloadURL := fmt.Sprintf("https://github.com/kaicontext/kai-cli/releases/latest/download/%s.gz", binaryName)

	fmt.Printf("Current version: %s\n", Version)

	// Check latest version from GitHub API
	latestResp, err := http.Get("https://api.github.com/repos/kaicontext/kai-cli/releases/latest")
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer latestResp.Body.Close()
	var releaseInfo struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(latestResp.Body).Decode(&releaseInfo); err == nil && releaseInfo.TagName != "" {
		latestVersion := strings.TrimPrefix(releaseInfo.TagName, "v")
		if latestVersion == Version {
			fmt.Printf("Already on the latest version (%s)\n", Version)
			return nil
		}
		fmt.Printf("Latest version:  %s\n", latestVersion)
	}

	// Check if binary is available (may not be uploaded yet if release just created)
	resp, err := http.Head(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Println("Binary not available yet — the release build may still be in progress.")
		fmt.Println("Try again in a minute, or reinstall via:")
		fmt.Println("  curl -sSL https://get.kaicontext.com | sh")
		return nil
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status checking for updates: %d", resp.StatusCode)
	}

	fmt.Println("Downloading update...")

	if checkOnly {
		fmt.Printf("Download URL: %s\n", downloadURL)
		return nil
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	fmt.Printf("Downloading %s...\n", binaryName)

	// Download the gzipped binary
	resp, err = http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status: %d", resp.StatusCode)
	}

	// Create temp file
	tmpFile, err := os.CreateTemp("", "kai-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Decompress gzip
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to decompress: %w", err)
	}

	_, err = io.Copy(tmpFile, gzReader)
	gzReader.Close()
	tmpFile.Close()
	if err != nil {
		return fmt.Errorf("failed to write update: %w", err)
	}

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to make executable: %w", err)
	}

	// Replace current binary
	// On Unix, we can rename over the running binary
	if err := os.Rename(tmpPath, execPath); err != nil {
		// If rename fails (e.g., cross-device), try copy
		src, err := os.Open(tmpPath)
		if err != nil {
			return fmt.Errorf("failed to open temp file: %w", err)
		}
		defer src.Close()

		dst, err := os.OpenFile(execPath, os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to open destination (try sudo?): %w", err)
		}
		defer dst.Close()

		if _, err := io.Copy(dst, src); err != nil {
			return fmt.Errorf("failed to copy update: %w", err)
		}
	}

	fmt.Println("Updated successfully!")
	fmt.Println("Run 'kai --version' to verify.")
	return nil
}

// ========== Shadow Run Implementation ==========

// JUnit XML types for parsing test results
type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Errors    int             `xml:"errors,attr"`
	Skipped   int             `xml:"skipped,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure"`
	Error     *junitError   `xml:"error"`
	Skipped   *junitSkipped `xml:"skipped"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
}

type junitError struct {
	Message string `xml:"message,attr"`
}

type junitSkipped struct{}

// parseJUnitXML parses JUnit XML test results
func parseJUnitXML(data []byte) (total int, failed []TestFailureDetail, err error) {
	extractMsg := func(tc junitTestCase) string {
		if tc.Failure != nil && tc.Failure.Message != "" {
			return tc.Failure.Message
		}
		if tc.Error != nil && tc.Error.Message != "" {
			return tc.Error.Message
		}
		return ""
	}

	// Try <testsuites> root
	var suites junitTestSuites
	if err := xml.Unmarshal(data, &suites); err == nil && len(suites.TestSuites) > 0 {
		for _, suite := range suites.TestSuites {
			for _, tc := range suite.TestCases {
				total++
				if tc.Failure != nil || tc.Error != nil {
					name := tc.Name
					if tc.ClassName != "" {
						name = tc.ClassName + "::" + tc.Name
					}
					failed = append(failed, TestFailureDetail{Name: name, ErrorMessage: extractMsg(tc)})
				}
			}
		}
		return total, failed, nil
	}

	// Try single <testsuite> root
	var suite junitTestSuite
	if err := xml.Unmarshal(data, &suite); err == nil && len(suite.TestCases) > 0 {
		for _, tc := range suite.TestCases {
			total++
			if tc.Failure != nil || tc.Error != nil {
				name := tc.Name
				if tc.ClassName != "" {
					name = tc.ClassName + "::" + tc.Name
				}
				failed = append(failed, TestFailureDetail{Name: name, ErrorMessage: extractMsg(tc)})
			}
		}
		return total, failed, nil
	}

	return 0, nil, fmt.Errorf("not valid JUnit XML")
}

// extractTestResultsJest parses Jest JSON output
func extractTestResultsJest(data []byte) (total int, failed []TestFailureDetail, err error) {
	var result struct {
		NumTotalTests  int `json:"numTotalTests"`
		NumPassedTests int `json:"numPassedTests"`
		TestResults    []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"testResults"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, nil, err
	}
	if len(result.TestResults) == 0 {
		return 0, nil, fmt.Errorf("no test results found")
	}
	total = result.NumTotalTests
	if total == 0 {
		total = len(result.TestResults)
	}
	for _, tr := range result.TestResults {
		if tr.Status == "failed" {
			failed = append(failed, TestFailureDetail{Name: tr.Name, ErrorMessage: tr.Message})
		}
	}
	return total, failed, nil
}

// extractTestResultsPytest parses pytest JSON output
func extractTestResultsPytest(data []byte) (total int, failed []TestFailureDetail, err error) {
	var result struct {
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Tests []struct {
			NodeID  string `json:"nodeid"`
			Outcome string `json:"outcome"`
			Call    struct {
				Longrepr string `json:"longrepr"`
			} `json:"call"`
		} `json:"tests"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, nil, err
	}
	if len(result.Tests) == 0 {
		return 0, nil, fmt.Errorf("no test results found")
	}
	total = result.Summary.Total
	if total == 0 {
		total = len(result.Tests)
	}
	for _, t := range result.Tests {
		if t.Outcome == "failed" {
			failed = append(failed, TestFailureDetail{Name: t.NodeID, ErrorMessage: t.Call.Longrepr})
		}
	}
	return total, failed, nil
}

// extractTestResultsGo parses Go test JSON output (go test -json)
func extractTestResultsGo(data []byte) (total int, failed []TestFailureDetail, err error) {
	testsSeen := make(map[string]string)  // pkg/Test -> last action
	testOutput := make(map[string]string) // pkg/Test -> accumulated output
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
			Output  string `json:"Output"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}
		key := ev.Package + "/" + ev.Test
		if ev.Action == "output" && ev.Output != "" {
			testOutput[key] += ev.Output
		}
		if ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip" {
			testsSeen[key] = ev.Action
		}
	}
	if len(testsSeen) == 0 {
		return 0, nil, fmt.Errorf("no Go test events found")
	}
	var failedNames []string
	for key, action := range testsSeen {
		total++
		if action == "fail" {
			failedNames = append(failedNames, key)
		}
	}
	sort.Strings(failedNames)
	for _, name := range failedNames {
		failed = append(failed, TestFailureDetail{Name: name, ErrorMessage: testOutput[name]})
	}
	return total, failed, nil
}

// extractTestResults parses test output in the specified format
func extractTestResults(data []byte, format string) (total int, failed []TestFailureDetail, err error) {
	switch format {
	case "junit":
		return parseJUnitXML(data)
	case "jest":
		return extractTestResultsJest(data)
	case "pytest":
		return extractTestResultsPytest(data)
	case "go":
		return extractTestResultsGo(data)
	case "auto", "":
		// Try JUnit XML first
		if t, f, err := parseJUnitXML(data); err == nil && t > 0 {
			return t, f, nil
		}
		// Try Jest
		if t, f, err := extractTestResultsJest(data); err == nil && t > 0 {
			return t, f, nil
		}
		// Try pytest
		if t, f, err := extractTestResultsPytest(data); err == nil && t > 0 {
			return t, f, nil
		}
		// Try Go
		if t, f, err := extractTestResultsGo(data); err == nil && t > 0 {
			return t, f, nil
		}
		return 0, nil, fmt.Errorf("could not detect test result format")
	default:
		return 0, nil, fmt.Errorf("unknown format: %s", format)
	}
}

// runTestCommand executes a test command and parses results
func runTestCommand(cmdStr string, tests []string, format string) (*ShadowRunResult, error) {
	// Substitute {{tests}} placeholder
	if len(tests) > 0 {
		cmdStr = strings.ReplaceAll(cmdStr, "{{tests}}", strings.Join(tests, " "))
	} else {
		cmdStr = strings.ReplaceAll(cmdStr, "{{tests}}", "")
	}

	result := &ShadowRunResult{
		Command:     cmdStr,
		FailedTests: []TestFailureDetail{},
	}

	start := time.Now()
	cmd := exec.Command("sh", "-c", cmdStr)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	result.DurationS = time.Since(start).Seconds()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("executing command: %w", err)
		}
	}

	output := buf.Bytes()

	// Try to find result files first
	resultFiles := []string{"junit.xml", "test-results.xml", "test-results.json"}
	for _, rf := range resultFiles {
		if data, err := os.ReadFile(rf); err == nil {
			if total, failed, err := extractTestResults(data, format); err == nil && total > 0 {
				result.TotalTests = total
				result.FailedTests = failed
				result.Passed = total - len(failed)
				return result, nil
			}
		}
	}

	// Parse stdout/stderr
	if total, failed, err := extractTestResults(output, format); err == nil && total > 0 {
		result.TotalTests = total
		result.FailedTests = failed
		result.Passed = total - len(failed)
		return result, nil
	}

	// Fall back to exit code only
	if result.ExitCode != 0 {
		result.FailedTests = []TestFailureDetail{{Name: "(exit code " + strconv.Itoa(result.ExitCode) + ")"}}
	}

	return result, nil
}

// detectFlakyTests re-runs failed tests and classifies each as flaky or real.
// If cmdStr contains {{tests}}, only the failed tests are rerun (targeted).
// Otherwise the full command is rerun.
func detectFlakyTests(cmdStr string, failedTests []TestFailureDetail, retries int, format string) (flaky []FlakyTestDetail, real []FlakyTestDetail) {
	if retries <= 0 || len(failedTests) == 0 {
		return nil, nil
	}

	// Build targeted command if possible
	failedNames := make([]string, len(failedTests))
	errorMsgs := make(map[string]string)
	for i, f := range failedTests {
		failedNames[i] = f.Name
		errorMsgs[f.Name] = f.ErrorMessage
	}

	targeted := strings.Contains(cmdStr, "{{tests}}")
	runCmd := cmdStr
	if targeted {
		runCmd = strings.ReplaceAll(cmdStr, "{{tests}}", strings.Join(failedNames, " "))
	}

	// Track per-test failure counts across retries
	failCounts := make(map[string]int)
	completedRetries := 0

	for i := 0; i < retries; i++ {
		fmt.Fprintf(os.Stderr, "  Flaky detection retry %d/%d...\n", i+1, retries)
		result, err := runTestCommand(runCmd, nil, format)
		if err != nil {
			continue
		}
		completedRetries++
		for _, f := range result.FailedTests {
			failCounts[f.Name]++
			// Keep the latest error message
			if f.ErrorMessage != "" {
				errorMsgs[f.Name] = f.ErrorMessage
			}
		}
	}

	if completedRetries == 0 {
		return nil, nil
	}

	// Classify each originally-failed test
	for _, name := range failedNames {
		count := failCounts[name]
		detail := FlakyTestDetail{
			Name:         name,
			ErrorMessage: errorMsgs[name],
			FailCount:    count,
			TotalRetries: completedRetries,
		}
		if count == 0 {
			detail.Classification = "flaky"
			detail.Confidence = 1.0
		} else if count < completedRetries {
			detail.Classification = "flaky"
			detail.Confidence = 1.0 - float64(count)/float64(completedRetries)
		} else {
			detail.Classification = "real"
			detail.Confidence = 1.0
		}

		if detail.Classification == "flaky" {
			flaky = append(flaky, detail)
		} else {
			real = append(real, detail)
		}
	}

	return flaky, real
}

// computeShadowMetrics computes comparison metrics between selective and full runs
func computeShadowMetrics(selective, full *ShadowRunResult, plan *CIPlan, flakyTests []FlakyTestDetail) ShadowMetrics {
	flakySet := make(map[string]bool)
	for _, f := range flakyTests {
		flakySet[f.Name] = true
	}

	// Targets selected by the plan
	selectedSet := make(map[string]bool)
	for _, t := range plan.Targets.Run {
		selectedSet[t] = true
	}

	// False negatives: tests that failed in full but weren't selected (excluding flaky)
	var falseNegatives int
	for _, f := range full.FailedTests {
		if !flakySet[f.Name] && !selectedSet[f.Name] {
			falseNegatives++
		}
	}

	// False positives: tests selected that passed in full run
	// (not directly measurable from failed lists, approximate)
	falsePositives := 0

	testsReduced := full.TotalTests - selective.TotalTests
	var testsReducedPct float64
	if full.TotalTests > 0 {
		testsReducedPct = float64(testsReduced) / float64(full.TotalTests) * 100
	}

	timeSaved := full.DurationS - selective.DurationS
	var timeSavedPct float64
	if full.DurationS > 0 {
		timeSavedPct = timeSaved / full.DurationS * 100
	}

	var accuracy float64
	nonFlakyFailed := 0
	for _, f := range full.FailedTests {
		if !flakySet[f.Name] {
			nonFlakyFailed++
		}
	}
	if nonFlakyFailed > 0 {
		accuracy = 1.0 - float64(falseNegatives)/float64(nonFlakyFailed)
	} else {
		accuracy = 1.0 // No real failures = perfect accuracy
	}

	return ShadowMetrics{
		TestsReduced:    testsReduced,
		TestsReducedPct: testsReducedPct,
		TimeSavedS:      timeSaved,
		TimeSavedPct:    timeSavedPct,
		FalseNegatives:  falseNegatives,
		FalsePositives:  falsePositives,
		Accuracy:        accuracy,
	}
}

var ciCommentCmd = &cobra.Command{
	Use:   "comment --report <shadow-report.json>",
	Short: "Post a CI summary comment on a GitHub Pull Request",
	Long: `Reads a shadow report JSON and posts a formatted summary as a PR comment.

Auto-detects GitHub context from environment variables:
  GITHUB_TOKEN       - API token for authentication
  GITHUB_REPOSITORY  - owner/repo
  GITHUB_EVENT_PATH  - event payload (to extract PR number)
  GITHUB_EVENT_NAME  - must be "pull_request" for auto-posting

Use --dry-run to print the comment to stdout without posting.

Examples:
  kai ci comment --report shadow-report.json
  kai ci comment --report shadow-report.json --dry-run
  kai ci comment --report shadow-report.json --token $TOKEN --repo owner/name --pr 42`,
	Args: cobra.NoArgs,
	RunE: runCIComment,
}

func runCIComment(cmd *cobra.Command, args []string) error {
	// Read the shadow report
	data, err := os.ReadFile(ciCommentReport)
	if err != nil {
		return fmt.Errorf("reading report: %w", err)
	}

	var report ShadowReport
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("parsing report: %w", err)
	}

	// Format the comment
	comment := formatPRComment(report)

	// Dry run: print to stdout
	if ciCommentDryRun {
		fmt.Println(comment)
		return nil
	}

	// Resolve GitHub context
	token := ciCommentToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	repo := ciCommentRepo
	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	prNum := ciCommentPR
	if prNum == 0 {
		prNum = detectPRNumber()
	}

	// If not in a PR context, just print to stdout
	if token == "" || repo == "" || prNum == 0 {
		if os.Getenv("GITHUB_EVENT_NAME") == "push" {
			fmt.Fprintf(os.Stderr, "Push event detected, skipping PR comment.\n")
			return nil
		}
		if token == "" {
			fmt.Fprintf(os.Stderr, "No GitHub token available, printing to stdout.\n")
		} else if prNum == 0 {
			fmt.Fprintf(os.Stderr, "No PR number detected, printing to stdout.\n")
		}
		fmt.Println(comment)
		return nil
	}

	// Post or update the comment
	if err := postOrUpdateGitHubComment(token, repo, prNum, comment); err != nil {
		// Non-fatal: print to stdout as fallback
		fmt.Fprintf(os.Stderr, "Warning: could not post GitHub comment: %v\n", err)
		fmt.Fprintf(os.Stderr, "Printing to stdout instead.\n")
		fmt.Println(comment)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Posted Kai CI summary to PR #%d\n", prNum)
	return nil
}

const kaiCommentMarker = "<!-- kai-ci-summary -->"
const kaiAuthorshipMarker = "<!-- kai-authorship-summary -->"

var ciAuthorshipCmd = &cobra.Command{
	Use:   "authorship",
	Short: "Post AI authorship summary as a GitHub PR comment",
	Long: `Queries authorship data from the latest capture and posts a summary
showing AI vs human code attribution as a PR comment.

Auto-detects GitHub context from environment variables:
  GITHUB_TOKEN       - API token for authentication
  GITHUB_REPOSITORY  - owner/repo
  GITHUB_EVENT_PATH  - event payload (to extract PR number)

Examples:
  kai ci authorship
  kai ci authorship --dry-run
  kai ci authorship --token $TOKEN --repo owner/name --pr 42`,
	Args: cobra.NoArgs,
	RunE: runCIAuthorship,
}

func runCIAuthorship(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Resolve latest snapshot
	resolver := ref.NewResolver(db)
	kind := ref.KindSnapshot
	result, err := resolver.Resolve("@snap:last", &kind)
	if err != nil {
		return fmt.Errorf("no snapshots found — run 'kai capture' first")
	}
	snapID := result.ID

	// Get project stats
	stats, err := authorship.ProjectStats(db, snapID)
	if err != nil {
		return fmt.Errorf("computing authorship stats: %w", err)
	}

	// Get per-file summaries for changed files
	// Use all authorship ranges to find which files have data
	allRanges, err := db.GetAllAuthorshipRanges(snapID)
	if err != nil {
		return fmt.Errorf("querying authorship ranges: %w", err)
	}

	// Group by file for per-file stats
	fileLines := make(map[string]struct{ ai, total int })
	fileAgents := make(map[string]map[string]bool)
	for _, r := range allRanges {
		lines := r.EndLine - r.StartLine + 1
		fl := fileLines[r.FilePath]
		fl.total += lines
		if r.AuthorType == "ai" {
			fl.ai += lines
			if fileAgents[r.FilePath] == nil {
				fileAgents[r.FilePath] = make(map[string]bool)
			}
			if r.Agent != "" {
				fileAgents[r.FilePath][r.Agent] = true
			}
		}
		fileLines[r.FilePath] = fl
	}

	// Format comment
	comment := formatAuthorshipComment(stats, fileLines, fileAgents)

	if ciAuthorshipDryRun {
		fmt.Println(comment)
		return nil
	}

	// Resolve GitHub context
	token := ciAuthorshipToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	repo := ciAuthorshipRepo
	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	prNum := ciAuthorshipPR
	if prNum == 0 {
		prNum = detectPRNumber()
	}

	if token == "" || repo == "" || prNum == 0 {
		if token == "" {
			fmt.Fprintf(os.Stderr, "No GitHub token available, printing to stdout.\n")
		} else if prNum == 0 {
			fmt.Fprintf(os.Stderr, "No PR number detected, printing to stdout.\n")
		}
		fmt.Println(comment)
		return nil
	}

	if err := postOrUpdateGitHubComment(token, repo, prNum, comment); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not post GitHub comment: %v\n", err)
		fmt.Fprintf(os.Stderr, "Printing to stdout instead.\n")
		fmt.Println(comment)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Posted AI authorship summary to PR #%d\n", prNum)
	return nil
}

func formatAuthorshipComment(stats *authorship.ProjectSummary, fileLines map[string]struct{ ai, total int }, fileAgents map[string]map[string]bool) string {
	var b strings.Builder

	b.WriteString(kaiAuthorshipMarker + "\n")
	b.WriteString("## AI Authorship\n\n")

	if stats.TotalLines == 0 {
		b.WriteString("No authorship data found.\n")
		return b.String()
	}

	// Summary line
	humanPct := 100 - stats.AIPct
	b.WriteString(fmt.Sprintf("**%.0f%% AI-authored**, %.0f%% human (%d lines tracked)\n\n", stats.AIPct, humanPct, stats.TotalLines))

	// Agent breakdown
	if len(stats.ByAgent) > 0 {
		for agent, lines := range stats.ByAgent {
			pct := float64(lines) / float64(stats.TotalLines) * 100
			b.WriteString(fmt.Sprintf("- **%s**: %d lines (%.0f%%)\n", agent, lines, pct))
		}
		b.WriteString("\n")
	}

	// Per-file table (only files with AI contribution, max 20)
	type fileStat struct {
		path   string
		aiPct  float64
		ai     int
		total  int
		agents []string
	}
	var aiFiles []fileStat
	for path, fl := range fileLines {
		if fl.ai > 0 {
			pct := float64(fl.ai) / float64(fl.total) * 100
			var agents []string
			for a := range fileAgents[path] {
				agents = append(agents, a)
			}
			aiFiles = append(aiFiles, fileStat{path, pct, fl.ai, fl.total, agents})
		}
	}

	if len(aiFiles) > 0 {
		// Sort by AI percentage descending
		sort.Slice(aiFiles, func(i, j int) bool {
			return aiFiles[i].aiPct > aiFiles[j].aiPct
		})

		b.WriteString("<details>\n<summary>Files with AI-authored code</summary>\n\n")
		b.WriteString("| File | AI | Lines | Agent |\n")
		b.WriteString("|------|-----|-------|-------|\n")

		limit := len(aiFiles)
		if limit > 20 {
			limit = 20
		}
		for _, f := range aiFiles[:limit] {
			agent := "-"
			if len(f.agents) > 0 {
				agent = strings.Join(f.agents, ", ")
			}
			b.WriteString(fmt.Sprintf("| `%s` | %.0f%% | %d/%d | %s |\n", f.path, f.aiPct, f.ai, f.total, agent))
		}
		if len(aiFiles) > 20 {
			b.WriteString(fmt.Sprintf("\n*...and %d more files*\n", len(aiFiles)-20))
		}
		b.WriteString("\n</details>\n")
	}

	b.WriteString("\n---\n*Generated by [Kai](https://kailayer.com)*\n")

	return b.String()
}

func formatPRComment(report ShadowReport) string {
	var b strings.Builder

	b.WriteString(kaiCommentMarker + "\n")
	b.WriteString("## Kai CI Summary\n\n")

	// Verdict banner
	switch report.Verdict {
	case ShadowVerdictSafe:
		b.WriteString("**Safe** — No missed failures\n\n")
	case ShadowVerdictMissed:
		b.WriteString("**False negative detected** — Kai would have missed a failing test.\n")
		b.WriteString("Full suite required.\n\n")
	case ShadowVerdictFallback:
		reason := report.Fallback.Reason
		if reason == "" {
			reason = "risk threshold exceeded"
		}
		b.WriteString(fmt.Sprintf("**Fallback triggered:** %s\n", reason))
		b.WriteString("Full suite executed to preserve safety.\n\n")
	case ShadowVerdictFlakySuspect:
		b.WriteString("**Flaky tests detected** — results may be unreliable\n\n")
	default:
		b.WriteString(fmt.Sprintf("**Verdict:** %s\n\n", report.Verdict))
	}

	// CI Time
	b.WriteString("**CI Time**\n")
	if report.FullRun != nil {
		b.WriteString(fmt.Sprintf("Full Suite: %s\n", formatDuration(report.FullRun.DurationS)))
	}
	if report.SelectiveRun != nil {
		b.WriteString(fmt.Sprintf("Kai Plan: %s\n", formatDuration(report.SelectiveRun.DurationS)))
	}
	if report.Metrics.TimeSavedPct > 0 {
		b.WriteString(fmt.Sprintf("Savings: **%.0f%%**\n", report.Metrics.TimeSavedPct))
	} else if report.Metrics.TimeSavedPct < 0 {
		b.WriteString(fmt.Sprintf("Overhead: **%.0f%%**\n", -report.Metrics.TimeSavedPct))
	}
	b.WriteString("\n")

	// Test Selection
	b.WriteString("**Test Selection**\n")
	if report.FullRun != nil {
		b.WriteString(fmt.Sprintf("Total Tests: %s\n", formatNumber(report.FullRun.TotalTests)))
	}
	if report.SelectiveRun != nil {
		b.WriteString(fmt.Sprintf("Selected: %s\n", formatNumber(report.SelectiveRun.TotalTests)))
	}
	if report.Metrics.TestsReducedPct > 0 {
		b.WriteString(fmt.Sprintf("Reduction: **%.0f%%**\n", report.Metrics.TestsReducedPct))
	}
	b.WriteString("\n")

	// Safety details
	b.WriteString("**Safety**\n")
	if report.Plan != nil {
		confLabel := "Low"
		if report.Plan.Confidence >= 0.8 {
			confLabel = "High"
		} else if report.Plan.Confidence >= 0.5 {
			confLabel = "Medium"
		}
		b.WriteString(fmt.Sprintf("Confidence: %s (%.0f%%)\n", confLabel, report.Plan.Confidence*100))
	}
	b.WriteString(fmt.Sprintf("False Negatives: %d\n", report.Metrics.FalseNegatives))
	b.WriteString(fmt.Sprintf("Fallback: %s\n", boolYesNo(report.Fallback.Triggered)))
	b.WriteString("\n")

	// Flaky tests (if any)
	if report.Flaky.Detected && len(report.Flaky.FlakyTests) > 0 {
		b.WriteString("<details>\n<summary>Flaky tests detected</summary>\n\n")
		for _, t := range report.Flaky.FlakyTests {
			b.WriteString(fmt.Sprintf("- `%s` (failed %d/%d retries)\n", t.Name, t.FailCount, t.TotalRetries))
		}
		b.WriteString("\n</details>\n\n")
	}

	// Footer
	kaiVer := report.KaiVersion
	if kaiVer == "" {
		kaiVer = Version
	}
	b.WriteString(fmt.Sprintf("_Kai %s_\n", kaiVer))

	return b.String()
}

func formatDuration(seconds float64) string {
	if seconds >= 60 {
		m := int(seconds) / 60
		s := int(seconds) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%.1fs", seconds)
}

func formatNumber(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return strconv.Itoa(n)
}

func boolYesNo(v bool) string {
	if v {
		return "Yes"
	}
	return "No"
}

func detectPRNumber() int {
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return 0
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		return 0
	}
	var event struct {
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Number int `json:"number"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return 0
	}
	if event.PullRequest.Number > 0 {
		return event.PullRequest.Number
	}
	return event.Number
}

func postOrUpdateGitHubComment(token, repo string, prNum int, body string) error {
	apiBase := "https://api.github.com"

	// List existing comments to find one with our marker
	listURL := fmt.Sprintf("%s/repos/%s/issues/%d/comments?per_page=100", apiBase, repo, prNum)

	req, err := http.NewRequest("GET", listURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("listing comments: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("listing comments: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var comments []struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return fmt.Errorf("parsing comments: %w", err)
	}

	// Find existing Kai comment
	var existingID int
	for _, c := range comments {
		if strings.Contains(c.Body, kaiCommentMarker) {
			existingID = c.ID
			break
		}
	}

	payload, _ := json.Marshal(map[string]string{"body": body})

	if existingID > 0 {
		// Update existing comment
		updateURL := fmt.Sprintf("%s/repos/%s/issues/comments/%d", apiBase, repo, existingID)
		req, err = http.NewRequest("PATCH", updateURL, bytes.NewReader(payload))
	} else {
		// Create new comment
		createURL := fmt.Sprintf("%s/repos/%s/issues/%d/comments", apiBase, repo, prNum)
		req, err = http.NewRequest("POST", createURL, bytes.NewReader(payload))
	}
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("posting comment: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// writeShadowJSON writes the shadow report as JSON
func writeShadowJSON(report ShadowReport, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// writeShadowMarkdown writes a markdown summary of the shadow report
func writeShadowMarkdown(report ShadowReport, path string) error {
	var b strings.Builder

	verdictEmoji := map[ShadowVerdict]string{
		ShadowVerdictSafe:         "OK",
		ShadowVerdictMissed:       "MISS",
		ShadowVerdictFallback:     "FALLBACK",
		ShadowVerdictFlakySuspect: "FLAKY",
		ShadowVerdictSkipped:      "SKIP",
	}

	b.WriteString("# Shadow Run Report\n\n")
	b.WriteString(fmt.Sprintf("**Verdict:** %s %s\n\n", verdictEmoji[report.Verdict], report.Verdict))
	b.WriteString(fmt.Sprintf("- **Git Range:** `%s`\n", report.GitRange))
	b.WriteString(fmt.Sprintf("- **Generated:** %s\n", report.GeneratedAt))
	b.WriteString(fmt.Sprintf("- **Kai Version:** %s\n\n", report.KaiVersion))

	b.WriteString("## Metrics\n\n")
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	b.WriteString(fmt.Sprintf("| Tests reduced | %d (%.1f%%) |\n", report.Metrics.TestsReduced, report.Metrics.TestsReducedPct))
	b.WriteString(fmt.Sprintf("| Time saved | %.1fs (%.1f%%) |\n", report.Metrics.TimeSavedS, report.Metrics.TimeSavedPct))
	b.WriteString(fmt.Sprintf("| False negatives | %d |\n", report.Metrics.FalseNegatives))
	b.WriteString(fmt.Sprintf("| Accuracy | %.1f%% |\n\n", report.Metrics.Accuracy*100))

	if report.SelectiveRun != nil {
		b.WriteString("## Selective Run\n\n")
		b.WriteString(fmt.Sprintf("- **Command:** `%s`\n", report.SelectiveRun.Command))
		b.WriteString(fmt.Sprintf("- **Exit code:** %d\n", report.SelectiveRun.ExitCode))
		b.WriteString(fmt.Sprintf("- **Duration:** %.1fs\n", report.SelectiveRun.DurationS))
		b.WriteString(fmt.Sprintf("- **Tests:** %d total, %d passed, %d failed\n\n", report.SelectiveRun.TotalTests, report.SelectiveRun.Passed, len(report.SelectiveRun.FailedTests)))
	}

	if report.FullRun != nil {
		b.WriteString("## Full Run\n\n")
		b.WriteString(fmt.Sprintf("- **Command:** `%s`\n", report.FullRun.Command))
		b.WriteString(fmt.Sprintf("- **Exit code:** %d\n", report.FullRun.ExitCode))
		b.WriteString(fmt.Sprintf("- **Duration:** %.1fs\n", report.FullRun.DurationS))
		b.WriteString(fmt.Sprintf("- **Tests:** %d total, %d passed, %d failed\n\n", report.FullRun.TotalTests, report.FullRun.Passed, len(report.FullRun.FailedTests)))
	}

	if report.Flaky.Detected {
		b.WriteString("## Flaky Tests\n\n")
		for _, t := range report.Flaky.FlakyTests {
			b.WriteString(fmt.Sprintf("- `%s` — %s (confidence: %.0f%%, failed %d/%d retries)\n",
				t.Name, t.Classification, t.Confidence*100, t.FailCount, t.TotalRetries))
			if t.ErrorMessage != "" {
				// Show first line of error message
				msg := t.ErrorMessage
				if idx := strings.Index(msg, "\n"); idx > 0 {
					msg = msg[:idx]
				}
				if len(msg) > 120 {
					msg = msg[:120] + "..."
				}
				b.WriteString(fmt.Sprintf("  > %s\n", msg))
			}
		}
		b.WriteString("\n")
	}

	if len(report.Flaky.RealTests) > 0 {
		b.WriteString("## Real Failures\n\n")
		for _, t := range report.Flaky.RealTests {
			b.WriteString(fmt.Sprintf("- `%s` (failed %d/%d retries)\n", t.Name, t.FailCount, t.TotalRetries))
			if t.ErrorMessage != "" {
				msg := t.ErrorMessage
				if idx := strings.Index(msg, "\n"); idx > 0 {
					msg = msg[:idx]
				}
				if len(msg) > 120 {
					msg = msg[:120] + "..."
				}
				b.WriteString(fmt.Sprintf("  > %s\n", msg))
			}
		}
		b.WriteString("\n")
	}

	if report.Metrics.FalseNegatives > 0 && report.FullRun != nil {
		selectedSet := make(map[string]bool)
		if report.Plan != nil {
			for _, t := range report.Plan.Targets.Run {
				selectedSet[t] = true
			}
		}
		b.WriteString("## False Negatives (Missed Failures)\n\n")
		for _, f := range report.FullRun.FailedTests {
			if !selectedSet[f.Name] {
				b.WriteString(fmt.Sprintf("- `%s`\n", f.Name))
			}
		}
		b.WriteString("\n")
	}

	if report.Fallback.Triggered {
		b.WriteString("## Fallback\n\n")
		b.WriteString(fmt.Sprintf("- **Reason:** %s\n", report.Fallback.Reason))
		b.WriteString(fmt.Sprintf("- **Confidence:** %.0f%%\n\n", report.Fallback.Confidence*100))
	}

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// printShadowSummary prints a summary to stderr
func printShadowSummary(report ShadowReport) {
	verdictSymbol := map[ShadowVerdict]string{
		ShadowVerdictSafe:         "[SAFE]",
		ShadowVerdictMissed:       "[MISSED]",
		ShadowVerdictFallback:     "[FALLBACK]",
		ShadowVerdictFlakySuspect: "[FLAKY]",
		ShadowVerdictSkipped:      "[SKIPPED]",
	}

	fmt.Fprintf(os.Stderr, "\n╔══════════════════════════════════════╗\n")
	fmt.Fprintf(os.Stderr, "║  Shadow Run: %-23s ║\n", verdictSymbol[report.Verdict])
	fmt.Fprintf(os.Stderr, "╠══════════════════════════════════════╣\n")
	fmt.Fprintf(os.Stderr, "║  Tests reduced: %4d (%5.1f%%)        ║\n", report.Metrics.TestsReduced, report.Metrics.TestsReducedPct)
	fmt.Fprintf(os.Stderr, "║  Time saved:   %5.1fs (%5.1f%%)       ║\n", report.Metrics.TimeSavedS, report.Metrics.TimeSavedPct)
	fmt.Fprintf(os.Stderr, "║  False negatives: %d                 ║\n", report.Metrics.FalseNegatives)
	fmt.Fprintf(os.Stderr, "║  Accuracy: %5.1f%%                    ║\n", report.Metrics.Accuracy*100)
	fmt.Fprintf(os.Stderr, "╚══════════════════════════════════════╝\n\n")
}

// extractAllTestFilesFromCoverage collects unique test file paths from coverage map entries.
func extractAllTestFilesFromCoverage(cm *CoverageMap) []string {
	seen := make(map[string]bool)
	for _, entries := range cm.Entries {
		for _, entry := range entries {
			if entry.TestID != "" && entry.TestID != "aggregate" {
				seen[entry.TestID] = true
			}
		}
	}
	// Also include coverage map keys that are test files
	for path := range cm.Entries {
		if parse.IsTestFile(path) {
			seen[path] = true
		}
	}
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

// generateCIPlanFast creates a CI plan using git diff + coverage map, skipping full snapshots.
func generateCIPlanFast(gitBase, gitHead, repoPath string, coverageMap *CoverageMap) (*CIPlan, func(), error) {
	start := time.Now()

	// Open repo (for path resolution only)
	repo, err := gitio.Open(repoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening repo: %w", err)
	}

	// Use native git diff — instant even on large repos
	added, modified, deleted, err := repo.DiffFilesNative(gitBase, gitHead)
	if err != nil {
		return nil, nil, fmt.Errorf("computing diff: %w", err)
	}
	changedFiles := make([]string, 0, len(added)+len(modified)+len(deleted))
	changedFiles = append(changedFiles, added...)
	changedFiles = append(changedFiles, modified...)
	changedFiles = append(changedFiles, deleted...)

	if len(changedFiles) == 0 {
		plan := CIPlan{
			Version:      1,
			Mode:         "skip",
			Risk:         "low",
			SafetyMode:   "shadow",
			Confidence:   1.0,
			Targets:      CITargets{Run: []string{}, Skip: []string{}, Full: []string{}, Tags: make(map[string][]string)},
			Impact:       CIImpact{FilesChanged: []string{}, SymbolsChanged: []CISymbolChange{}, ModulesAffected: []string{}, Uncertainty: []string{}},
			Policy:       CIPolicy{Strategy: "coverage-fast"},
			Safety:       CISafety{StructuralRisks: []StructuralRisk{}, Confidence: 1.0, ExpansionReasons: []string{}},
			Uncertainty:  CIUncertainty{Sources: []string{}},
			ExpansionLog: []string{},
			Provenance: CIProvenance{
				KaiVersion:      Version,
				DetectorVersion: DetectorVersion,
				GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
				Analyzers:       []string{"coverage-fast@1"},
			},
		}
		elapsed := time.Since(start)
		fmt.Fprintf(os.Stderr, "Fast path: 0 changed files, skip (%.1fs)\n", elapsed.Seconds())
		return &plan, func() {}, nil
	}

	// Load CI policy for MinHits threshold
	ciPolicy, policyHash, err := loadCIPolicy()
	if err != nil {
		return nil, nil, fmt.Errorf("loading CI policy: %w", err)
	}
	envHash := computeEnvHash(ciPolicy.EnvVars)
	panicSwitch := checkPanicSwitch()

	// Get all test files from coverage map
	allTestFiles := extractAllTestFilesFromCoverage(coverageMap)

	// Look up changed files in coverage map
	affectedTargets := make(map[string]bool)
	filesWithCoverage := 0
	filesWithoutCoverage := 0

	for _, path := range changedFiles {
		if parse.IsTestFile(path) {
			affectedTargets[path] = true // Changed test always runs
			continue
		}
		entries, has := coverageMap.Entries[path]
		if !has {
			// Try suffix matching (same logic as existing full path)
			for mapPath, mapEntries := range coverageMap.Entries {
				if strings.HasSuffix(mapPath, path) || strings.HasSuffix(path, mapPath) {
					entries = mapEntries
					has = true
					break
				}
			}
		}
		if has {
			filesWithCoverage++
			for _, entry := range entries {
				if entry.TestID != "aggregate" && entry.TestID != "" {
					if entry.HitCount >= ciPolicy.Coverage.MinHits {
						affectedTargets[entry.TestID] = true
					}
				}
			}
		} else {
			filesWithoutCoverage++
		}
	}

	// Build the plan
	plan := CIPlan{
		Version:    1,
		Mode:       "selective",
		Risk:       "low",
		SafetyMode: "shadow",
		Confidence: 1.0,
		Targets: CITargets{
			Run:  []string{},
			Skip: []string{},
			Full: allTestFiles,
			Tags: make(map[string][]string),
		},
		Impact: CIImpact{
			FilesChanged:    changedFiles,
			SymbolsChanged:  []CISymbolChange{},
			ModulesAffected: []string{},
			Uncertainty:     []string{},
		},
		Policy: CIPolicy{
			Strategy: "coverage-fast",
		},
		Safety: CISafety{
			StructuralRisks:  []StructuralRisk{},
			Confidence:       1.0,
			PanicSwitch:      panicSwitch,
			ExpansionReasons: []string{},
		},
		Uncertainty: CIUncertainty{
			Sources: []string{},
		},
		ExpansionLog: []string{},
		Provenance: CIProvenance{
			KaiVersion:      Version,
			DetectorVersion: DetectorVersion,
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			Analyzers:       []string{"coverage-fast@1"},
			PolicyHash:      policyHash,
			EnvHash:         envHash,
		},
	}

	if panicSwitch {
		plan.Mode = "full"
		plan.Targets.Run = allTestFiles
		plan.Safety.RecommendFull = true
		plan.Safety.RecommendReason = "Panic switch activated"
	} else {
		for t := range affectedTargets {
			plan.Targets.Run = append(plan.Targets.Run, t)
		}
		sort.Strings(plan.Targets.Run)

		plan.Prediction = CIPrediction{
			SelectiveTests: len(plan.Targets.Run),
			FullTests:      len(allTestFiles),
		}
		if len(allTestFiles) > 0 {
			plan.Prediction.PredictedSavings = float64(len(allTestFiles)-len(plan.Targets.Run)) / float64(len(allTestFiles)) * 100
		}

		// Safety: lower confidence if changed files have no coverage
		confidence := 1.0
		if filesWithoutCoverage > 0 {
			// Scale confidence down based on uncovered files
			coveredRatio := float64(filesWithCoverage) / float64(filesWithCoverage+filesWithoutCoverage)
			confidence = 0.5 + 0.5*coveredRatio
			if confidence < 0.7 {
				plan.Risk = "medium"
			}
			plan.Safety.StructuralRisks = append(plan.Safety.StructuralRisks, StructuralRisk{
				Type:        "uncovered-files",
				Description: fmt.Sprintf("%d changed files have no coverage data", filesWithoutCoverage),
			})
		}
		if len(plan.Targets.Run) == 0 && len(changedFiles) > 0 {
			confidence = 0.5
			plan.Risk = "medium"
		}
		plan.Safety.Confidence = confidence
		plan.Confidence = confidence
	}

	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "Fast path: %d changed files, %d tests selected (%.1fs)\n",
		len(changedFiles), len(plan.Targets.Run), elapsed.Seconds())

	return &plan, func() {}, nil
}

// generateCIPlanFromGitRange creates an ephemeral CI plan from a git range
func generateCIPlanFromGitRange(gitRange, repoPath string) (*CIPlan, func(), error) {
	parts := strings.Split(gitRange, "..")
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("invalid --git-range format: expected BASE..HEAD (e.g., main..feature)")
	}
	gitBase, gitHead := parts[0], parts[1]

	// Fast path: use git diff + coverage map when available
	coverageMap := loadOrCreateCoverageMap()
	hasCoverage := coverageMap != nil && len(coverageMap.Entries) > 0
	if hasCoverage && !ciNoFast {
		plan, cleanup, err := generateCIPlanFast(gitBase, gitHead, repoPath, coverageMap)
		if err == nil {
			return plan, cleanup, nil
		}
		// Fast path failed, fall through to full snapshot
		fmt.Fprintf(os.Stderr, "Fast path failed (%v), falling back to full snapshot\n", err)
	}

	// Create temp database
	tmpDir, err := os.MkdirTemp("", "kai-ci-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	dbPath := filepath.Join(tmpDir, "db.sqlite")
	objDir := filepath.Join(tmpDir, "objects")
	os.MkdirAll(objDir, 0755)
	db, err := graph.Open(dbPath, objDir)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating temp database: %w", err)
	}

	if err := applyDBSchema(db); err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("applying schema: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Creating snapshot from git ref: %s\n", gitBase)
	baseSnapshotID, err := createSnapshotFromGitRef(db, repoPath, gitBase)
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("creating base snapshot: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Creating snapshot from git ref: %s\n", gitHead)
	headSnapshotID, err := createSnapshotFromGitRef(db, repoPath, gitHead)
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("creating head snapshot: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Creating changeset...\n")
	changesetID, err := createChangesetFromSnapshots(db, baseSnapshotID, headSnapshotID, "")
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("creating changeset: %w", err)
	}

	matcher, _ := loadMatcher()
	if matcher == nil {
		matcher = module.NewMatcher(nil)
	}
	creator := snapshot.NewCreator(db, matcher)

	changedFiles, err := getChangedFiles(db, creator, baseSnapshotID, headSnapshotID)
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("getting changed files: %w", err)
	}

	panicSwitch := checkPanicSwitch()
	ciPolicy, policyHash, err := loadCIPolicy()
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("loading CI policy: %w", err)
	}
	envHash := computeEnvHash(ciPolicy.EnvVars)

	var changesetHex, baseHex, headHex string
	if changesetID != nil {
		changesetHex = util.BytesToHex(changesetID)
	}
	if baseSnapshotID != nil {
		baseHex = util.BytesToHex(baseSnapshotID)
	}
	if headSnapshotID != nil {
		headHex = util.BytesToHex(headSnapshotID)
	}

	analyzersUsed := []string{}

	plan := CIPlan{
		Version:    1,
		Mode:       "selective",
		Risk:       "low",
		SafetyMode: "shadow",
		Confidence: 1.0,
		Targets: CITargets{
			Run:      []string{},
			Skip:     []string{},
			Full:     []string{},
			Tags:     make(map[string][]string),
			Fallback: false,
		},
		Impact: CIImpact{
			FilesChanged:    changedFiles,
			SymbolsChanged:  []CISymbolChange{},
			ModulesAffected: []string{},
			Uncertainty:     []string{},
		},
		Policy: CIPolicy{
			Strategy:     "auto",
			Expanded:     false,
			FallbackUsed: "",
		},
		Safety: CISafety{
			StructuralRisks:  []StructuralRisk{},
			Confidence:       1.0,
			RecommendFull:    false,
			PanicSwitch:      panicSwitch,
			AutoExpanded:     false,
			ExpansionReasons: []string{},
		},
		Uncertainty: CIUncertainty{
			Score:   0,
			Sources: []string{},
		},
		ExpansionLog: []string{},
		Provenance: CIProvenance{
			Changeset:       changesetHex,
			Base:            baseHex,
			Head:            headHex,
			KaiVersion:      Version,
			DetectorVersion: DetectorVersion,
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			Analyzers:       analyzersUsed,
			PolicyHash:      policyHash,
			EnvHash:         envHash,
		},
		Prediction: CIPrediction{},
	}

	files, err := creator.GetSnapshotFiles(headSnapshotID)
	if err != nil {
		db.Close()
		cleanup()
		return nil, nil, fmt.Errorf("getting snapshot files: %w", err)
	}

	allTestFiles := getAllTestFiles(files)

	if panicSwitch {
		plan.Mode = "full"
		plan.Targets.Run = allTestFiles
		plan.Safety.RecommendFull = true
		plan.Safety.RecommendReason = "Panic switch activated"
	} else if len(changedFiles) == 0 {
		plan.Mode = "skip"
	} else {
		affectedTargets := make(map[string]bool)
		filePathByID := make(map[string]string)
		for _, f := range files {
			path, _ := f.Payload["path"].(string)
			idHex := util.BytesToHex(f.ID)
			filePathByID[idHex] = path
		}

		// Use import graph
		analyzersUsed = append(analyzersUsed, "imports@1")
		for _, changedPath := range changedFiles {
			testsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeTests)
			for _, e := range testsEdges {
				srcHex := util.BytesToHex(e.Src)
				if path, ok := filePathByID[srcHex]; ok {
					affectedTargets[path] = true
				} else if srcNode, err := db.GetNode(e.Src); err == nil && srcNode != nil {
					if srcPath, ok := srcNode.Payload["path"].(string); ok {
						affectedTargets[srcPath] = true
					}
				}
			}
			importsEdges, _ := db.GetEdgesToByPath(changedPath, graph.EdgeImports)
			for _, e := range importsEdges {
				srcHex := util.BytesToHex(e.Src)
				if path, ok := filePathByID[srcHex]; ok {
					if parse.IsTestFile(path) {
						affectedTargets[path] = true
					}
				} else if srcNode, err := db.GetNode(e.Src); err == nil && srcNode != nil {
					if srcPath, ok := srcNode.Payload["path"].(string); ok {
						if parse.IsTestFile(srcPath) {
							affectedTargets[srcPath] = true
						}
					}
				}
			}
		}

		// Also check coverage if enabled
		if ciPolicy.Coverage.Enabled {
			coverageMap := loadOrCreateCoverageMap()
			for _, changedPath := range changedFiles {
				if parse.IsTestFile(changedPath) {
					continue
				}
				entries, hasCoverage := coverageMap.Entries[changedPath]
				if !hasCoverage {
					for mapPath, mapEntries := range coverageMap.Entries {
						if strings.HasSuffix(mapPath, changedPath) || strings.HasSuffix(changedPath, mapPath) {
							entries = mapEntries
							hasCoverage = true
							break
						}
					}
				}
				if hasCoverage {
					for _, entry := range entries {
						if entry.HitCount >= ciPolicy.Coverage.MinHits && entry.TestID != "aggregate" && entry.TestID != "" {
							affectedTargets[entry.TestID] = true
						}
					}
				}
			}
		}

		for t := range affectedTargets {
			plan.Targets.Run = append(plan.Targets.Run, t)
		}
		sort.Strings(plan.Targets.Run)

		plan.Targets.Full = allTestFiles
		plan.Prediction = CIPrediction{
			SelectiveTests: len(plan.Targets.Run),
			FullTests:      len(allTestFiles),
		}
		if len(allTestFiles) > 0 {
			plan.Prediction.PredictedSavings = float64(len(allTestFiles)-len(plan.Targets.Run)) / float64(len(allTestFiles)) * 100
		}

		confidence := 1.0
		if len(plan.Targets.Run) == 0 && len(changedFiles) > 0 {
			confidence = 0.5
			plan.Risk = "medium"
		}
		plan.Safety.Confidence = confidence
		plan.Confidence = confidence
		plan.Provenance.Analyzers = analyzersUsed
	}

	db.Close()

	return &plan, cleanup, nil
}

// runShadowRun is the main orchestration function for shadow run
func runShadowRun(cmd *cobra.Command, args []string) error {
	te := telemetry.NewEvent("shadow_run")
	defer te.Finish()

	fmt.Fprintf(os.Stderr, "Generating CI plan from git range: %s\n", shadowGitRange)
	plan, cleanup, err := generateCIPlanFromGitRange(shadowGitRange, shadowGitRepo)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return fmt.Errorf("generating CI plan: %w", err)
	}

	te.SetPhase("plan", 0)

	// Check fallback
	fallbackInfo := ShadowFallbackInfo{}
	if plan.Safety.RecommendFull {
		fallbackInfo.Triggered = true
		fallbackInfo.Reason = plan.Safety.RecommendReason
		fallbackInfo.Confidence = plan.Safety.Confidence
	}

	// Run selective tests
	fmt.Fprintf(os.Stderr, "Running selective tests (%d targets)...\n", len(plan.Targets.Run))
	selectiveResult, err := runTestCommand(shadowKaiCmd, plan.Targets.Run, shadowResultFormat)
	if err != nil {
		return fmt.Errorf("running selective tests: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Selective: exit=%d duration=%.1fs tests=%d failed=%d\n",
		selectiveResult.ExitCode, selectiveResult.DurationS, selectiveResult.TotalTests, len(selectiveResult.FailedTests))

	// Short-circuit if selective fails and --skip-full-on-fail
	if shadowSkipFullOnFail && selectiveResult.ExitCode != 0 {
		report := ShadowReport{
			Version:      1,
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
			KaiVersion:   Version,
			GitRange:     shadowGitRange,
			Plan:         plan,
			Verdict:      ShadowVerdictSafe,
			SelectiveRun: selectiveResult,
			Metrics:      ShadowMetrics{Accuracy: 1.0},
			Flaky:        ShadowFlakyInfo{},
			Fallback:     fallbackInfo,
		}
		if shadowOutJSON != "" {
			if err := writeShadowJSON(report, shadowOutJSON); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write JSON report: %v\n", err)
			}
		}
		if shadowOutMD != "" {
			if err := writeShadowMarkdown(report, shadowOutMD); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write markdown summary: %v\n", err)
			}
		}
		printShadowSummary(report)
		te.SetPhase("verdict_"+string(report.Verdict), 0)
		return nil
	}

	// Run full tests
	fmt.Fprintf(os.Stderr, "Running full test suite...\n")
	fullResult, err := runTestCommand(shadowFullCmd, nil, shadowResultFormat)
	if err != nil {
		return fmt.Errorf("running full tests: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Full: exit=%d duration=%.1fs tests=%d failed=%d\n",
		fullResult.ExitCode, fullResult.DurationS, fullResult.TotalTests, len(fullResult.FailedTests))

	// Detect flaky tests if there are failures and retries requested
	var flakyDetails, realDetails []FlakyTestDetail
	if len(fullResult.FailedTests) > 0 && shadowRetries > 0 {
		fmt.Fprintf(os.Stderr, "Detecting flaky tests (%d retries)...\n", shadowRetries)
		flakyDetails, realDetails = detectFlakyTests(shadowFullCmd, fullResult.FailedTests, shadowRetries, shadowResultFormat)
		if len(flakyDetails) > 0 {
			fmt.Fprintf(os.Stderr, "  Found %d flaky tests, %d real failures\n", len(flakyDetails), len(realDetails))
		}
	}

	// Compute metrics
	metrics := computeShadowMetrics(selectiveResult, fullResult, plan, flakyDetails)

	// Determine verdict
	verdict := ShadowVerdictSafe
	if metrics.FalseNegatives > 0 {
		verdict = ShadowVerdictMissed
	} else if len(flakyDetails) > 0 {
		verdict = ShadowVerdictFlakySuspect
	} else if fallbackInfo.Triggered {
		verdict = ShadowVerdictFallback
	}

	flakyInfo := ShadowFlakyInfo{
		Detected:   len(flakyDetails) > 0,
		FlakyTests: flakyDetails,
		RealTests:  realDetails,
		Retries:    shadowRetries,
	}

	// Persist flaky history
	if len(flakyDetails) > 0 {
		_ = appendFlakyRecord(FlakyHistoryRecord{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			GitRange:  shadowGitRange,
			Tests:     flakyDetails,
		})
	}

	report := ShadowReport{
		Version:      1,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		KaiVersion:   Version,
		GitRange:     shadowGitRange,
		Plan:         plan,
		Verdict:      verdict,
		SelectiveRun: selectiveResult,
		FullRun:      fullResult,
		Metrics:      metrics,
		Flaky:        flakyInfo,
		Fallback:     fallbackInfo,
	}

	// Write reports
	if shadowOutJSON != "" {
		if err := writeShadowJSON(report, shadowOutJSON); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not write JSON report: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "JSON report written to %s\n", shadowOutJSON)
		}
	}
	if shadowOutMD != "" {
		if err := writeShadowMarkdown(report, shadowOutMD); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not write markdown summary: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Markdown summary written to %s\n", shadowOutMD)
		}
	}

	printShadowSummary(report)
	te.SetPhase("verdict_"+string(verdict), 0)

	// Record misses for learning data
	if verdict == ShadowVerdictMissed {
		selectedSet := make(map[string]bool)
		for _, t := range plan.Targets.Run {
			selectedSet[t] = true
		}
		var missedTests []string
		var failedNames []string
		for _, f := range fullResult.FailedTests {
			failedNames = append(failedNames, f.Name)
			if !selectedSet[f.Name] {
				missedTests = append(missedTests, f.Name)
			}
		}
		_ = appendMissRecord(MissRecord{
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			PlanProvenance: plan.Provenance,
			FailedTests:    failedNames,
			SelectedTests:  plan.Targets.Run,
			MissedTests:    missedTests,
		})
		return fmt.Errorf("shadow run verdict: missed (%d false negatives)", metrics.FalseNegatives)
	}

	return nil
}

// --- Remote CI Commands (protocol spec section 3) ---

var ciRunsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List CI runs from the remote server",
	RunE:  runCIRuns,
}

var ciRunCmd = &cobra.Command{
	Use:   "run <run-id-or-number>",
	Short: "Show CI run details with job status",
	Args:  cobra.ExactArgs(1),
	RunE:  runCIRun,
}

var ciStatusCmd = &cobra.Command{
	Use:   "status [snapshot-ref]",
	Short: "Show trust level for a snapshot (unverified, agent-claimed, CI-verified)",
	Long: `Shows the trust level for a snapshot based on CI evidence and agent assertions.

Trust levels:
  CI-verified     A CIRun with conclusion=success exists for this snapshot
  Agent-claimed   An agent asserted tests-pass (via kai_checkpoint_now --assert)
  Unverified      No CI ran and no agent assertion exists

Examples:
  kai ci status                # Check snap.latest
  kai ci status @snap:prev     # Check previous snapshot`,
	RunE: runCIStatus,
}

var ciLogsCmd = &cobra.Command{
	Use:   "logs <run-id-or-number>",
	Short: "Show logs for a CI run",
	Args:  cobra.ExactArgs(1),
	RunE:  runCILogs,
}

var ciTraceCmd = &cobra.Command{
	Use:   "trace <run-id-or-number>",
	Short: "Stream live CI output (like gh run watch)",
	Long: `Streams live log output for a CI run, updating in real-time as
jobs start, run, and complete. Exits when the run finishes.

Examples:
  kai ci trace 110
  kai ci trace 110 --job build-kailab`,
	Args: cobra.ExactArgs(1),
	RunE: runCITrace,
}

var ciCancelCmd = &cobra.Command{
	Use:   "cancel <run-id-or-number>",
	Short: "Cancel a CI run",
	Args:  cobra.ExactArgs(1),
	RunE:  runCICancel,
}

var ciRerunCmd = &cobra.Command{
	Use:   "rerun <run-id>",
	Short: "Re-run a CI workflow",
	Args:  cobra.ExactArgs(1),
	RunE:  runCIRerun,
}

var ciSecretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "List CI secrets",
	RunE:  runCISecrets,
}

var ciSecretSetCmd = &cobra.Command{
	Use:   "secret-set <name> <value>",
	Short: "Set a CI secret",
	Args:  cobra.ExactArgs(2),
	RunE:  runCISecretSet,
}

func getRemoteOrgRepo() (string, string, string, error) {
	r, err := remote.GetRemote("origin")
	if err != nil {
		return "", "", "", fmt.Errorf("no remote configured (use 'kai remote set origin <url>')")
	}
	return r.URL, r.Tenant, r.Repo, nil
}

func resolveRunID(client *remote.ControlClient, org, repo, input string) string {
	if _, err := strconv.Atoi(input); err == nil {
		runs, _, err := client.ListCIRuns(org, repo, 100)
		if err == nil {
			num, _ := strconv.Atoi(input)
			for _, r := range runs {
				if r.RunNumber == num {
					return r.ID
				}
			}
		}
	}
	return input
}

func runCIStatus(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Resolve snapshot
	snapRef := "snap.latest"
	if len(args) > 0 {
		snapRef = args[0]
	}
	snapID, err := resolveSnapshotID(db, snapRef)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", snapRef, err)
	}
	snapHex := util.BytesToHex(snapID)

	// Query remote for CI runs matching this snapshot
	baseURL, org, repo, remoteErr := getRemoteOrgRepo()
	ciVerified := false
	var matchingRun *remote.CIRun
	if remoteErr == nil {
		client := remote.NewControlClient(baseURL)
		runs, _, err := client.ListCIRuns(org, repo, 20)
		if err == nil {
			for i, r := range runs {
				// Match by SnapshotID (new field) or TriggerSHA (legacy)
				if r.SnapshotID == snapHex || r.TriggerSHA == snapHex || r.TriggerSHA == snapHex[:12] {
					matchingRun = &runs[i]
					if r.Status == "completed" && r.Conclusion == "success" {
						ciVerified = true
					}
					break
				}
			}
		}
	}

	// Check for agent assertions (checkpoint_now with assert field)
	// Agent assertions are stored in sync_events on the server. For now,
	// check local session refs as a proxy — if a session.*.base ref exists
	// whose base is ≤ this snapshot, an agent session was active.
	// Full assertion query requires sync_events API (future).

	fmt.Printf("Snapshot: %s\n", snapHex[:12])
	fmt.Printf("Ref:      %s\n", snapRef)
	fmt.Println()

	if ciVerified && matchingRun != nil {
		fmt.Printf("Trust:    ✓ CI-verified\n")
		fmt.Printf("Run:      #%d %s (%s)\n", matchingRun.RunNumber, matchingRun.WorkflowName, matchingRun.Conclusion)
	} else if matchingRun != nil {
		status := matchingRun.Status
		if matchingRun.Conclusion != "" {
			status = matchingRun.Conclusion
		}
		fmt.Printf("Trust:    ○ CI ran but not passing\n")
		fmt.Printf("Run:      #%d %s (%s)\n", matchingRun.RunNumber, matchingRun.WorkflowName, status)
	} else {
		fmt.Printf("Trust:    — Unverified\n")
		fmt.Println("          No CI run found for this snapshot.")
		if remoteErr != nil {
			fmt.Println("          (no remote configured)")
		}
	}

	return nil
}

func runCIRuns(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runs, total, err := client.ListCIRuns(org, repo, ciRunsLimit)
	if err != nil {
		return fmt.Errorf("listing runs: %w", err)
	}
	if len(runs) == 0 {
		fmt.Println("No CI runs.")
		return nil
	}
	fmt.Printf("Showing %d of %d runs for %s/%s\n\n", len(runs), total, org, repo)
	for _, r := range runs {
		icon := "○"
		switch {
		case r.Status == "completed" && r.Conclusion == "success":
			icon = "✓"
		case r.Status == "completed" && r.Conclusion == "failure":
			icon = "✕"
		case r.Status == "in_progress":
			icon = "●"
		case r.Status == "queued":
			icon = "◦"
		}
		ref := r.TriggerRef
		if strings.HasPrefix(ref, "refs/heads/") {
			ref = strings.TrimPrefix(ref, "refs/heads/")
		}
		sha := ""
		if len(r.TriggerSHA) >= 7 {
			sha = r.TriggerSHA[:7]
		}
		status := r.Status
		if r.Conclusion != "" {
			status = r.Conclusion
		}
		// Prefer StartedAt ("when it began"); fall back to CreatedAt
		// for queued runs that haven't started yet.
		when := r.StartedAt
		if when == "" {
			when = r.CreatedAt
		}
		fmt.Printf("%s #%-4d %-12s %-10s %s %-9s %s\n",
			icon, r.RunNumber, r.WorkflowName, status, ref, sha, ciRunWhen(when))
	}
	return nil
}

// ciRunWhen formats an ISO-8601 timestamp from the server as a relative
// age ("2m ago", "1h ago"), or absolute date if older than a day. Empty
// or unparseable input renders as "—" so the column stays aligned.
func ciRunWhen(iso string) string {
	if iso == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// Server clock skew / future timestamp; show absolute.
		return t.Local().Format("2006-01-02 15:04")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

func runCIRun(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runID := resolveRunID(client, org, repo, args[0])
	run, err := client.GetCIRun(org, repo, runID)
	if err != nil {
		return fmt.Errorf("getting run: %w", err)
	}
	status := run.Status
	if run.Conclusion != "" {
		status = run.Conclusion
	}
	fmt.Printf("%s #%d  %s\n", run.WorkflowName, run.RunNumber, status)
	fmt.Printf("  trigger: %s on %s\n", run.TriggerEvent, run.TriggerRef)
	if len(run.TriggerSHA) >= 12 {
		fmt.Printf("  sha:     %s\n", run.TriggerSHA[:12])
	}
	fmt.Println()
	jobs, err := client.ListCIJobs(org, repo, runID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	for _, j := range jobs {
		icon := "○"
		switch {
		case j.Conclusion == "success":
			icon = "✓"
		case j.Conclusion == "failure":
			icon = "✕"
		case j.Status == "in_progress":
			icon = "●"
		}
		jStatus := j.Status
		if j.Conclusion != "" {
			jStatus = j.Conclusion
		}
		fmt.Printf("  %s %-25s %s\n", icon, j.Name, jStatus)
		for _, s := range j.Steps {
			sIcon := "○"
			switch {
			case s.Conclusion == "success":
				sIcon = "✓"
			case s.Conclusion == "failure":
				sIcon = "✕"
			}
			exitStr := ""
			if s.ExitCode != nil && *s.ExitCode != 0 {
				exitStr = fmt.Sprintf(" (exit %d)", *s.ExitCode)
			}
			fmt.Printf("    %s %s%s\n", sIcon, s.Name, exitStr)
		}
	}
	return nil
}

func runCILogs(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runID := resolveRunID(client, org, repo, args[0])
	jobs, err := client.ListCIJobs(org, repo, runID)
	if err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}
	var targetJob *remote.CIJob
	if ciLogsJob != "" {
		for i, j := range jobs {
			if j.Name == ciLogsJob {
				targetJob = &jobs[i]
				break
			}
		}
		if targetJob == nil {
			return fmt.Errorf("job %q not found", ciLogsJob)
		}
	} else {
		for i, j := range jobs {
			if j.Conclusion == "failure" {
				targetJob = &jobs[i]
				break
			}
		}
		if targetJob == nil && len(jobs) > 0 {
			targetJob = &jobs[0]
		}
	}
	if targetJob == nil {
		return fmt.Errorf("no jobs found")
	}
	fmt.Printf("=== %s ===\n\n", targetJob.Name)
	logs, err := client.GetCILogs(org, repo, runID, targetJob.ID)
	if err != nil {
		return fmt.Errorf("getting logs: %w", err)
	}
	for _, entry := range logs {
		fmt.Print(entry.Content)
	}
	return nil
}

func runCITrace(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runID := resolveRunID(client, org, repo, args[0])

	// Track per-job log cursors (last chunk_seq seen)
	cursors := make(map[string]int)     // jobID -> last chunk_seq
	jobPrinted := make(map[string]bool) // jobID -> printed header
	lastStatus := ""

	for {
		// Fetch run status
		run, err := client.GetCIRun(org, repo, runID)
		if err != nil {
			return fmt.Errorf("getting run: %w", err)
		}

		// Print status changes
		status := run.Status
		if run.Conclusion != "" {
			status = run.Conclusion
		}
		if status != lastStatus {
			if lastStatus != "" {
				fmt.Fprintf(os.Stderr, "\r\033[K")
			}
			switch status {
			case "queued":
				fmt.Fprintf(os.Stderr, "◦ Run #%d queued...\n", run.RunNumber)
			case "in_progress":
				fmt.Fprintf(os.Stderr, "● Run #%d in progress\n", run.RunNumber)
			case "success":
				fmt.Fprintf(os.Stderr, "✓ Run #%d succeeded\n", run.RunNumber)
			case "failure":
				fmt.Fprintf(os.Stderr, "✕ Run #%d failed\n", run.RunNumber)
			case "cancelled":
				fmt.Fprintf(os.Stderr, "○ Run #%d cancelled\n", run.RunNumber)
			default:
				fmt.Fprintf(os.Stderr, "  Run #%d %s\n", run.RunNumber, status)
			}
			lastStatus = status
		}

		// Fetch jobs and stream logs
		jobs, err := client.ListCIJobs(org, repo, runID)
		if err == nil {
			for _, job := range jobs {
				// Filter by --job if specified
				if ciTraceJob != "" && job.Name != ciTraceJob {
					continue
				}

				// Only stream jobs that are running or completed
				if job.Status != "in_progress" && job.Status != "completed" {
					continue
				}

				// Print job header once
				if !jobPrinted[job.ID] {
					icon := "●"
					if job.Status == "completed" {
						if job.Conclusion == "success" {
							icon = "✓"
						} else {
							icon = "✕"
						}
					}
					fmt.Printf("\n%s %s\n", icon, job.Name)
					jobPrinted[job.ID] = true
				}

				// Fetch new log chunks
				afterSeq := cursors[job.ID]
				logs, err := client.GetCILogsSince(org, repo, runID, job.ID, afterSeq)
				if err != nil {
					continue
				}
				for _, entry := range logs {
					fmt.Print(entry.Content)
					if entry.ChunkSeq > cursors[job.ID] {
						cursors[job.ID] = entry.ChunkSeq
					}
				}

				// Print completion
				if job.Status == "completed" && job.Conclusion != "" {
					if !strings.HasSuffix(fmt.Sprintf("%v", cursors[job.ID]), "_done") {
						if job.Conclusion == "success" {
							fmt.Printf("  ✓ %s completed\n", job.Name)
						} else {
							fmt.Printf("  ✕ %s %s\n", job.Name, job.Conclusion)
						}
						cursors[job.ID+"_done"] = 1
					}
				}
			}
		}

		// Exit when run is done
		if run.Status == "completed" {
			if run.Conclusion == "success" {
				return nil
			}
			return fmt.Errorf("run %s", run.Conclusion)
		}

		time.Sleep(2 * time.Second)
	}
}

func runCICancel(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runID := resolveRunID(client, org, repo, args[0])
	if err := client.CancelCIRun(org, repo, runID); err != nil {
		return fmt.Errorf("cancelling run: %w", err)
	}
	fmt.Printf("Cancelled run %s\n", args[0])
	return nil
}

func runCIRerun(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	runID := resolveRunID(client, org, repo, args[0])
	newID, err := client.RerunCI(org, repo, runID)
	if err != nil {
		return err
	}
	fmt.Printf("Re-run created: %s\n", newID)
	return nil
}

func runCISecrets(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	names, err := client.ListCISecrets(org, repo)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No secrets configured.")
		return nil
	}
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	return nil
}

func runCISecretSet(cmd *cobra.Command, args []string) error {
	baseURL, org, repo, err := getRemoteOrgRepo()
	if err != nil {
		return err
	}
	client := remote.NewControlClient(baseURL)
	if err := client.SetCISecret(org, repo, args[0], args[1]); err != nil {
		return err
	}
	fmt.Printf("Secret %s set.\n", args[0])
	return nil
}

// --- kai prime: pre-injection context retrieval ---

// primeStopWords are common words that don't help narrow down code search.
var primeStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "in": true, "for": true,
	"to": true, "is": true, "it": true, "of": true, "and": true,
	"or": true, "on": true, "at": true, "by": true, "my": true,
	"this": true, "that": true, "with": true, "from": true, "be": true,
	"do": true, "how": true, "what": true, "why": true, "can": true,
	"i": true, "me": true, "we": true, "not": true, "no": true,
	"all": true, "but": true, "so": true, "if": true, "up": true,
}

// primeTokenizeQuery splits a query into lowercase keywords, removing stop words.
func primeTokenizeQuery(query string) []string {
	// Split on non-alphanumeric characters
	parts := regexp.MustCompile(`[^a-zA-Z0-9_]+`).Split(strings.ToLower(query), -1)
	var keywords []string
	for _, p := range parts {
		if p == "" || primeStopWords[p] {
			continue
		}
		keywords = append(keywords, p)
	}
	return keywords
}

// primeSplitCamelCase splits "handleLoginRequest" into ["handle", "login", "request"].
func primeSplitCamelCase(s string) []string {
	var parts []string
	var current strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			if current.Len() > 0 {
				parts = append(parts, strings.ToLower(current.String()))
				current.Reset()
			}
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, strings.ToLower(current.String()))
	}
	return parts
}

// primeScoreText scores a text against keywords. Returns number of keyword hits.
func primeScoreText(text string, keywords []string) float64 {
	lower := strings.ToLower(text)
	score := 0.0
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			score += 1.0
			// Bonus for exact segment match (path segment or camelCase word)
			segments := append(strings.Split(lower, "/"), strings.Split(lower, ".")...)
			segments = append(segments, primeSplitCamelCase(text)...)
			for _, seg := range segments {
				if seg == kw {
					score += 0.5
					break
				}
			}
		}
	}
	return score
}

// primeFileScore holds scoring data for a file during prime retrieval.
type primeFileScore struct {
	Path       string
	FileID     []byte
	Score      float64
	Symbols    []primeSymbolInfo
	Imports    []string
	ImportedBy []string
	Tests      []string
}

// primeSymbolInfo holds symbol data for prime output.
type primeSymbolInfo struct {
	Name      string
	Kind      string
	Signature string
	Line      int
}

// primeGetRecentFiles returns files recently modified in git, ordered by recency.
func primeGetRecentFiles() []string {
	cmd := exec.Command("git", "log", "--format=", "--name-only", "-n", "30", "--diff-filter=ACMR")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		files = append(files, line)
	}
	return files
}

func runPrime(cmd *cobra.Command, args []string) error {
	query := args[0]
	keywords := primeTokenizeQuery(query)
	if len(keywords) == 0 {
		return fmt.Errorf("no usable keywords in query")
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Get latest snapshot
	resolver := ref.NewResolver(db)
	kind := ref.KindSnapshot
	result, err := resolver.Resolve("@snap:last", &kind)
	if err != nil {
		return fmt.Errorf("no snapshots found — run 'kai capture' first: %w", err)
	}
	snapID := result.ID

	// Get all files in the snapshot
	creator := snapshot.NewCreator(db, nil)
	files, err := creator.GetSnapshotFiles(snapID)
	if err != nil {
		return fmt.Errorf("getting snapshot files: %w", err)
	}

	// Score every file by keyword match on path + symbol names
	scored := make(map[string]*primeFileScore)
	for _, file := range files {
		path, _ := file.Payload["path"].(string)
		if path == "" {
			continue
		}

		fs := &primeFileScore{
			Path:   path,
			FileID: file.ID,
			Score:  primeScoreText(path, keywords),
		}

		// Score symbols in this file
		symbols, err := creator.GetSymbolsInFile(file.ID, snapID)
		if err != nil {
			continue
		}
		for _, sym := range symbols {
			name, _ := sym.Payload["fqName"].(string)
			if name == "" {
				continue
			}
			symScore := primeScoreText(name, keywords)
			if symScore > 0 {
				fs.Score += symScore
			}

			info := primeSymbolInfo{Name: name}
			if v, _ := sym.Payload["kind"].(string); v != "" {
				info.Kind = v
			}
			if v, _ := sym.Payload["signature"].(string); v != "" {
				info.Signature = v
			}
			if r, ok := sym.Payload["range"].(map[string]interface{}); ok {
				if line, ok := r["startLine"].(float64); ok {
					info.Line = int(line)
				}
			}
			fs.Symbols = append(fs.Symbols, info)
		}

		scored[path] = fs
	}

	// Edge walk: boost neighbors of matched files
	for _, fs := range scored {
		if fs.Score <= 0 {
			continue
		}

		// Get imports (what this file depends on)
		importEdges, err := db.GetEdges(fs.FileID, graph.EdgeImports)
		if err == nil {
			for _, edge := range importEdges {
				node, err := db.GetNode(edge.Dst)
				if err != nil || node == nil {
					continue
				}
				depPath, _ := node.Payload["path"].(string)
				if depPath == "" {
					continue
				}
				fs.Imports = append(fs.Imports, depPath)
				// Boost the neighbor
				if neighbor, ok := scored[depPath]; ok {
					neighbor.Score += fs.Score * 0.3
				}
			}
		}

		// Get dependents (what imports this file)
		depEdges, err := db.GetEdgesToByPath(fs.Path, graph.EdgeImports)
		if err == nil {
			seen := make(map[string]bool)
			for _, edge := range depEdges {
				node, err := db.GetNode(edge.Src)
				if err != nil || node == nil {
					continue
				}
				depPath, _ := node.Payload["path"].(string)
				if depPath == "" || seen[depPath] {
					continue
				}
				seen[depPath] = true
				fs.ImportedBy = append(fs.ImportedBy, depPath)
				// Boost the neighbor
				if neighbor, ok := scored[depPath]; ok {
					neighbor.Score += fs.Score * 0.2
				}
			}
		}

		// Get tests
		testEdges, err := db.GetEdgesToByPath(fs.Path, graph.EdgeTests)
		if err == nil {
			seen := make(map[string]bool)
			for _, edge := range testEdges {
				node, err := db.GetNode(edge.Src)
				if err != nil || node == nil {
					continue
				}
				testPath, _ := node.Payload["path"].(string)
				if testPath == "" || seen[testPath] {
					continue
				}
				seen[testPath] = true
				fs.Tests = append(fs.Tests, testPath)
			}
		}
	}

	// Git recency boost
	recentFiles := primeGetRecentFiles()
	for i, path := range recentFiles {
		if fs, ok := scored[path]; ok {
			if i < 10 {
				fs.Score *= 1.5
			} else {
				fs.Score *= 1.2
			}
			// If a recently-edited file has zero score from keywords,
			// give it a small baseline so it can appear if nothing else matches
			if fs.Score == 0 {
				fs.Score = 0.3
			}
		}
	}

	// Collect files with positive scores and sort by score descending
	var ranked []*primeFileScore
	for _, fs := range scored {
		if fs.Score > 0 {
			ranked = append(ranked, fs)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})

	// Pack into ~1500 tokens (~6000 chars) of structured markdown.
	// Smaller context = less cache-creation overhead per turn.
	const charBudget = 6000
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("# Kai Context: %q\n\n", query))

	charsSoFar := buf.Len()
	for _, fs := range ranked {
		if charsSoFar >= charBudget {
			break
		}

		var section strings.Builder
		section.WriteString(fmt.Sprintf("## %s\n", fs.Path))

		// Symbols — names and kinds only, no signatures
		if len(fs.Symbols) > 0 {
			shown := 0
			const maxSymbols = 5
			for _, sym := range fs.Symbols {
				if shown >= maxSymbols {
					break
				}
				if shown > 0 {
					section.WriteString(", ")
				} else {
					section.WriteString("Symbols: ")
				}
				section.WriteString(sym.Name)
				if sym.Kind != "" {
					section.WriteString(" (" + sym.Kind + ")")
				}
				shown++
			}
			if len(fs.Symbols) > maxSymbols {
				section.WriteString(fmt.Sprintf(" +%d more", len(fs.Symbols)-maxSymbols))
			}
			section.WriteString("\n")
		}

		if len(fs.Imports) > 0 {
			imports := fs.Imports
			if len(imports) > 3 {
				imports = imports[:3]
			}
			section.WriteString("Imports: " + strings.Join(imports, ", "))
			if len(fs.Imports) > 3 {
				section.WriteString(fmt.Sprintf(" +%d more", len(fs.Imports)-3))
			}
			section.WriteString("\n")
		}

		if len(fs.ImportedBy) > 0 {
			deps := fs.ImportedBy
			if len(deps) > 3 {
				deps = deps[:3]
			}
			section.WriteString("Used by: " + strings.Join(deps, ", "))
			if len(fs.ImportedBy) > 3 {
				section.WriteString(fmt.Sprintf(" +%d more", len(fs.ImportedBy)-3))
			}
			section.WriteString("\n")
		}

		if len(fs.Tests) > 0 {
			tests := fs.Tests
			if len(tests) > 2 {
				tests = tests[:2]
			}
			section.WriteString("Tests: " + strings.Join(tests, ", "))
			if len(fs.Tests) > 2 {
				section.WriteString(fmt.Sprintf(" +%d more", len(fs.Tests)-2))
			}
			section.WriteString("\n")
		}

		section.WriteString("\n")

		sectionStr := section.String()
		if charsSoFar+len(sectionStr) > charBudget && charsSoFar > 100 {
			break
		}
		buf.WriteString(sectionStr)
		charsSoFar += len(sectionStr)
	}

	fmt.Print(buf.String())
	return nil
}
