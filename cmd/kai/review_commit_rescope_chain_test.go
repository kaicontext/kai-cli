package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeChainGraph answers rcChainGraph from tables and counts the lookups.
type fakeChainGraph struct {
	sym                  map[string][]rcSite // "file|symbol" → sites
	callers, tests, uses map[string][]string
	calls                int
}

func (f *fakeChainGraph) SymbolCallers(p, s string) []rcSite {
	f.calls++
	return f.sym[p+"|"+s]
}
func (f *fakeChainGraph) Callers(p string) []string { f.calls++; return f.callers[p] }
func (f *fakeChainGraph) Tests(p string) []string   { f.calls++; return f.tests[p] }
func (f *fakeChainGraph) Uses(p string) []string    { f.calls++; return f.uses[p] }

func chainPaths(c []rcChained) []string {
	var out []string
	for _, x := range c {
		out = append(out, x.Path)
	}
	return out
}

func syms(file string, s ...rcChangedSym) rcChainInputs {
	return rcChainInputs{Symbols: map[string][]rcChangedSym{file: s}}
}

// The call sites of a changed function come first, with their lines, and the
// rest of the file's callers are left out: they do not call what changed.
func TestReReviewChain_SymbolCallersReplaceFileCallers(t *testing.T) {
	g := &fakeChainGraph{
		sym:     map[string][]rcSite{"a.go|Charge": {{"billing.go", 40}, {"billing.go", 12}, {"api.go", 7}}},
		callers: map[string][]string{"a.go": {"api.go", "billing.go", "admin.go", "metrics.go", "a_test.go"}},
		tests:   map[string][]string{"a.go": {"a_test.go"}},
	}
	got := rcReReviewChain([]string{"a.go"}, syms("a.go", rcChangedSym{Name: "Charge"}), g, 10)
	want := []rcChained{
		{Path: "api.go", Kind: rcChainSymbol, Of: "a.go", What: "Charge", Lines: []int{7}},
		{Path: "billing.go", Kind: rcChainSymbol, Of: "a.go", What: "Charge", Lines: []int{12, 40}},
		{Path: "a_test.go", Kind: rcChainTest, Of: "a.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %+v\nwant  %+v", got, want)
	}
	if r := got[1].reason(); r != "calls Charge (changed in a.go) at line 12, 40" {
		t.Errorf("reason = %q", r)
	}
}

// A type change has no call sites, and a function with none recorded may
// still be reached another way: both keep the whole-file callers.
func TestReReviewChain_FileCallersWhenSymbolsDoNotCover(t *testing.T) {
	g := &fakeChainGraph{
		sym:     map[string][]rcSite{"a.go|Charge": {{"billing.go", 3}}},
		callers: map[string][]string{"a.go": {"admin.go", "billing.go"}},
	}
	in := syms("a.go", rcChangedSym{Name: "Charge"}, rcChangedSym{Name: "Invoice", Type: true})
	if got, want := chainPaths(rcReReviewChain([]string{"a.go"}, in, g, 10)), []string{"billing.go", "admin.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("type change: chain = %v, want %v", got, want)
	}
	in = syms("a.go", rcChangedSym{Name: "Unrecorded"})
	if got, want := chainPaths(rcReReviewChain([]string{"a.go"}, in, g, 10)), []string{"admin.go", "billing.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no sites: chain = %v, want %v", got, want)
	}
}

// Files sharing a changed key come right after the call sites; a removed
// symbol or key says so.
func TestReReviewChain_KeysAndRemoved(t *testing.T) {
	g := &fakeChainGraph{sym: map[string][]rcSite{"a.go|Old": {{"b.go", 5}}}}
	in := rcChainInputs{
		Symbols: map[string][]rcChangedSym{"a.go": {{Name: "Old", Removed: true}}},
		Keys: map[string][]rcChangedKey{"a.go": {{Key: "review_run_cost", Removed: true,
			Uses: map[string][]int{"dash.ts": {9}}}}},
	}
	got := rcReReviewChain([]string{"a.go"}, in, g, 10)
	if len(got) != 2 || got[0].Path != "b.go" || got[1].Path != "dash.ts" {
		t.Fatalf("chain = %+v", got)
	}
	if r := got[0].reason(); r != "calls Old (removed in a.go) at line 5" {
		t.Errorf("symbol reason = %q", r)
	}
	if r := got[1].reason(); r != `uses key "review_run_cost" (removed in a.go) at line 9` {
		t.Errorf("key reason = %q", r)
	}
}

