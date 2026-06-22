package orchestrator

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// testFrameworkConvention describes, per language/stack, how tests are written
// in a project — so an agent asked to add a test does not have to
// explore to discover the framework, and does not invent a new one.
type testFrameworkConvention struct {
	stack string
	rule  string
	// isTestFile reports whether a filename is a test file for this
	// stack, used to surface concrete examples for the agent to mirror.
	isTestFile func(name string) bool
}

var testFrameworkConventions = []testFrameworkConvention{
	{
		stack: "Go",
		rule: "standard `testing` package only — func TestXxx(t *testing.T) with t.Errorf / t.Fatalf. " +
			"Do NOT add testify, ginkgo, gomega, or any new test module: introducing a test dependency is a refactor nobody asked for. " +
			"Test files are <name>_test.go in the same package.",
		isTestFile: func(n string) bool { return strings.HasSuffix(n, "_test.go") },
	},
	{
		stack: "Rust",
		rule: "in-file `#[cfg(test)] mod tests` with `#[test]` functions, or integration tests under tests/. " +
			"Do NOT add a new test crate.",
		isTestFile: func(n string) bool { return strings.HasSuffix(n, "_test.rs") || n == "tests.rs" },
	},
	{
		stack: "Python",
		rule:  "pytest — test_*.py with plain `assert`. Do NOT add a new test framework.",
		isTestFile: func(n string) bool {
			return strings.HasPrefix(n, "test_") && strings.HasSuffix(n, ".py")
		},
	},
}

const (
	testContextMaxDepth         = 6
	testContextExamplesPerStack = 3
)

// testContextBlock scans spawnDir and returns a "TEST CONTEXT" block
// telling the agent how tests are written here — the framework
// convention plus a couple of real test files to mirror — so an agent
// asked to add a test neither burns its read budget rediscovering the
// convention nor invents a new one.
//
// The 2026-05-16 dogfood pinned both failure modes from one missing
// signal: a worker writing a test for a one-line feature explored so
// heavily it hit the read gate, AND pulled in github.com/onsi/gomega
// (a BDD framework the codebase never used). Handing it the
// convention up front fixes both — the test-side analogue of
// buildContextBlock.
//
// Returns "" when no known stack is detected.
func testContextBlock(spawnDir string) string {
	conv := map[string]testFrameworkConvention{}
	for _, c := range testFrameworkConventions {
		conv[c.stack] = c
	}
	markerStack := map[string]string{}
	for _, m := range buildMarkers {
		markerStack[m.file] = m.stack
	}

	present := map[string]bool{}
	examples := map[string][]string{}

	_ = filepath.WalkDir(spawnDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // one bad entry shouldn't abort the scan
		}
		rel, relErr := filepath.Rel(spawnDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && buildContextDirSkip[d.Name()] {
				return filepath.SkipDir
			}
			if rel != "." && strings.Count(rel, "/")+1 > testContextMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if st, ok := markerStack[d.Name()]; ok {
			if _, known := conv[st]; known {
				present[st] = true
			}
		}
		for st, c := range conv {
			if c.isTestFile(d.Name()) && len(examples[st]) < testContextExamplesPerStack {
				examples[st] = append(examples[st], rel)
			}
		}
		return nil
	})

	if len(present) == 0 {
		return ""
	}
	stacks := make([]string, 0, len(present))
	for st := range present {
		stacks = append(stacks, st)
	}
	sort.Strings(stacks)

	var b strings.Builder
	b.WriteString("TEST CONTEXT — how tests are written in this workspace. ")
	b.WriteString("Match the existing convention exactly; adding a new test framework or dependency is a refactor, not part of the task:\n")
	for _, st := range stacks {
		fmt.Fprintf(&b, "  - %s: %s\n", st, conv[st].rule)
		if ex := examples[st]; len(ex) > 0 {
			sort.Strings(ex)
			fmt.Fprintf(&b, "    Mirror an existing test file: %s\n", strings.Join(ex, ", "))
		}
	}
	return b.String()
}
