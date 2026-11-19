package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

// The check numbers "Duplicate of issue N" within its batch; the merged
// result carries the allegation it means across batches.
func TestMergeBatchesRenumbersDuplicates(t *testing.T) {
	batches := [][]string{{"a1", "a2", "a3", "a4", "a5"}, {"b1", "b2", "b3"}, {"c1"}}
	res := func(reasons ...string) *rcChallengeResult {
		r := &rcChallengeResult{match: finding.MatchPartial, proposed: finding.ReadinessMerge}
		for i, why := range reasons {
			r.Allegations = append(r.Allegations, rcAllegationResult{Issue: "x" + string(rune('0'+i)), Status: rcStatusRefuted, Reason: why})
		}
		return r
	}
	merged, err := rcMergeBatches(batches, nil,
		[]*rcChallengeResult{res("", "", "", "", ""), res("", "", "Duplicate of issue 2 (same check)"), res("Duplicate of issue 4")},
		[]error{nil, nil, nil})
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Allegations[7].DuplicateOf; got != 7 {
		t.Errorf("batch 2's duplicate of its issue 2 points at %d, want 7", got)
	}
	if got := merged.Allegations[8].DuplicateOf; got != 0 {
		t.Errorf("an issue number outside its batch points at %d, want 0", got)
	}
}

func gateFixture() *rcChallengeResult {
	return &rcChallengeResult{match: finding.MatchPartial, proposed: finding.ReadinessSmallFixes, Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:11 — raised three times", Status: rcStatusSupported},
		{ID: 4, Issue: "a.go:12 — raised once", Status: rcStatusSupported},
		{ID: 2, Issue: "a.go:11 — the same, reworded", Status: rcStatusRefuted, Reason: "Duplicate of issue 1", DuplicateOf: 1},
		{ID: 3, Issue: "a.go:10 — the same again", Status: rcStatusRefuted, Reason: rcSelectionSkipPrefix + "repeat of 2 (same check)"},
		{ID: 5, Issue: "a.go:40 — far from the change", Status: rcStatusSupported},
		{ID: 6, Issue: "b.go:5 — elsewhere (also: a.go:15)", Status: rcStatusSupported},
	}}
}

func gateRepo() *rcRepo { return &rcRepo{changed: map[string][][2]int{"a.go": {{10, 12}}}} }

func TestPublishGateCountsVotesAndWithholds(t *testing.T) {
	res := gateFixture()
	rcPublishGate(res, gateRepo(), true)
	votes := map[int]int{}
	status := map[int]string{}
	for _, a := range res.Allegations {
		votes[a.ID], status[a.ID] = a.Votes, a.Status
		if a.Status == rcStatusRefuted && a.ID != 2 && !strings.HasPrefix(a.Reason, rcSelectionSkipPrefix) {
			t.Errorf("withheld %d reads as refuted: %q", a.ID, a.Reason)
		}
	}
	if votes[1] != 3 || votes[2] != 3 || votes[3] != 3 || votes[4] != 1 {
		t.Errorf("votes = %v, want 3 for the chain 3→2→1 and 1 for issue 4", votes)
	}
	if status[1] != rcStatusSupported {
		t.Error("the corroborated finding was withheld")
	}
	if status[4] != rcStatusRefuted {
		t.Error("a single-proposal finding was published with corroboration on")
	}
	if status[5] != rcStatusRefuted {
		t.Error("a finding 28 lines from the change was published")
	}
	if status[6] != rcStatusRefuted {
		t.Error("a single-proposal finding near the change only through its also-list should still need a second proposal")
	}

	// Without corroboration only the distance applies; an also-location in
	// the change counts as in the change.
	res = gateFixture()
	rcPublishGate(res, gateRepo(), false)
	for _, a := range res.Allegations {
		if want := map[int]string{1: rcStatusSupported, 4: rcStatusSupported, 5: rcStatusRefuted, 6: rcStatusSupported}[a.ID]; want != "" && a.Status != want {
			t.Errorf("corroboration off: %d is %s, want %s", a.ID, a.Status, want)
		}
	}

	// No changed ranges known: nothing is withheld for distance.
	res = gateFixture()
	rcPublishGate(res, nil, false)
	if res.Allegations[4].Status != rcStatusSupported {
		t.Error("withheld for distance with no diff ranges")
	}
}

