package agent

import (
	"strconv"
	"testing"

	"kai/internal/agent/message"
)

func bashCall(id, cmd string) message.ToolCall {
	return message.ToolCall{ID: id, Name: "bash", Input: `{"command":` + jsonQuote(cmd) + `}`}
}
func editCall(id, path string) message.ToolCall {
	return message.ToolCall{ID: id, Name: "edit", Input: `{"file_path":` + jsonQuote(path) + `,"old_string":"a","new_string":"b"}`}
}
func jsonQuote(s string) string { return strconv.Quote(s) }

func TestBashCallRunsTests(t *testing.T) {
	yes := []string{
		`npx vitest run`, `npm test`, `npm run test`, `cd kai-desktop && npx vitest run src/x.test.js`,
		`go test ./...`, `pytest -q`, `yarn test`, `cargo test`,
	}
	for _, c := range yes {
		if !bashCallRunsTests(`{"command":` + jsonQuote(c) + `}`) {
			t.Errorf("expected %q to be a test run", c)
		}
	}
	no := []string{`cat foo.go`, `grep test bar`, `cd tests && ls`, `node main.cjs`, `go build ./...`}
	for _, c := range no {
		if bashCallRunsTests(`{"command":` + jsonQuote(c) + `}`) {
			t.Errorf("expected %q NOT to be a test run", c)
		}
	}
}

func TestClassifyTurnTestFight(t *testing.T) {
	// Ran tests, edited only a test file → ranTests, no prodEdit (streak climbs).
	ran, prod := classifyTurnTestFight([]message.ToolCall{
		bashCall("a", "npx vitest run"),
		editCall("b", "src/__tests__/main.cjs.test.js"),
	})
	if !ran || prod {
		t.Errorf("test-only fight turn: got ran=%v prod=%v, want ran=true prod=false", ran, prod)
	}
	// Production edit present → prodEdit true (streak resets).
	_, prod2 := classifyTurnTestFight([]message.ToolCall{
		bashCall("a", "npx vitest run"),
		editCall("b", "src/main.cjs"),
	})
	if !prod2 {
		t.Errorf("production edit should set prodEdit=true")
	}
	// Neither tests nor edits → both false.
	ran3, prod3 := classifyTurnTestFight([]message.ToolCall{bashCall("a", "ls")})
	if ran3 || prod3 {
		t.Errorf("neutral turn: got ran=%v prod=%v, want both false", ran3, prod3)
	}
}

func TestIsTestFilePath(t *testing.T) {
	for _, p := range []string{"x_test.go", "src/a.test.js", "src/__tests__/b.js", "tests/c.py", "foo/test_d.py"} {
		if !isTestFilePath(p) {
			t.Errorf("%q should be a test path", p)
		}
	}
	for _, p := range []string{"src/main.cjs", "internal/runner.go", "lib/util.ts"} {
		if isTestFilePath(p) {
			t.Errorf("%q should NOT be a test path", p)
		}
	}
}
