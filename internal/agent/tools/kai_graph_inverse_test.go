package tools

import (
	"context"
	"strings"
	"testing"
)

// toolByName returns the registered tool with the given name (graph
// tools only — no binary/workspace wired).
func toolByName(t *testing.T, g KaiGrapher, name string) BaseTool {
	t.Helper()
	for _, tl := range (&KaiTools{DB: g}).All() {
		if tl.Info().Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func TestKaiCallees_FindsOutgoingCalls(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("api/server.go")
	g.addFile("router.go")
	g.addCall("api/server.go", "router.go", "Register", 42)
	g.addCall("api/server.go", "router.go", "Listen", 50)
	g.addCall("other.go", "router.go", "Register", 9) // different caller — excluded

	resp, _ := toolByName(t, g, "kai_callees").Run(context.Background(), ToolCall{
		Name: "kai_callees", Input: `{"file":"api/server.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	for _, want := range []string{"Register", "Listen", ":42", ":50"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("missing %q in output:\n%s", want, resp.Content)
		}
	}
	if strings.Contains(resp.Content, ":9") {
		t.Errorf("output should not include another file's call:\n%s", resp.Content)
	}
}

func TestKaiDependencies_FindsOutboundDeps(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("a.go")
	g.addFile("b.go")
	g.addFile("c.go")
	g.addCall("a.go", "b.go", "Foo", 1)
	g.addCall("c.go", "b.go", "Bar", 2) // c depends on b, not a

	resp, _ := toolByName(t, g, "kai_dependencies").Run(context.Background(), ToolCall{
		Name: "kai_dependencies", Input: `{"file":"a.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "b.go") {
		t.Errorf("expected b.go as a dependency:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "c.go") {
		t.Errorf("c.go is not a dependency of a.go:\n%s", resp.Content)
	}
}

func TestKaiTests_FindsTestFilesViaImport(t *testing.T) {
	g := newFakeKaiGraph()
	g.addFile("gate.go")
	g.addFile("gate_test.go")
	g.addFile("helper.go")
	g.addImport("gate_test.go", "gate.go") // test imports target → counts
	g.addImport("helper.go", "gate.go")    // non-test importer → excluded

	resp, _ := toolByName(t, g, "kai_tests").Run(context.Background(), ToolCall{
		Name: "kai_tests", Input: `{"file":"gate.go"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "gate_test.go") {
		t.Errorf("expected gate_test.go:\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, "helper.go") {
		t.Errorf("a non-test importer should be excluded:\n%s", resp.Content)
	}
}

func TestNewTools_Registered(t *testing.T) {
	g := newFakeKaiGraph()
	names := map[string]bool{}
	for _, tl := range (&KaiTools{DB: g, KaiBinary: "/bin/true", Workspace: "/tmp"}).All() {
		names[tl.Info().Name] = true
	}
	for _, want := range []string{"kai_callees", "kai_dependencies", "kai_tests", "kai_blame", "kai_log"} {
		if !names[want] {
			t.Errorf("new tool %q not registered", want)
		}
	}
}

func TestTruncateSnapshotLog_KeepsLimit(t *testing.T) {
	in := "snap aaa\n    msg a\nsnap bbb\n    msg b\nsnap ccc\n    msg c"
	out := truncateSnapshotLog(in, 2)
	if !strings.Contains(out, "snap aaa") || !strings.Contains(out, "snap bbb") {
		t.Errorf("expected first two snapshots kept:\n%s", out)
	}
	if strings.Contains(out, "snap ccc") {
		t.Errorf("third snapshot should be truncated:\n%s", out)
	}
	if !strings.Contains(out, "limit reached") {
		t.Errorf("expected a truncation note:\n%s", out)
	}
}
