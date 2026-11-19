package main

import "testing"

func TestReplayRestoresWhatSelectionAndGateWithheld(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:3 — posted", Status: rcStatusSupported, Rank: "publish", Votes: 2,
			Original: &rcOriginalWording{Issue: "a.go:3 — the check's words"}},
		{ID: 2, Issue: "a.go:90 — far away", Status: rcStatusRefuted, Reason: rcSelectionSkipPrefix + "outside the change: x"},
		{ID: 3, Issue: "a.go:4 — really refuted", Status: rcStatusRefuted, Reason: "the code handles it"},
	}}
	rcReplayRestore(res)
	a := res.Allegations
	if a[0].Issue != "a.go:3 — the check's words" || a[0].Rank != "" || a[0].Votes != 0 {
		t.Errorf("published one not restored: %+v", a[0])
	}
	if a[1].Status != rcStatusSupported || a[1].Reason != "" {
		t.Errorf("withheld one not restored: %+v", a[1])
	}
	if a[2].Status != rcStatusRefuted {
		t.Errorf("a real refutation must stay refuted: %+v", a[2])
	}
}

func TestReplayRebuildsChangedLinesAndTheGateUsesThem(t *testing.T) {
	var rec rcReplayRecord
	rec.Diff.Files = []struct {
		Path  string `json:"path"`
		Patch string `json:"patch"`
	}{{Path: "a.go", Patch: "@@ -1,3 +10,4 @@\n ctx\n+new\n+new2\n ctx2\n"}}
	_, repo := rcReplayDiff(&rec)
	if got := repo.changed["a.go"]; len(got) != 1 || got[0] != [2]int{11, 12} {
		t.Fatalf("changed ranges = %v, want [[11 12]]", got)
	}
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Issue: "a.go:12 — near", Status: rcStatusSupported},
		{ID: 2, Issue: "a.go:90 — far", Status: rcStatusSupported},
	}}
	rcPublishGate(res, repo, false)
	if res.Allegations[0].Status != rcStatusSupported || res.Allegations[1].Status != rcStatusRefuted {
		t.Errorf("gate over the replayed diff: %+v", res.Allegations)
	}
}