func TestPublishGateNeverEmptiesAReview(t *testing.T) {
	res := &rcChallengeResult{match: finding.MatchPartial, proposed: finding.ReadinessSmallFixes, Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:90 — first", Status: rcStatusSupported, Reason: "kept reason"},
		{ID: 2, Issue: "a.go:95 — second", Status: rcStatusSupported},
	}}
	rcPublishGate(res, gateRepo(), true)
	if a := res.Allegations[0]; a.Status != rcStatusSupported || a.Reason != "kept reason" {
		t.Errorf("the selection's first was not kept as it was: %+v", a)
	}
	if res.Allegations[1].Status != rcStatusRefuted {
		t.Error("the fallback kept more than one")
	}
}

func TestPublishGateSwitchOff(t *testing.T) {
	t.Setenv("KAI_REVIEW_PUBLISH_GATE", "0")
	res := gateFixture()
	rcPublishGate(res, gateRepo(), true)
	for i, a := range res.Allegations {
		if a.Status != gateFixture().Allegations[i].Status || a.Votes != 0 {
			t.Errorf("gate off changed %d: %+v", a.ID, a)
		}
	}
}

// The cap runs before the gate, so a defect it cut used to stay cut even when
// the gate then freed a slot. With the refill on, the cut ones take the freed
// slots in the selection's order, when they pass the same gate.
func TestRefillGivesGateFreedSlotsToWhatTheCapCut(t *testing.T) {
	mk := func() *rcChallengeResult {
		var al []rcAllegationResult
		for i := 1; i <= 6; i++ {
			al = append(al, rcAllegationResult{ID: i, Issue: fmt.Sprintf("a.go:%d — defect %d", i*100, i), Status: rcStatusSupported, Reason: "confirmed"})
		}
		res := &rcChallengeResult{Allegations: al}
		rcMarkRank(res, rcRankAnswer{Publish: []int{1, 2, 3, 4, 5, 6}}, 1)
		rcApplyRank(res, rcRankAnswer{Publish: []int{1, 2, 3, 4, 5, 6}}, 1)
		return res
	}
	// Defects 2 and 3 sit far from the change; 5 and 6 (cut by the cap) are near it, 4 is near too.
	repo := &rcRepo{changed: map[string][][2]int{"a.go": {{100, 100}, {400, 400}, {500, 500}, {600, 600}}}}
	published := func(res *rcChallengeResult) []int {
		var ids []int
		for _, a := range res.Allegations {
			if a.Status == rcStatusSupported {
				ids = append(ids, a.ID)
			}
		}
		sort.Ints(ids)
		return ids
	}
	res := mk()
	rcPublishGate(res, repo, false)
	if got := published(res); !reflect.DeepEqual(got, []int{1, 4}) {
		t.Fatalf("without the refill, published %v, want [1 4]", got)
	}
	t.Setenv("KAI_REVIEW_REFILL", "1")
	res = mk()
	rcPublishGate(res, repo, false)
	if got := published(res); !reflect.DeepEqual(got, []int{1, 4, 5, 6}) {
		t.Errorf("with the refill, published %v, want [1 4 5 6]: two slots were freed, the two cut defects take them", got)
	}

	// By depth: on for standard and deep, off for light and for the values that
	// name no depth, and KAI_REVIEW_REFILL=0 wins over the depth.
	for _, tc := range []struct {
		tier, refill string
		want         []int
	}{
		{"standard", "", []int{1, 4, 5, 6}},
		{"deep", "", []int{1, 4, 5, 6}},
		{"light", "", []int{1, 4}},
		{"auto", "", []int{1, 4, 5, 6}},
		{"auto", "0", []int{1, 4}},
		{"AUTO", "", []int{1, 4, 5, 6}},
		{"free", "", []int{1, 4}},
		{"baseline", "", []int{1, 4}},
		{"", "", []int{1, 4}},
		{"standard", "0", []int{1, 4}},
		{"deep", "0", []int{1, 4}},
		{"light", "1", []int{1, 4, 5, 6}},
	} {
		t.Setenv("KAI_REVIEW_TIER", tc.tier)
		t.Setenv("KAI_REVIEW_REFILL", tc.refill)
		res := mk()
		rcPublishGate(res, repo, false)
		if got := published(res); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tier %q refill %q: published %v, want %v", tc.tier, tc.refill, got, tc.want)
		}
	}
}

