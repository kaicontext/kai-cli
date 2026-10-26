package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectionPublishesTheChosenDefectsInOrder(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:1 — typo in a test name", Status: rcStatusSupported},
		{ID: 2, Issue: "a.go:9 — nil map write panics", Status: rcStatusSupported},
		{ID: 3, Issue: "a.go:4 — wrong unit", Status: rcStatusRefuted},
		{ID: 4, Issue: "b.go:2 — race on counter", Status: rcStatusSupported},
	}}
	kept, dropped := rcApplyRank(res, rcRankAnswer{Publish: []int{4, 2}, Skip: map[int]string{1: "test naming"}}, 3)
	if kept != 2 || dropped != 1 {
		t.Fatalf("kept, dropped = %d, %d; want 2, 1", kept, dropped)
	}
	var got []int
	for _, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			got = append(got, a.ID)
		}
	}
	if !reflect.DeepEqual(got, []int{4, 2}) {
		t.Errorf("published %v, want [4 2] in the chosen order", got)
	}
	for _, a := range res.Allegations {
		if a.ID == 1 && (a.Status != rcStatusRefuted || !strings.Contains(a.Reason, "test naming")) {
			t.Errorf("a skipped defect must be refuted with the selection's reason, got %q %q", a.Status, a.Reason)
		}
	}
}

func TestSelectionCapsAndNeverPublishesNothing(t *testing.T) {
	mk := func() *rcChallengeResult {
		var r rcChallengeResult
		for i := 1; i <= 5; i++ {
			r.Allegations = append(r.Allegations, rcAllegationResult{ID: i, Issue: "x.go:1 — d", Status: rcStatusSupported})
		}
		return &r
	}
	// Past the main cap, chosen defects move to the minor list (cap 3): 3 + 2.
	if kept, _ := rcApplyRank(mk(), rcRankAnswer{Publish: []int{5, 4, 3, 2, 1}}, 3); kept != 5 {
		t.Errorf("kept %d; want 3 main + 2 overflowed into minor", kept)
	}
	var big rcChallengeResult
	for i := 1; i <= 9; i++ {
		big.Allegations = append(big.Allegations, rcAllegationResult{ID: i, Issue: "x.go:1 — d", Status: rcStatusSupported})
	}
	if kept, _ := rcApplyRank(&big, rcRankAnswer{Publish: []int{1, 2, 3, 4, 5, 6, 7, 8, 9}}, 3); kept != 3+rcRankMinorCap {
		t.Errorf("kept %d; the overflow must stop at the minor cap", kept)
	}
	res := mk()
	if kept, _ := rcApplyRank(res, rcRankAnswer{Skip: map[int]string{1: "", 2: ""}}, 3); kept != 1 || res.Allegations[0].ID != 1 {
		t.Errorf("an empty choice must still publish the first confirmed defect; kept %d, first %d", kept, res.Allegations[0].ID)
	}
}

// A small, real defect the selection marks minor is published after the main
// ones, past the main cap, with a cap of its own; it is not held back.
func TestSelectionPublishesMinorDefectsAfterTheOthers(t *testing.T) {
	var res rcChallengeResult
	for i := 1; i <= 8; i++ {
		res.Allegations = append(res.Allegations, rcAllegationResult{ID: i, Issue: "x.go:1 — d", Status: rcStatusSupported})
	}
	kept, dropped := rcApplyRank(&res, rcRankAnswer{
		Publish: []int{2, 1},
		Minor:   []int{1, 5, 6, 7, 8}, // 1 is already published; only three of the rest fit
		Skip:    map[int]string{3: "cosmetic", 4: "duplicate"},
	}, 3)
	if kept != 5 || dropped != 3 {
		t.Fatalf("kept, dropped = %d, %d; want 5, 3", kept, dropped)
	}
	var got []int
	for _, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			got = append(got, a.ID)
		}
	}
	if !reflect.DeepEqual(got, []int{2, 1, 5, 6, 7}) {
		t.Errorf("published %v, want [2 1 5 6 7]: main first, then minor up to its cap, no repeats", got)
	}
}

