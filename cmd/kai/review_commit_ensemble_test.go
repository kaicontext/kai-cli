package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSelectionPublishesTheChosenDefectsInOrder(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:1 — typo in a test name", Status: rcStatusSupported},
		{ID: 2, Issue: "a.go:9 — nil map write panics", Status: rcStatusSupported},
		{ID: 3, Issue: "a.go:4 — wrong unit", Status: rcStatusRefuted},
		{ID: 4, Issue: "b.go:2 — race on counter", Status: rcStatusSupported},
	}}
	kept, dropped := rcApplyRank(res, []int{4, 2}, map[int]string{1: "test naming"}, 3)
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
	if kept, _ := rcApplyRank(mk(), []int{5, 4, 3, 2, 1}, nil, 3); kept != 3 {
		t.Errorf("kept %d past a cap of 3", kept)
	}
	res := mk()
	if kept, _ := rcApplyRank(res, nil, map[int]string{1: "", 2: ""}, 3); kept != 1 || res.Allegations[0].ID != 1 {
		t.Errorf("an empty choice must still publish the first confirmed defect; kept %d, first %d", kept, res.Allegations[0].ID)
	}
}

func TestSelectionAnswerParses(t *testing.T) {
	pub, skip, err := rcParseRank("Here you go:\n```json\n{\"publish\":[3,1],\"skip\":[{\"id\":2,\"why\":\"cosmetic\"}]}\n```")
	if err != nil || !reflect.DeepEqual(pub, []int{3, 1}) || skip[2] != "cosmetic" {
		t.Fatalf("parse = %v %v %v", pub, skip, err)
	}
	if _, _, err := rcParseRank("no json here"); err == nil {
		t.Error("an answer without JSON must be an error, so the review keeps every confirmed defect")
	}
}

func TestRankCapGrowsWithTheChange(t *testing.T) {
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