// In standard and deep, a defect two proposals raised that the cap cut takes
// the slot the gate frees by withholding a single-proposal one.
func TestRefillPublishesTwoVoteDefectTheCapCut(t *testing.T) {
	t.Setenv("KAI_REVIEW_REFILL", "")
	mk := func() *rcChallengeResult {
		var al []rcAllegationResult
		for i := 1; i <= 6; i++ {
			al = append(al, rcAllegationResult{ID: i, Issue: fmt.Sprintf("a.go:%d — defect %d", 100+i, i), Status: rcStatusSupported, Reason: "confirmed"})
		}
		// Votes count the allegations of a duplicate group: 1 and 5 each have a
		// second proposal's copy (7, 8), so they carry two votes.
		al = append(al,
			rcAllegationResult{ID: 7, Issue: "a.go:101 — copy of 1", Status: rcStatusRefuted, DuplicateOf: 1},
			rcAllegationResult{ID: 8, Issue: "a.go:105 — copy of 5", Status: rcStatusRefuted, DuplicateOf: 5})
		// One main slot and three minor ones: 1-4 are published, 5 (two
		// proposals) and 6 (one) are cut. The gate withholds the single-proposal
		// 2-4, freeing three slots; only 5 passes the gate to take one.
		ans := rcRankAnswer{Publish: []int{1, 2, 3, 4, 5, 6}}
		res := &rcChallengeResult{Allegations: al}
		rcMarkRank(res, ans, 1)
		rcApplyRank(res, ans, 1)
		return res
	}
	repo := &rcRepo{changed: map[string][][2]int{"a.go": {{100, 110}}}}
	published := func(res *rcChallengeResult) []int {
		var ids []int
		for _, a := range res.Allegations {
			if a.Status == rcStatusSupported {
				ids = append(ids, a.ID)
			}
		}
		sort.Ints(ids)
		return ids
	}
	for _, tc := range []struct {
		tier, refill string
		want         []int
	}{
		{"light", "", []int{1}},
		{"standard", "", []int{1, 5}},
		{"deep", "", []int{1, 5}},
		{"auto", "", []int{1, 5}},
		{"auto", "0", []int{1}},
		{"standard", "0", []int{1}},
	} {
		t.Setenv("KAI_REVIEW_TIER", tc.tier)
		t.Setenv("KAI_REVIEW_REFILL", tc.refill)
		res := mk()
		rcPublishGate(res, repo, true)
		if got := published(res); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tier %q refill %q: published %v, want %v", tc.tier, tc.refill, got, tc.want)
		}
	}
}

// The "never publish nothing" fallback holds in every depth: when the gate
// would withhold everything, the selection's first stays and nothing refills.
func TestPublishGateFallbackHoldsWithRefillOn(t *testing.T) {
	for _, tier := range []string{"standard", "deep", "auto"} {
		t.Setenv("KAI_REVIEW_TIER", tier)
		res := &rcChallengeResult{match: finding.MatchPartial, proposed: finding.ReadinessSmallFixes, Allegations: []rcAllegationResult{
			{ID: 1, Issue: "a.go:90 — first", Status: rcStatusSupported, Reason: "kept reason"},
			{ID: 2, Issue: "a.go:95 — second", Status: rcStatusSupported},
		}}
		rcPublishGate(res, gateRepo(), true)
		if res.Allegations[0].Status != rcStatusSupported || res.Allegations[1].Status != rcStatusRefuted {
			t.Errorf("%s: fallback broke: %+v", tier, res.Allegations)
		}
	}
}