// When every confirmed defect is minor, the minor ones are the review; the
// never-publish-nothing fallback must not add a defect the selection skipped.
func TestAllMinorSelectionPublishesOnlyTheMinorOnes(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:1 — dead code", Status: rcStatusSupported},
		{ID: 2, Issue: "a.go:2 — metric tag key differs from siblings", Status: rcStatusSupported},
	}}
	kept, _ := rcApplyRank(res, rcRankAnswer{Minor: []int{2}, Skip: map[int]string{1: "dead code"}}, 3)
	if kept != 1 || res.Allegations[0].ID != 2 || res.Allegations[0].Status != rcStatusSupported {
		t.Fatalf("kept %d, first %+v; want only the minor defect 2 published", kept, res.Allegations[0])
	}
	if res.Allegations[1].Status != rcStatusRefuted {
		t.Errorf("the skipped defect was published: %+v", res.Allegations[1])
	}
}

func TestSelectionAnswerParses(t *testing.T) {
	ans, err := rcParseRank("Here you go:\n```json\n{\"publish\":[3,1],\"minor\":[4],\"skip\":[{\"id\":2,\"why\":\"cosmetic\"}]}\n```")
	if err != nil || !reflect.DeepEqual(ans.Publish, []int{3, 1}) || !reflect.DeepEqual(ans.Minor, []int{4}) || ans.Skip[2] != "cosmetic" {
		t.Fatalf("parse = %+v %v", ans, err)
	}
	if _, err := rcParseRank("no json here"); err == nil {
		t.Error("an answer without JSON must be an error, so the review keeps every confirmed defect")
	}
}

func TestRankCapGrowsWithTheChange(t *testing.T) {
	// This covers the base cap (3..6), which KAI_REVIEW_LATENT_DEFECTS must not
	// change; pin the flag off so the suite is deterministic even when a
	// flag-on A/B run exports it. The +2 bump is asserted in
	// TestRankCapRaisedUnderFlag.
	t.Setenv("KAI_REVIEW_LATENT_DEFECTS", "")
	prev := 0
	for _, n := range []int{1, 3, 4, 10, 11, 25, 26, 200} {
		c := rcRankCap(n)
		if c < prev || c < 3 || c > 6 {
			t.Errorf("rcRankCap(%d) = %d", n, c)
		}
		prev = c
	}
}

