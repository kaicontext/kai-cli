package agentprompt

import (
	"strings"
	"testing"

	"kai/internal/planner"
)

func TestBuild_MinimalTask(t *testing.T) {
	out := Build(planner.AgentTask{
		Name:   "tests",
		Prompt: "add unit tests for the rate limiter",
	}, Context{})

	mustContain(t, out, []string{
		`agent "tests"`,
		"Task: add unit tests for the rate limiter",
		"orchestrator will integrate",
	})
	mustNotContain(t, out, []string{
		"Files you should focus on:",
		"Files you must NOT modify:",
		"Graph context",
	})
}

func TestBuild_WithFilesAndForbidden(t *testing.T) {
	out := Build(planner.AgentTask{
		Name:      "backend-api",
		Prompt:    "add rate limit middleware",
		Files:     []string{"middleware/ratelimit.go", "router.go"},
		DontTouch: []string{"pkg/auth/login.go"},
	}, Context{
		RepoRoot:  "/repo",
		Language:  "go",
		Protected: []string{"pkg/billing/**"},
	})

	mustContain(t, out, []string{
		"Working directory: /repo",
		"Primary language: go",
		"Files you should focus on:",
		"middleware/ratelimit.go",
		"router.go",
		"Files you must NOT modify:",
		"pkg/auth/login.go",
		"pkg/billing/**",
		"If changing one of these is genuinely necessary",
	})
}

func TestBuild_DeterministicOrdering(t *testing.T) {
	// Same inputs in different order must produce the same output —
	// stable for golden-file tests and for cache keys.
	a := Build(planner.AgentTask{
		Name:      "x",
		Prompt:    "p",
		Files:     []string{"b.go", "a.go", "c.go"},
		DontTouch: []string{"z.go", "m.go"},
	}, Context{Protected: []string{"forbidden/**"}})

	b := Build(planner.AgentTask{
		Name:      "x",
		Prompt:    "p",
		Files:     []string{"c.go", "a.go", "b.go"},
		DontTouch: []string{"m.go", "z.go"},
	}, Context{Protected: []string{"forbidden/**"}})

	if a != b {
		t.Fatalf("Build is non-deterministic across input ordering:\n%s\n---\n%s", a, b)
	}
}

// TestBuild_DontTouchAndProtectedMerged verifies the forbidden list
// is dedup-merged across DontTouch and Protected so the agent sees
// one clean list rather than two.
func TestBuild_DontTouchAndProtectedMerged(t *testing.T) {
	out := Build(planner.AgentTask{
		Name:      "x",
		Prompt:    "p",
		DontTouch: []string{"shared.go", "x.go"},
	}, Context{
		Protected: []string{"shared.go", "y.go"},
	})

	count := strings.Count(out, "shared.go")
	if count != 1 {
		t.Fatalf("expected shared.go to appear once (deduped), got %d times in:\n%s", count, out)
	}
	mustContain(t, out, []string{"x.go", "y.go"})
}

func TestBuild_GraphContextRendered(t *testing.T) {
	gctx := "router.go: called by api/server.go, api/health.go"
	out := Build(planner.AgentTask{Name: "x", Prompt: "p"}, Context{
		GraphContext: gctx,
	})
	mustContain(t, out, []string{"Graph context for the files in scope:", gctx})
}

// TestBuild_ExplorationPlaybookPresent pins the May-2026 fix for
// the "where is the TUI?" failure: the agent searched a single
// folder by name guess (kai-tui), got an empty tree, repeated the
// same call four times, then declared the feature didn't exist.
// The playbook tells the agent to lead with kai_grep on concept
// keywords across all roots, never trust folder names, and never
// repeat an empty tool call. This test guards against the playbook
// being silently stripped during prompt-tuning passes.
func TestBuild_ExplorationPlaybookPresent(t *testing.T) {
	out := Build(planner.AgentTask{Name: "x", Prompt: "find the TUI"}, Context{})
	mustContain(t, out, []string{
		"Folder names lie",
		"explore by CONCEPT, not",
		"kai_grep",
		"Anti-loop rule",
		"Verify-vs-build",
	})
}

// TestBuild_EditBudgetPresent pins the EDIT BUDGET block that tells
// the worker to stop researching and start editing. Round-15/16/17
// dogfood: opus-4-6 burned 30-50 turns re-reading the same regions
// before editing — same caching properties as the planner but no
// equivalent budget hint. This test guards the prompt from regressing.
func TestBuild_EditBudgetPresent(t *testing.T) {
	out := Build(planner.AgentTask{Name: "x", Prompt: "make repl.go toggleable"}, Context{})
	mustContain(t, out, []string{
		"EDIT BUDGET",
		"~10 read-tool calls",
		"start editing",
		"converge",
		"Plan completion",
		"A single edit that addresses step 1 and exits is NOT done",
		"I'm blocked because",
	})
}

func mustContain(t *testing.T, s string, parts []string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Errorf("output missing %q\nfull output:\n%s", p, s)
		}
	}
}

func mustNotContain(t *testing.T, s string, parts []string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(s, p) {
			t.Errorf("output should not contain %q\nfull output:\n%s", p, s)
		}
	}
}

// TestBuild_AcceptanceCriteria: the planner's acceptance criteria must
// render into the agent prompt so the agent knows what "done" means,
// not just which edits to type.
func TestBuild_AcceptanceCriteria(t *testing.T) {
	out := Build(planner.AgentTask{
		Name:   "config",
		Prompt: "extract the repeated fallback",
		AcceptanceCriteria: []string{
			"adding a new role model requires no edit to Load()",
			"behavior is identical to before",
		},
	}, Context{})

	mustContain(t, out, []string{
		"Done means ALL of these hold",
		"adding a new role model requires no edit to Load()",
		"behavior is identical to before",
	})
}

// TestBuild_NoAcceptanceCriteria: absent criteria render nothing.
func TestBuild_NoAcceptanceCriteria(t *testing.T) {
	out := Build(planner.AgentTask{Name: "x", Prompt: "p"}, Context{})
	mustNotContain(t, out, []string{"Done means ALL of these hold"})
}