// bypassFixture builds a review the cap has already cut. The first `limit` defects are published
// the others are listed in order after them and the cap cuts them.
// Each defect has 2 votes unless votes says otherwise (duplicate allegations).
// Every defect cites a.go:<id*100>; the change covers the lines in near.
func bypassFixture(limit int, order []int, votes map[int]int, near []int, repoPath string) (*rcChallengeResult, *rcRepo) {
	var al []rcAllegationResult
	for _, id := range order {
		al = append(al, rcAllegationResult{ID: id, Issue: fmt.Sprintf("a.go:%d — defect %d", id*100, id), Status: rcStatusSupported, Reason: "confirmed"})
	}
	dup := 100
	for _, id := range order {
		v := 2 // corroborated, so only the cap and the distance decide
		if n, ok := votes[id]; ok {
			v = n
		}
		for k := 1; k < v; k++ {
			dup++
			al = append(al, rcAllegationResult{ID: dup, Issue: "dup", Status: rcStatusRefuted, DuplicateOf: id, Reason: fmt.Sprintf("Duplicate of issue %d", id)})
		}
	}
	res := &rcChallengeResult{Allegations: al}
	rcMarkRank(res, rcRankAnswer{Publish: order}, limit)
	rcApplyRank(res, rcRankAnswer{Publish: order[:limit]}, limit) // the cap cuts the rest; rankPos keeps the order
	repo := &rcRepo{changed: map[string][][2]int{}}
	for _, n := range near {
		repo.changed["a.go"] = append(repo.changed["a.go"], [2]int{n * 100, n * 100})
	}
	return res, repo
}

func bypassPublished(res *rcChallengeResult) (ids, bypassed []int) {
	for _, a := range res.Allegations {
		if a.Status == rcStatusSupported {
			ids = append(ids, a.ID)
			if a.VoteBypass {
				bypassed = append(bypassed, a.ID)
			}
		}
	}
	sort.Ints(ids)
	return
}

func TestVoteBypassPublishesCapCutDefectWithThreeVotes(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 3}, []int{1, 2}, "a.go")
	rcPublishGate(res, repo, true)
	ids, by := bypassPublished(res)
	if !reflect.DeepEqual(ids, []int{1, 2}) || !reflect.DeepEqual(by, []int{2}) {
		t.Fatalf("published %v bypassed %v, want [1 2] / [2]", ids, by)
	}
}

func TestVoteBypassNeedsThreeVotes(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	t.Setenv("KAI_REVIEW_REFILL", "0")
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 2}, []int{1, 2}, "a.go")
	rcPublishGate(res, repo, true)
	if ids, by := bypassPublished(res); !reflect.DeepEqual(ids, []int{1}) || len(by) != 0 {
		t.Fatalf("2 votes: published %v bypassed %v, want [1] / none", ids, by)
	}
	// With the refill on and a freed slot, the refill (not the bypass) brings it back.
	t.Setenv("KAI_REVIEW_REFILL", "1")
	res, repo = bypassFixture(2, []int{1, 3, 2}, map[int]int{2: 2}, []int{1, 2}, "a.go") // 3 is far: frees a slot
	rcPublishGate(res, repo, true)
	if ids, by := bypassPublished(res); !reflect.DeepEqual(ids, []int{1, 2}) || len(by) != 0 {
		t.Fatalf("refill: published %v bypassed %v, want [1 2] / none", ids, by)
	}
}

func TestVoteBypassKeepsOutsideChangeWithheld(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 4}, []int{1}, "a.go") // 2 is far from the change
	rcPublishGate(res, repo, true)
	if ids, _ := bypassPublished(res); !reflect.DeepEqual(ids, []int{1}) {
		t.Fatalf("published %v, want [1]: defect 2 sits outside the change", ids)
	}
}

func TestVoteBypassLeavesRankerSkipsAlone(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 3}, []int{1, 2}, "a.go")
	for i := range res.Allegations {
		if res.Allegations[i].ID == 2 {
			res.Allegations[i].Reason = rcSelectionSkipPrefix + "repeat of 1"
		}
	}
	rcPublishGate(res, repo, true)
	if ids, _ := bypassPublished(res); !reflect.DeepEqual(ids, []int{1}) {
		t.Fatalf("published %v, want [1]: a ranker skip stays skipped", ids)
	}
}

func TestVoteBypassTakesWhatTheRefillLeaves(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	// 1 published, 2 far (frees one slot), 3 and 4 cut with 3 votes each.
	res, repo := bypassFixture(2, []int{1, 2, 3, 4}, map[int]int{3: 3, 4: 3}, []int{1, 3, 4}, "a.go")
	rcPublishGate(res, repo, true)
	ids, by := bypassPublished(res)
	if !reflect.DeepEqual(ids, []int{1, 3, 4}) {
		t.Fatalf("published %v, want [1 3 4]", ids)
	}
	if !reflect.DeepEqual(by, []int{4}) {
		t.Errorf("bypassed %v, want [4]: the refill takes the higher-ranked cut defect (3), the bypass the other", by)
	}
}