func TestPublishedProseKeepsOnlyDefects(t *testing.T) {
	prose := "## Scope\n- a.go\n\n## Findings\n\n### a.go:1 — bug\nx\n\n## Could not verify\n- maybe — why\n\n## Limitations\n- did not read b\n\n## Decisions (need your call)\n- ship it\n"
	got := rcDefectsOnlyProse(prose)
	for _, gone := range []string{"Could not verify", "maybe", "Limitations", "did not read b"} {
		if strings.Contains(got, gone) {
			t.Errorf("published prose still has %q:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"## Scope", "### a.go:1 — bug", "## Decisions (need your call)", "ship it"} {
		if !strings.Contains(got, kept) {
			t.Errorf("published prose lost %q:\n%s", kept, got)
		}
	}
}

func TestTwoSweepsMergeWithoutRepeats(t *testing.T) {
	a := rcSweepResult{Issues: []string{"a.go:1 — x", "a.go:2 — y"}, Sources: []rcSource{{}}, Chunks: 1}
	b := rcSweepResult{Issues: []string{"a.go:2 — y", "a.go:3 — z"}, Chunks: 1}
	got := rcMergeSweeps(a, b)
	if !reflect.DeepEqual(got.Issues, []string{"a.go:1 — x", "a.go:2 — y", "a.go:3 — z"}) || len(got.Sources) != 1 {
		t.Errorf("merged = %v (sources %d)", got.Issues, len(got.Sources))
	}
}

func TestEnsembleStagesCanBeTurnedOff(t *testing.T) {
	t.Setenv("KAI_FINDER2_MODEL", "off")
	t.Setenv("KAI_RANK_MODEL", "none")
	if m := rcEnsembleModel(rcStageFinder2); m != "" {
		t.Errorf("finder2 = %q with KAI_FINDER2_MODEL=off", m)
	}
	if m := rcEnsembleModel(rcStageRank); m != "" {
		t.Errorf("rank = %q with KAI_RANK_MODEL=none", m)
	}
	if m := rcEnsembleModel(rcStageSweep2); m == "" {
		t.Error("sweep2 has a default model")
	}
}

func TestAnIssueNamedByFunctionGetsItsLine(t *testing.T) {
	files := map[string][]string{"cmd/kai/ship.go": {"package main", "", "// shipIsMergeSubject is used below", "func shipIsMergeSubject(s string) bool {", "\treturn false", "}"}}
	read := func(_, p string) ([]string, bool) { l, ok := files[p]; return l, ok }
	tree := []string{"cmd/kai/ship.go"}
	got := rcLocateNamedIssue("h", "cmd/kai/ship.go:shipIsMergeSubject — matches too much", tree, read)
	if got != "cmd/kai/ship.go:4 — matches too much" {
		t.Errorf("got %q, want the definition line", got)
	}
	if got := rcLocateNamedIssue("h", "`cmd/kai/ship.go:main.shipIsMergeSubject` — x", tree, read); got != "`cmd/kai/ship.go:4` — x" {
		t.Errorf("qualified name: got %q", got)
	}
	for _, keep := range []string{"cmd/kai/ship.go:12 — has a line", "cmd/kai/ship.go:nowhere — unknown symbol", "missing.go:foo — no such file"} {
		if got := rcLocateNamedIssue("h", keep, tree, read); got != keep {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

func TestASlowSecondFinderIsLeftBehind(t *testing.T) {
	old := rcSecondFinderGrace
	rcSecondFinderGrace = 10 * time.Millisecond
	defer func() { rcSecondFinderGrace = old }()
	cancelled := false
	sf := &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: func() { cancelled = true }, model: "slow"}
	if r := rcAwaitSecondFinder(sf); r.Raw != "" || len(r.Issues) != 0 || !cancelled {
		t.Errorf("a finder past its grace must be cancelled and contribute nothing; got %+v cancelled=%v", r, cancelled)
	}
	sf = &rcSecondFinder{ch: make(chan rcFinderResult, 1), cancel: func() {}, model: "fast"}
	sf.ch <- rcFinderResult{Model: "fast", Issues: []string{"a.go:1 — x"}, Raw: "r"}
	if r := rcAwaitSecondFinder(sf); len(r.Issues) != 1 {
		t.Errorf("a finished finder's issues were lost: %+v", r)
	}
}

// A confirmed earlier point the selection step leaves out is still in the
// code: its thread must not be closed as withdrawn (Kai's review of #188).
func TestASelectionSkipDoesNotWithdrawAnEarlierPoint(t *testing.T) {
	issue := "src/d.go:4 — reads past the end"
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "src/a.go:1 — first", Status: rcStatusSupported},
		{ID: 2, Issue: issue, Status: rcStatusSupported},
	}}
	rcApplyRank(res, rcRankAnswer{Publish: []int{1}}, 1)
	if res.Allegations[1].Status != rcStatusRefuted || !strings.HasPrefix(res.Allegations[1].Reason, rcSelectionSkipPrefix) {
		t.Fatalf("setup: allegation 2 = %+v, want a selection skip", res.Allegations[1])
	}
	tags := map[string]string{}
	rcRememberTags(tags, "issues", "- "+issue, "(earlier: 999999999999)")
	prior := &rcPrior{Head: "abc", Points: []rcPriorPoint{{Kind: "defect", Ref: "999999999999"}}}
	for _, s := range rcEarlierStatuses(prior, nil, "", res, tags, nil, true) {
		if s.Ref == "999999999999" && s.Status == rcEarlierWithdrawn {
			t.Errorf("a confirmed point the selection step skipped was withdrawn: %+v", s)
		}
	}
}

// A doc comment that names the function and a keyword is not its definition.
func TestANamedIssueSkipsDocComments(t *testing.T) {
	files := map[string][]string{"a.go": {"package main", "// shipIsMergeSubject returns the type of a subject", "var x = 1", "func shipIsMergeSubject(s string) bool {", "\treturn false", "}"}}
	read := func(_, p string) ([]string, bool) { l, ok := files[p]; return l, ok }
	if got := rcLocateNamedIssue("h", "a.go:shipIsMergeSubject — x", []string{"a.go"}, read); got != "a.go:4 — x" {
		t.Errorf("got %q, want the func line, not the doc comment", got)
	}
}

// A call site that assigns the result is not the declaration, and a
// sentence-ending dot is not part of the name (Kai's review of #188).
func TestANamedIssueSkipsCallSitesAndTrailingDots(t *testing.T) {
	files := map[string][]string{"a.go": {"package main", "var ok = shipIsMergeSubject(\"x\")", "func (r *T) shipIsMergeSubject(s string) bool {", "\treturn false", "}"}}
	read := func(_, p string) ([]string, bool) { l, ok := files[p]; return l, ok }
	if got := rcLocateNamedIssue("h", "a.go:shipIsMergeSubject — x", []string{"a.go"}, read); got != "a.go:3 — x" {
		t.Errorf("call site: got %q, want the method line", got)
	}
	if got := rcLocateNamedIssue("h", "a.go:shipIsMergeSubject. It matches too much", []string{"a.go"}, read); got != "a.go:3. It matches too much" {
		t.Errorf("trailing dot: got %q", got)
	}
}

// The summary does not count a selection skip as refuted.
func TestASelectionSkipIsNotCountedAsRefuted(t *testing.T) {
	s, r, u := rcCountAllegations([]rcAllegationResult{
		{Status: rcStatusSupported, Issue: "a.go:1 — x"},
		{Status: rcStatusRefuted, Reason: rcSelectionSkipPrefix + "outside the 3 most important"},
		{Status: rcStatusRefuted, Reason: "the caller validates it"},
	})
	if s != 1 || r != 1 || u != 0 {
		t.Errorf("counts = %d supported, %d refuted, %d unresolved; want 1, 1, 0", s, r, u)
	}
}

// A finding that quotes a heading mid-line does not stop the real section
// after it from being removed (Kai's review of #188).
func TestDefectsOnlyProseSkipsAQuotedHeading(t *testing.T) {
	prose := "## Findings\n- the doc says see ## Limitations below\n\n## Limitations\n- read 3 files\n"
	got := rcDefectsOnlyProse(prose)
	if strings.Contains(got, "read 3 files") || !strings.Contains(got, "see ## Limitations below") {
		t.Errorf("got %q", got)
	}
}

// A set-but-empty stage variable falls back to the default; "off" turns it off.
func TestAnEmptyEnsembleVariableKeepsTheDefault(t *testing.T) {
	t.Setenv("KAI_RANK_MODEL", "")
	if rcEnsembleModel(rcStageRank) == "" {
		t.Error("empty KAI_RANK_MODEL turned the rank stage off")
	}
	t.Setenv("KAI_RANK_MODEL", "off")
	if m := rcEnsembleModel(rcStageRank); m != "" {
		t.Errorf("off: got %q", m)
	}
}

// With no declaration, a named issue stays unlocated rather than landing on
// an import or call site.
func TestANamedIssueWithNoDeclarationStaysUnlocated(t *testing.T) {
	files := map[string][]string{"A.java": {"import x.shipIsMergeSubject;", "class A {", "  boolean shipIsMergeSubject(String s) { return false; }", "}"}}
	read := func(_, p string) ([]string, bool) { l, ok := files[p]; return l, ok }
	in := "A.java:shipIsMergeSubject — x"
	if got := rcLocateNamedIssue("h", in, []string{"A.java"}, read); got != in {
		t.Errorf("got %q, want it unchanged", got)
	}
}
