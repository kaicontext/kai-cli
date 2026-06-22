package planner

import (
	"strings"
	"testing"

	"kai/internal/graph"
)

func fileNode(lang string) *graph.Node {
	return &graph.Node{
		Kind:    graph.KindFile,
		Payload: map[string]interface{}{"lang": lang},
	}
}

// TestSummarizeFileNodes pins the planner's project-facts annotation.
// The 2026-05-15 dogfood: planner saw an empty project named
// "kai-tui" and hallucinated a Rust stack. These cases lock the
// signal that prevents it.
func TestSummarizeFileNodes(t *testing.T) {
	cases := []struct {
		name     string
		nodes    []*graph.Node
		wantSubs []string // all must appear in the result
	}{
		{
			name:     "no files indexed",
			nodes:    nil,
			wantSubs: []string{"EMPTY", "0 files", "kai_tree"},
		},
		{
			// The actual kai-tui case: .gitignore (blob) + a yaml file.
			name:     "only non-code files",
			nodes:    []*graph.Node{fileNode("blob"), fileNode("yaml")},
			wantSubs: []string{"2 file", "NONE are source code", "EMPTY of code", "kai_tree"},
		},
		{
			name: "healthy go project",
			nodes: []*graph.Node{
				fileNode("go"), fileNode("go"), fileNode("go"),
				fileNode("markdown"),
			},
			wantSubs: []string{"4 files indexed", "go"},
		},
		{
			name: "mixed code stack reports dominant languages",
			nodes: []*graph.Node{
				fileNode("typescript"), fileNode("typescript"),
				fileNode("go"), fileNode("css"),
			},
			wantSubs: []string{"4 files indexed", "typescript"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeFileNodes(c.nodes)
			for _, sub := range c.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("summarizeFileNodes() = %q; missing %q", got, sub)
				}
			}
		})
	}
}

// TestSummarizeFileNodes_HealthyProjectGivesNoWarning confirms a real
// code project does NOT get an EMPTY/verify warning — the planner
// should treat it as plannable ground truth, not be told to re-check.
func TestSummarizeFileNodes_HealthyProjectGivesNoWarning(t *testing.T) {
	got := summarizeFileNodes([]*graph.Node{fileNode("go"), fileNode("go")})
	for _, bad := range []string{"EMPTY", "NOT INDEXED", "kai_tree", "do not"} {
		if strings.Contains(got, bad) {
			t.Errorf("healthy project annotation %q should not contain %q", got, bad)
		}
	}
}

func TestTopLangs(t *testing.T) {
	hist := map[string]int{"go": 10, "markdown": 3, "yaml": 5, "css": 1}
	got := topLangs(hist, 3)
	// Most frequent first: go(10), yaml(5), markdown(3); css(1) dropped.
	if got != "go, yaml, markdown" {
		t.Errorf("topLangs = %q, want %q", got, "go, yaml, markdown")
	}
	if c := strings.Count(got, ","); c != 2 {
		t.Errorf("topLangs should cap at 3 entries, got %d separators in %q", c, got)
	}
	// Empty lang key renders as "unknown".
	if l := topLangs(map[string]int{"": 4}, 3); l != "unknown" {
		t.Errorf("empty lang should render as 'unknown', got %q", l)
	}
}