func TestVoteBypassSwitches(t *testing.T) {
	for _, tc := range []struct {
		tier, sw string
		want     bool
	}{
		{"light", "", false},
		{"", "", false},
		{"free", "", false},
		{"standard", "", true},
		{"deep", "", true},
		{"AUTO", "", true},
		{"deep", "0", false},
		{"", "1", true},
		{"light", "1", true},
		{"standard", "yes", true}, // other values fall back to the tier
		{"light", "yes", false},
	} {
		t.Setenv("KAI_REVIEW_TIER", tc.tier)
		t.Setenv("KAI_REVIEW_VOTE_BYPASS", tc.sw)
		if got := rcVoteBypassOn(); got != tc.want {
			t.Errorf("tier %q switch %q: on = %v, want %v", tc.tier, tc.sw, got, tc.want)
		}
		res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 3}, []int{1, 2}, "a.go")
		rcPublishGate(res, repo, true)
		_, by := bypassPublished(res)
		if (len(by) == 1) != tc.want {
			t.Errorf("tier %q switch %q: bypassed %v, want on = %v", tc.tier, tc.sw, by, tc.want)
		}
	}
}

func TestVoteBypassOffWhenGateOff(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	t.Setenv("KAI_REVIEW_PUBLISH_GATE", "0")
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 3}, []int{1, 2}, "a.go")
	rcPublishGate(res, repo, true)
	if ids, by := bypassPublished(res); !reflect.DeepEqual(ids, []int{1}) || len(by) != 0 {
		t.Fatalf("gate off: published %v bypassed %v, want [1] / none (nothing changes)", ids, by)
	}
}

func TestVoteBypassAddsToTheFallback(t *testing.T) {
	t.Setenv("KAI_REVIEW_TIER", "standard")
	// 1 is far from the change and the gate withholds it; the fallback keeps it.
	res, repo := bypassFixture(1, []int{1, 2}, map[int]int{2: 3}, []int{2}, "a.go")
	rcPublishGate(res, repo, true)
	ids, by := bypassPublished(res)
	if !reflect.DeepEqual(ids, []int{1, 2}) || !reflect.DeepEqual(by, []int{2}) {
		t.Fatalf("published %v bypassed %v, want [1 2] / [2]: the first is kept by the fallback, the 3-vote cut defect is added", ids, by)
	}
}

// --gate-only restores the cap-cut 3-vote defects of a saved record without
// touching a model provider: with no credentials at all it still answers.
func TestReplayGateOnlyMakesNoProviderCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KAI_REVIEW_TIER", "standard")
	cut := rcSelectionSkipPrefix + "outside the 1 most important defects of this change"
	rec := map[string]any{
		"diff": map[string]any{"files": []map[string]string{{"path": "a.go", "patch": "@@ -1,1 +100,1 @@\n+x"}}},
		"challenge": map[string]any{"allegations": []map[string]any{
			{"id": 1, "issue": "a.go:100 — kept", "status": "supported", "reason": "ok"},
			{"id": 2, "issue": "a.go:100 — cut, 3 votes", "status": "refuted", "reason": cut},
			{"id": 3, "issue": "a.go:100 — dup", "status": "refuted", "duplicateOf": 2, "reason": "Duplicate of issue 2"},
			{"id": 4, "issue": "a.go:100 — dup", "status": "refuted", "duplicateOf": 2, "reason": "Duplicate of issue 2"},
		}},
	}
	raw, _ := json.Marshal(rec)
	path := filepath.Join(t.TempDir(), "rec.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	replayRecord, replayGateOnly = path, true
	defer func() { replayRecord, replayGateOnly = "", false }()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := runReviewReplay(reviewReplayCmd, nil)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatalf("gate-only: %v", err)
	}
	var out rcReplayOut
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Allegations {
		if a.ID == 2 && (a.Status != rcStatusSupported || !a.VoteBypass || a.Votes != 3) {
			t.Errorf("allegation 2 = %+v, want restored by the bypass with 3 votes", a)
		}
	}
}
