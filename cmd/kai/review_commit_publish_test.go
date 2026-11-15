package main

import (
	"fmt"
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
}
