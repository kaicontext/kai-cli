package main

import (
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