// The graph lists a test file among the callers of what it tests (it calls
// it). It is chained once, as a test, after the real callers.
func TestReReviewChain_TestCallerIsATest(t *testing.T) {
	g := &fakeChainGraph{
		callers: map[string][]string{"a.go": {"a_test.go", "caller.go"}},
		tests:   map[string][]string{"a.go": {"a_test.go"}},
	}
	got := rcReReviewChain([]string{"a.go"}, rcChainInputs{}, g, 10)
	want := []rcChained{
		{Path: "caller.go", Kind: rcChainCaller, Of: "a.go"},
		{Path: "a_test.go", Kind: rcChainTest, Of: "a.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %+v, want %+v", got, want)
	}
}

// An unchanged caller outside the pull request is chained; a focus file and a
// vendored file are not, and a file two focus files share is listed once.
func TestReReviewChain_FiltersAndDedupes(t *testing.T) {
	g := &fakeChainGraph{callers: map[string][]string{
		"a.go": {"b.go", "vendor/x/y.go", "shared.go"},
		"b.go": {"shared.go", "other.go"},
	}}
	got := chainPaths(rcReReviewChain([]string{"a.go", "b.go"}, rcChainInputs{}, g, 10))
	if want := []string{"shared.go", "other.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// The cap holds, and focus files take turns so one widely used file cannot
// fill the list alone.
func TestReReviewChain_CapAndRoundRobin(t *testing.T) {
	g := &fakeChainGraph{callers: map[string][]string{
		"hub.go":  {"c1.go", "c2.go", "c3.go", "c4.go", "c5.go"},
		"leaf.go": {"l1.go"},
	}}
	got := chainPaths(rcReReviewChain([]string{"hub.go", "leaf.go"}, rcChainInputs{}, g, 3))
	if want := []string{"c1.go", "l1.go", "c2.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// A full review (no focus) or a zero cap chains nothing; with no graph the
// keys still chain; a full list stops querying the graph.
func TestReReviewChain_Off(t *testing.T) {
	g := &fakeChainGraph{callers: map[string][]string{"a.go": {"x.go"}}}
	if c := rcReReviewChain(nil, rcChainInputs{}, g, 10); c != nil {
		t.Fatalf("no focus: chain = %v", c)
	}
	if c := rcReReviewChain([]string{"a.go"}, rcChainInputs{}, g, 0); c != nil {
		t.Fatalf("cap 0: chain = %v", c)
	}
	keys := rcChainInputs{Keys: map[string][]rcChangedKey{"a.go": {{Key: "k_1", Uses: map[string][]int{"z.go": {1}}}}}}
	if c := chainPaths(rcReReviewChain([]string{"a.go"}, keys, nil, 10)); !reflect.DeepEqual(c, []string{"z.go"}) {
		t.Fatalf("no graph: chain = %v", c)
	}
	g.calls = 0
	rcReReviewChain([]string{"a.go"}, syms("a.go", rcChangedSym{Name: "F"}), g, 1)
	if g.calls != 2 { // SymbolCallers (none), then Callers fills the one slot
		t.Fatalf("a full list made %d graph lookups, want 2", g.calls)
	}
}

func TestSinceSymbols(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/pay.go b/pay.go",
		"--- a/pay.go",
		"+++ b/pay.go",
		"@@ -10,2 +10,2 @@ func (s *Svc) Charge(ctx context.Context, amt int) error {",
		"-	if amt < 0 {",
		"+	if amt <= 0 {",
		"@@ -30 +30 @@ type Invoice struct {",
		"-	Total int",
		"+	Total int64",
		"@@ -40,3 +39,0 @@ func Refund(id string) error {",
		"-func legacyRefund(id string) error {",
		"-	return nil",
		"-}",
		"@@ -50,0 +48,3 @@",
		"+func NewHelper() *Helper {",
		"diff --git a/gone.py b/gone.py",
		"--- a/gone.py",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-def old_api(x):",
	}, "\n")
	got := rcSinceSymbols(diff)
	want := map[string][]rcChangedSym{
		"pay.go": {
			{Name: "Charge"},
			{Name: "Invoice", Type: true},
			{Name: "Refund"},
			{Name: "legacyRefund", Removed: true},
			{Name: "NewHelper"},
		},
		"gone.py": {{Name: "old_api", Removed: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %+v\nwant      %+v", got, want)
	}
}

func TestSinceKeys(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/ev.go b/ev.go",
		"--- a/ev.go",
		"+++ b/ev.go",
		`@@ -1,3 +1,3 @@`,
		`-	capture("review_run_cost", props)`,
		`+	capture("review_cost_v2", props)`,
		`+	os.Getenv("KAI_REVIEW_X"); route := "/api/v1/runs"`,
		`+	log("hello"); open("config.yaml"); n := "123"; u := "https://x.io/a"`,
		`-	keep := "stay.here"`,
		`+	keep := "stay.here"`,
	}, "\n")
	got := rcSinceKeys(diff)["ev.go"]
	want := []rcChangedKey{
		{Key: "review_run_cost", Removed: true},
		{Key: "review_cost_v2"},
		{Key: "KAI_REVIEW_X"},
		{Key: "/api/v1/runs"},
		{Key: "stay.here"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %+v\nwant   %+v", got, want)
	}
}

// Other files' uses are kept; the key's own file, a key used everywhere and a
// key with no other user are dropped; removed keys are searched first.
func TestResolveKeys(t *testing.T) {
	old := rcKeyGrep
	defer func() { rcKeyGrep = old }()
	var asked []string
	rcKeyGrep = func(lits []string, root string) map[string][]string {
		asked = lits
		many := make([]string, rcLookupTooCommon+1)
		for i := range many {
			many[i] = "f.go:1"
		}
		return map[string][]string{
			"gone_key":   {"ev.go:3", "dash.ts:9", "dash.ts:20"},
			"common_key": many,
			"lonely_key": {"ev.go:4"},
		}
	}
	got := rcResolveKeys(map[string][]rcChangedKey{"ev.go": {
		{Key: "common_key"}, {Key: "lonely_key"}, {Key: "gone_key", Removed: true},
	}}, "")
	if asked[0] != "gone_key" {
		t.Errorf("searched %v, want removed key first", asked)
	}
	want := map[string][]rcChangedKey{"ev.go": {{Key: "gone_key", Removed: true, Uses: map[string][]int{"dash.ts": {9, 20}}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}

func TestSweepFocus(t *testing.T) {
	if f := rcSweepFocus(nil, []rcChained{{Path: "x.go"}}); f != nil {
		t.Fatalf("full review: %v", f)
	}
	// A re-check with no file to read: the sweeps read nothing, not the
	// whole pull request.
	if f := rcSweepFocus([]string{}, nil); f == nil || len(f) != 0 {
		t.Fatalf("re-check: sweep focus = %#v, want empty and non-nil", f)
	}
	order, patches := rcOnlyPatches([]string{"a.go"}, map[string]string{"a.go": "+x"}, rcSweepFocus([]string{}, nil))
	if len(order) != 0 || len(patches) != 0 {
		t.Fatalf("re-check sweeps %v", order)
	}
	got := rcSweepFocus([]string{"a.go"}, []rcChained{{Path: "b.go"}})
	if want := []string{"a.go", "b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sweep focus = %v, want %v", got, want)
	}
}

func TestChainMax_Env(t *testing.T) {
	for v, want := range map[string]int{"": rcChainDefaultMax, "off": 0, "0": 0, "5": 5, "junk": rcChainDefaultMax, "-3": rcChainDefaultMax} {
		t.Setenv("KAI_REVIEW_RESCOPE_CHAIN", v)
		if got := rcChainMax(); got != want {
			t.Errorf("KAI_REVIEW_RESCOPE_CHAIN=%q: max = %d, want %d", v, got, want)
		}
	}
}

// Related files cost one turn per two, and never lift the budget past the
// ceiling.
func TestReReviewTurns(t *testing.T) {
	base := rcReviewMaxTurns(2)
	if got := rcReReviewTurns(2, 0); got != base {
		t.Errorf("no chain: %d, want %d", got, base)
	}
	if got := rcReReviewTurns(2, 5); got != base+3 {
		t.Errorf("5 related: %d, want %d", got, base+3)
	}
	if got := rcReReviewTurns(100, 12); got != rcReviewTurnCeiling {
		t.Errorf("ceiling: %d, want %d", got, rcReviewTurnCeiling)
	}
}

func TestReReviewChainBlock(t *testing.T) {
	if b := rcReReviewChainBlock(nil); b != "" {
		t.Fatalf("empty chain: %q", b)
	}
	b := rcReReviewChainBlock([]rcChained{
		{Path: "billing.go", Kind: rcChainSymbol, Of: "a.go", What: "Charge", Lines: []int{12}},
		{Path: "a_test.go", Kind: rcChainTest, Of: "a.go"},
	})
	for _, want := range []string{
		"RE-REVIEW RELATED FILES",
		"- billing.go — calls Charge (changed in a.go) at line 12\n",
		"- a_test.go — tests a.go\n",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("block missing %q:\n%s", want, b)
		}
	}
}

// With no call site in the graph (a file its parser skipped), a word search
// stands in; a name used everywhere is not listed.
func TestGrepFallback(t *testing.T) {
	old := rcWordGrep
	defer func() { rcWordGrep = old }()
	rcWordGrep = func(name, root string) []string {
		if name == "Common" {
			many := make([]string, rcLookupTooCommon+1)
			for i := range many {
				many[i] = "x.go:1"
			}
			return many
		}
		return []string{"main.go:10", "cmd.go:4", "main.go:900"}
	}
	g := rcGrepFallback{rcChainGraph: &fakeChainGraph{sym: map[string][]rcSite{"a.go|InGraph": {{"g.go", 2}}}}}
	if s := g.SymbolCallers("a.go", "InGraph"); !reflect.DeepEqual(s, []rcSite{{"g.go", 2}}) {
		t.Errorf("graph answer replaced: %v", s)
	}
	if s := g.SymbolCallers("main.go", "Skipped"); !reflect.DeepEqual(s, []rcSite{{"cmd.go", 4}}) {
		t.Errorf("fallback = %v, want the other file's call only", s)
	}
	if s := g.SymbolCallers("a.go", "Common"); s != nil {
		t.Errorf("too common: %v", s)
	}
	if s := (rcGrepFallback{}).SymbolCallers("a.go", "Fn"); s != nil {
		t.Errorf("short name searched: %v", s)
	}
}

// Production call sites rank before test ones.
func TestReReviewChain_ProductionCallSitesFirst(t *testing.T) {
	g := &fakeChainGraph{sym: map[string][]rcSite{"a.go|F": {{"a_test.go", 1}, {"b_test.go", 1}, {"z.go", 1}}}}
	got := chainPaths(rcReReviewChain([]string{"a.go"}, syms("a.go", rcChangedSym{Name: "F"}), g, 2))
	if want := []string{"z.go", "a_test.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
}

// Past the lookup budget nothing more is asked of the graph.
func TestGrepFallback_Deadline(t *testing.T) {
	inner := &fakeChainGraph{callers: map[string][]string{"a.go": {"x.go"}}}
	g := rcGrepFallback{rcChainGraph: inner, deadline: time.Now().Add(-time.Second)}
	if g.Callers("a.go") != nil || g.SymbolCallers("a.go", "Name") != nil || g.Tests("a.go") != nil || g.Uses("a.go") != nil {
		t.Fatal("answered past the deadline")
	}
	if inner.calls != 0 {
		t.Fatalf("graph asked %d times past the deadline", inner.calls)
	}
}

// A hunk of comments and blank lines touches nothing a caller can see, so
// the function its header names is not chained; a declaration on a changed
// line still is.
func TestSinceSymbols_CommentOnlyHunk(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -10 +10,2 @@ func Quiet() {",
		"+	// explains the loop",
		"+",
		"@@ -30 +31 @@ func Loud() {",
		"-	return 1",
		"+	return 2",
	}, "\n")
	got := rcSinceSymbols(diff)["a.go"]
	if want := []rcChangedSym{{Name: "Loud"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %+v, want %+v", got, want)
	}
}

// Whole-file callers, tests and uses are chained only for focus files whose
// code changed; call sites and key users still are for every focus file.
func TestReReviewChain_OnlyCodeChangesFallBack(t *testing.T) {
	g := &fakeChainGraph{
		callers: map[string][]string{"point.go": {"x.go"}, "edited.go": {"y.go"}},
		tests:   map[string][]string{"point.go": {"point_test.go"}},
	}
	in := rcChainInputs{Code: map[string]bool{"edited.go": true}}
	got := chainPaths(rcReReviewChain([]string{"edited.go", "point.go"}, in, g, 10))
	if want := []string{"y.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	if c := rcSinceCode("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n+// note\n+\n"); c["a.go"] {
		t.Fatal("a comment-only change counted as code")
	}
}

// In a diff body "--- x" is a removed "-- x" and "+++ x" an added "++ x":
// part of the file's change, not a new file header.
func TestDiffFiles_HeaderLikeBodyLines(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/q.sql b/q.sql",
		"--- a/q.sql",
		"+++ b/q.sql",
		"@@ -3 +3 @@",
		"--- old note",
		"+++ counter",
		"diff --git a/r.sql b/r.sql",
		"--- a/r.sql",
		"+++ b/r.sql",
		"@@ -1 +1 @@",
		"+select 1;",
	}, "\n")
	got := map[string][]string{}
	rcDiffFiles(diff, func(file, line string) { got[file] = append(got[file], line) })
	want := map[string][]string{
		"q.sql": {"@@ -3 +3 @@", "--- old note", "+++ counter"},
		"r.sql": {"@@ -1 +1 @@", "+select 1;"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines by file = %q\nwant %q", got, want)
	}
}

// Over the limit, the since-diff stops git rather than waiting on it.
func TestSinceDiff_OverLimitReturnsAtOnce(t *testing.T) {
	run, write := gitRepo(t)
	write("big.txt", "x\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	write("big.txt", strings.Repeat("a line of the big change\n", 40000))
	run("commit", "-qam", "big")
	old := rcSinceDiffLimit
	defer func() { rcSinceDiffLimit = old }()
	rcSinceDiffLimit = 1024
	start := time.Now()
	if out := rcSinceDiff("HEAD~1", "", []string{"big.txt"}); len(out) == 0 || len(out) > 1024 {
		t.Fatalf("got %d bytes, want a capped, non-empty diff", len(out))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s: waited on a blocked git", d)
	}
}

// A non-ASCII focus file's changed functions are keyed by its real name, so
// they match the focus.
func TestSinceDiff_NonASCIIName(t *testing.T) {
	run, write := gitRepo(t)
	write("café.go", "package c\n\nfunc Brew() int { return 1 }\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	write("café.go", "package c\n\nfunc Brew() int { return 2 }\n")
	run("commit", "-qam", "change")
	syms := rcSinceSymbols(rcSinceDiff("HEAD~1", "", []string{"café.go"}))
	if got := syms["café.go"]; len(got) != 1 || got[0].Name != "Brew" {
		t.Fatalf("symbols = %+v, want Brew under café.go", syms)
	}
}

// Whether a changed line is a comment depends on the file's language: "#",
// "*" and "--" begin code in some languages and comments in others.
func TestCommentOrBlank_ByLanguage(t *testing.T) {
	cases := []struct {
		file, line string
		comment    bool
	}{
		{"a.go", "\t// note", true},
		{"a.go", "\t/* note", true},
		{"a.go", "\t * continued", true},
		{"a.go", "\t*/", true},
		{"a.go", "\t*p = v", false},
		{"a.go", "", true},
		{"a.c", "#include <stdio.h>", false},
		{"a.c", "\t--i;", false},
		{"a.rs", "#[derive(Debug)]", false},
		{"a.rs", "/// doc", true},
		{"a.py", "    # note", true},
		{"a.py", "    x = 1", false},
		{"a.sh", "# note", true},
		{"a.yaml", "# note", true},
		{"q.sql", "-- note", true},
		{"q.sql", "select 1;", false},
		{"a.lua", "-- note", true},
		{"a.ts", "-- not a comment", false},
		{"page.html", "<!-- note -->", true},
		{"Makefile", "# note", true},
		{"a.unknownext", "# maybe", false}, // unknown: code, so it still chains
	}
	for _, c := range cases {
		if got := rcCommentOrBlank(c.file, c.line); got != c.comment {
			t.Errorf("rcCommentOrBlank(%q, %q) = %v, want %v", c.file, c.line, got, c.comment)
		}
	}
}

// A Go function changed only by a pointer assignment is touched, and its
// file's code changed.
func TestSinceSymbols_PointerAssignmentIsCode(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -10 +10 @@ func (s *S) Set(v int) {",
		"-	*s.p = 0",
		"+	*s.p = v",
	}, "\n")
	if got := rcSinceSymbols(diff)["a.go"]; len(got) != 1 || got[0].Name != "Set" {
		t.Fatalf("symbols = %+v, want Set", got)
	}
	if !rcSinceCode(diff)["a.go"] {
		t.Fatal("a pointer assignment did not count as code")
	}
}

// An added "++i;" is "+++i;" in a diff body: code, not a header.
func TestSinceCode_HeaderLikeCodeLine(t *testing.T) {
	diff := "diff --git a/a.c b/a.c\n--- a/a.c\n+++ b/a.c\n@@ -4 +4 @@ int next(void) {\n+++i;\n"
	if !rcSinceCode(diff)["a.c"] {
		t.Fatal(`"++i;" did not count as code`)
	}
}

// Prose is not code: "class Foo handles…" in a README declares nothing.
func TestSinceSymbols_ProseFilesDeclareNothing(t *testing.T) {
	diff := "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n+class Foo handles retries\n"
	if got := rcSinceSymbols(diff); len(got["README.md"]) != 0 {
		t.Fatalf("symbols from prose: %+v", got)
	}
}

// After the author merges main in, the since-diff holds the pull request's
// own new edits only: a function main rewrote is not one of them.
func TestSinceDiff_BaseMovedOwnChangesOnly(t *testing.T) {
	run, write := gitRepo(t)
	src := func(parse, other string) string {
		return "package u\n\nfunc Parse() int {\n\treturn " + parse + "\n}\n\n\n\n\n\nfunc Other() int {\n\treturn " + other + "\n}\n"
	}
	write("util.go", src("1", "1"))
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-q", "-b", "pr")
	write("util.go", src("1", "2"))
	run("commit", "-qam", "pr")
	reviewed := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	write("util.go", src("9", "1")) // main rewrites Parse
	run("commit", "-qam", "main moves")
	run("checkout", "-q", "pr")
	run("merge", "-q", "--no-edit", "main")
	write("util.go", src("9", "3")) // the author edits Other again
	run("commit", "-qam", "fix")
	old := reviewCommitBase
	defer func() { reviewCommitBase = old }()
	reviewCommitBase = "main"
	got := rcSinceSymbols(rcSinceDiff(reviewed, "", []string{"util.go"}))["util.go"]
	if len(got) != 1 || got[0].Name != "Other" {
		t.Fatalf("symbols = %+v, want Other only", got)
	}
}

// The review's file list quotes a non-ASCII path; the since-diff and the
// chain still find the file.
func TestSinceDiff_QuotedFocus(t *testing.T) {
	run, write := gitRepo(t)
	write("café.go", "package c\n\nfunc Brew() int { return 1 }\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	write("café.go", "package c\n\nfunc Brew() int { return 2 }\n")
	run("commit", "-qam", "change")
	syms := rcSinceSymbols(rcSinceDiff("HEAD~1", "", []string{`"caf\303\251.go"`}))
	if got := syms["café.go"]; len(got) != 1 || got[0].Name != "Brew" {
		t.Fatalf("symbols = %+v, want Brew under café.go", syms)
	}
}

// git grep quotes non-ASCII paths unless told not to; the fallback's sites
// are real paths, and the focus file's own lines are not its callers.
func TestWordGrep_UnquotedPaths(t *testing.T) {
	run, write := gitRepo(t)
	write("café.go", "package c\n\nfunc Brew() int { return 1 }\n\nvar x = Brew()\n")
	write("uses.go", "package c\n\nvar y = Brew()\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	dir, _ := os.Getwd()
	got := rcGrepFallback{root: dir}.SymbolCallers("café.go", "Brew")
	if want := []rcSite{{"uses.go", 3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sites = %+v, want %+v", got, want)
	}
}

// kai review-commit run from a subdirectory: the focus paths are relative to
// the repository root, and the since-diff still finds them.
func TestSinceDiff_FromSubdirectory(t *testing.T) {
	run, write := gitRepo(t)
	root, _ := os.Getwd()
	if err := os.MkdirAll("pkg/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	write("pkg/sub/s.go", "package s\n\nfunc Run() int { return 1 }\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	write("pkg/sub/s.go", "package s\n\nfunc Run() int { return 2 }\n")
	run("commit", "-qam", "change")
	t.Chdir("pkg")
	got := rcSinceSymbols(rcSinceDiff("HEAD~1", root, []string{"pkg/sub/s.go"}))["pkg/sub/s.go"]
	if len(got) != 1 || got[0].Name != "Run" {
		t.Fatalf("symbols = %+v, want Run", got)
	}
}
