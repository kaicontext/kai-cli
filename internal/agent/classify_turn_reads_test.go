package agent

import (
	"encoding/json"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// viewCall constructs a view tool call with the JSON input shape
// parseViewRange expects.
func viewCall(id, path string, offset, limit int) message.ToolCall {
	return message.ToolCall{
		ID:    id,
		Name:  "view",
		Input: `{"file_path":"` + path + `","offset":` + itoa(offset) + `,"limit":` + itoa(limit) + `}`,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestClassifyTurnReads_FirstViewCounts confirms the very first
// view of a file counts against the streak. Without this we'd
// never tick the gate up at all.
func TestClassifyTurnReads_FirstViewCounts(t *testing.T) {
	edits, _, streak, added, file, end := classifyTurnReads(
		[]message.ToolCall{viewCall("a", "foo.go", 0, 2000)},
		"", 0,
	)
	if edits != 0 || streak != 1 || added != 1 {
		t.Errorf("expected edits=0 streak=1 added=1, got edits=%d streak=%d added=%d", edits, streak, added)
	}
	if file != "foo.go" || end != 2000 {
		t.Errorf("expected paging tracker (foo.go,2000), got (%q,%d)", file, end)
	}
}

// TestClassifyTurnReads_ContiguousPagingDoesNotCount is the load-
// bearing assertion for issue 3 today: paging the next chunk of a
// file the worker already started reading is continuation, not new
// investigation. The streak counter shouldn't tick.
func TestClassifyTurnReads_ContiguousPagingDoesNotCount(t *testing.T) {
	// Turn 1: view foo.go @0:2000 → counts.
	_, _, streak1, added1, file1, end1 := classifyTurnReads(
		[]message.ToolCall{viewCall("a", "foo.go", 0, 2000)},
		"", 0,
	)
	if streak1 != 1 || added1 != 1 || end1 != 2000 {
		t.Fatalf("turn 1 expected streak=1 added=1 end=2000, got streak=%d added=%d end=%d", streak1, added1, end1)
	}

	// Turn 2: view foo.go @2000:4000 (next page) → does NOT count.
	_, _, streak2, added2, file2, end2 := classifyTurnReads(
		[]message.ToolCall{viewCall("b", "foo.go", 2000, 2000)},
		file1, end1,
	)
	if streak2 != 0 {
		t.Errorf("turn 2 contiguous paging should not count, got streak=%d", streak2)
	}
	if added2 != 1 {
		t.Errorf("turn 2 should still increment per-run total reads (cost is real), got added=%d", added2)
	}
	if file2 != "foo.go" || end2 != 4000 {
		t.Errorf("turn 2 paging tracker should advance to end=4000, got (%q,%d)", file2, end2)
	}

	// Turn 3: view foo.go @4000:6000 → still continuation.
	_, _, streak3, _, _, end3 := classifyTurnReads(
		[]message.ToolCall{viewCall("c", "foo.go", 4000, 2000)},
		file2, end2,
	)
	if streak3 != 0 || end3 != 6000 {
		t.Errorf("turn 3 expected streak=0 end=6000, got streak=%d end=%d", streak3, end3)
	}
}

// TestClassifyTurnReads_DifferentFileCounts: switching files
// resets the paging tracker, so the new file's first view is a
// new investigation and counts.
func TestClassifyTurnReads_DifferentFileCounts(t *testing.T) {
	_, _, streak, _, file, end := classifyTurnReads(
		[]message.ToolCall{viewCall("a", "bar.go", 0, 2000)},
		"foo.go", 2000,
	)
	if streak != 1 {
		t.Errorf("switching files should count, got streak=%d", streak)
	}
	if file != "bar.go" || end != 2000 {
		t.Errorf("paging tracker should track the new file, got (%q,%d)", file, end)
	}
}

// TestClassifyTurnReads_NonContiguousSeekCounts: jumping
// backwards or skipping ahead in the same file is NOT paging —
// it's re-investigation. Should count.
func TestClassifyTurnReads_NonContiguousSeekCounts(t *testing.T) {
	// Tracker says foo.go up through line 2000. A view at @5000:
	// skipped a gap, so it's not contiguous.
	_, _, streak, _, _, end := classifyTurnReads(
		[]message.ToolCall{viewCall("a", "foo.go", 5000, 2000)},
		"foo.go", 2000,
	)
	if streak != 1 {
		t.Errorf("non-contiguous seek should count, got streak=%d", streak)
	}
	if end != 7000 {
		t.Errorf("tracker should advance to the new view's end, got end=%d", end)
	}
}

// TestClassifyTurnReads_NonViewReadsAlwaysCount: kai_grep,
// kai_search, etc. are independent investigations and always
// count against the streak even when interleaved with paging.
func TestClassifyTurnReads_NonViewReadsAlwaysCount(t *testing.T) {
	calls := []message.ToolCall{
		viewCall("a", "foo.go", 2000, 2000), // paging continuation
		{ID: "b", Name: "kai_grep", Input: `{"pattern":"foo"}`},
	}
	_, _, streak, added, _, _ := classifyTurnReads(calls, "foo.go", 2000)
	if streak != 1 {
		t.Errorf("expected streak=1 (paging skipped, grep counted), got %d", streak)
	}
	if added != 2 {
		t.Errorf("expected added=2 (both are reads for cost purposes), got %d", added)
	}
}

// TestClassifyTurnReads_EditClearsCounter: a substantive edit in the
// turn means the streak should reset upstream. This helper just
// reports the count; the caller does the reset. Verify the
// edits return is correct.
func TestClassifyTurnReads_EditClearsCounter(t *testing.T) {
	calls := []message.ToolCall{
		viewCall("a", "foo.go", 0, 2000),
		{ID: "b", Name: "edit", Input: `{"file_path":"foo.go","old_string":"x","new_string":"y"}`},
	}
	edits, substantive, streak, _, _, _ := classifyTurnReads(calls, "", 0)
	if edits != 1 {
		t.Errorf("expected edits=1, got %d", edits)
	}
	if substantive != 1 {
		t.Errorf("a code-changing edit should count as substantive, got %d", substantive)
	}
	if streak != 1 {
		t.Errorf("read still counts within this turn (caller resets streak when substantive edits>0), got streak=%d", streak)
	}
}

// TestClassifyTurnReads_CosmeticEditDoesNotCount pins the 2026-05-15
// dogfood fix: an agent toggled a `// marker` comment on and off to
// farm read-streak resets. A comment/whitespace-only edit must count
// as an edit (for the budget suffix) but NOT as a substantive edit
// (which is what resets the streak).
func TestClassifyTurnReads_CosmeticEditDoesNotCount(t *testing.T) {
	cases := []struct {
		name      string
		old, want string
	}{
		{"add-line-comment", "package views", "package views // copy-plan"},
		{"remove-line-comment", "package views // copy-plan", "package views"},
		{"whitespace-only", "x := 1", "x  :=  1"},
		{"block-comment", "func f() {}", "func f() {} /* note */"},
		{"hash-comment", "value: 1", "value: 1  # yaml note"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := []message.ToolCall{
				{ID: "e", Name: "edit", Input: `{"file_path":"f.go","old_string":` +
					jsonStr(c.old) + `,"new_string":` + jsonStr(c.want) + `}`},
			}
			edits, substantive, _, _, _, _ := classifyTurnReads(calls, "", 0)
			if edits != 1 {
				t.Errorf("cosmetic edit still counts as an edit, got edits=%d", edits)
			}
			if substantive != 0 {
				t.Errorf("cosmetic edit must NOT count as substantive, got substantive=%d", substantive)
			}
		})
	}
}

// jsonStr quotes s as a JSON string literal for test fixtures.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestClassifyTurnReads_MultiplePagesInOneTurn: within a single
// turn the agent may issue 3 view calls paging the same file.
// First counts as new investigation, the rest are continuations.
func TestClassifyTurnReads_MultiplePagesInOneTurn(t *testing.T) {
	calls := []message.ToolCall{
		viewCall("a", "foo.go", 0, 2000),
		viewCall("b", "foo.go", 2000, 2000),
		viewCall("c", "foo.go", 4000, 2000),
	}
	_, _, streak, added, _, end := classifyTurnReads(calls, "", 0)
	if streak != 1 {
		t.Errorf("first view counts, subsequent pages don't — expected streak=1, got %d", streak)
	}
	if added != 3 {
		t.Errorf("all three views cost tokens — expected added=3, got %d", added)
	}
	if end != 6000 {
		t.Errorf("tracker should advance through all three, got end=%d", end)
	}
}

// TestBashCallReads covers the K2.6-benchmark fix: a bash call that
// only inspects files (cat/sed/head/grep/…) must count against the
// read-streak gate, while a build/test/git/write bash turn must not.
func TestBashCallReads(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"cat", "cat internal/freelist/hashmap.go", true},
		{"sed range", "sed -n '1,80p' internal/freelist/hashmap.go", true},
		{"head pipe tail", "head -n 1235 db.go | tail -n +1164", true},
		{"grep with devnull", "grep -n 'WriteTo' tx.go 2>/dev/null", true},
		{"python read", `python3 -c "import sys; sys.stdout.write(open('x').read())"`, true},
		{"perl read", "perl -pe '' internal/freelist/hashmap.go", true},
		{"chained reads", "grep -n foo a.go && sed -n '1,5p' b.go", true},
		{"env prefix", "GREP_COLOR=1 grep foo a.go", true},
		{"go build is neutral", "go build ./...", false},
		{"go test is neutral", "go test -short ./...", false},
		{"git is neutral", "git diff", false},
		{"redirect is a write", "cat a.go > b.go", false},
		{"rm is not a read", "rm dummy_gate_reset.go", false},
		{"unknown binary fails closed", "frobnicate x", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input := `{"command":` + jsonStr(c.cmd) + `}`
			if got := bashCallReads(input); got != c.want {
				t.Errorf("bashCallReads(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

// TestClassifyTurnReads_BashReadExtendsStreak: a turn whose only tool
// call is a read-only bash command counts as one streak read — the
// agent cannot dodge the streak gate by reading through the shell.
func TestClassifyTurnReads_BashReadExtendsStreak(t *testing.T) {
	calls := []message.ToolCall{
		{ID: "a", Name: "bash", Input: `{"command":"sed -n '1,80p' db.go"}`},
	}
	_, _, streak, added, _, _ := classifyTurnReads(calls, "", 0)
	if streak != 1 {
		t.Errorf("read-only bash must extend the streak, got streak=%d", streak)
	}
	if added != 1 {
		t.Errorf("read-only bash counts toward read total, got added=%d", added)
	}
	build := []message.ToolCall{
		{ID: "b", Name: "bash", Input: `{"command":"go test ./..."}`},
	}
	_, _, streak2, _, _, _ := classifyTurnReads(build, "", 0)
	if streak2 != 0 {
		t.Errorf("build bash turn must stay streak-neutral, got streak=%d", streak2)
	}
}

// TestClassifyTurnReads_InterceptedReadsDoNotCount: a deduped (or
// otherwise intercepted) view never ran and returned only a stub, so
// it must count toward neither the streak nor the per-run read total.
func TestClassifyTurnReads_InterceptedReadsDoNotCount(t *testing.T) {
	calls := []message.ToolCall{
		viewCall("a", "foo.go", 0, 2000), // real read
		viewCall("b", "foo.go", 0, 2000), // deduped re-read
		{ID: "c", Name: "kai_grep", Input: `{"pattern":"x"}`},
	}
	decisions := []interceptDecision{
		{},                // a ran
		{Intercept: true}, // b intercepted (stub)
		{},                // c ran
	}
	_, _, streak, added, _, _ := classifyTurnReadsWithDecisions(calls, decisions, "", 0)
	if streak != 2 {
		t.Errorf("expected streak=2 (real view + grep; deduped view skipped), got %d", streak)
	}
	if added != 2 {
		t.Errorf("expected added=2 (intercepted read not counted as cost), got %d", added)
	}
}
