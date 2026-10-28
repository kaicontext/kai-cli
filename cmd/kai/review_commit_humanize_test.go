package main

import "testing"

func TestHumanizeAppliesOnlyCompleteRewordings(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Status: rcStatusSupported, Finding: "f1"},
		{ID: 2, Status: rcStatusSupported, Finding: "f2"},
		{ID: 3, Status: rcStatusSupported, Finding: "f3"},
		{ID: 4, Status: rcStatusRefuted, Finding: "f4"},
	}}
	got, err := rcParseHumanize(`Sure: {"findings": [
		{"id": 1, "title": "Relayed events lose UnitsChanged.", "plain": "Plain.", "when": "When.", "impact": "Impact.", "fix": "Fix."},
		{"id": 2, "title": "", "plain": "No title."},
		{"id": 3, "title": "A title far too long to be a title because it keeps going and going well past ninety characters", "plain": "x"},
		{"id": 4, "title": "Refuted", "plain": "x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := rcApplyHumanize(res, []int{0, 1, 2}, got); n != 1 {
		t.Fatalf("applied %d, want 1", n)
	}
	a := res.Allegations[0]
	if a.Title != "Relayed events lose UnitsChanged" || a.Plain != "Plain." || a.When != "When." || a.Impact != "Impact." || a.Fix != "Fix." {
		t.Errorf("rewording not applied as written: %+v", a)
	}
	for _, i := range []int{1, 2, 3} {
		if b := res.Allegations[i]; b.Title != "" || b.Plain != "" {
			t.Errorf("allegation %d got a rewording it should not have: %+v", b.ID, b)
		}
	}
	for i, a := range res.Allegations {
		if want := []string{rcStatusSupported, rcStatusSupported, rcStatusSupported, rcStatusRefuted}[i]; a.Status != want {
			t.Errorf("humanize changed a status: %+v", a)
		}
	}
}

// After the selection reorders, ids run out of order; answers are numbered by
// position, so each title lands on the finding it was written for.
func TestHumanizeMapsAnswersByPosition(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{ID: 1, Status: rcStatusSupported, Finding: "f1"},
		{ID: 3, Status: rcStatusSupported, Finding: "f3"},
		{ID: 2, Status: rcStatusSupported, Finding: "f2"},
	}}
	got := map[int]rcHumanized{1: {Title: "First", Plain: "p"}, 2: {Title: "Second", Plain: "p"}, 3: {Title: "Third", Plain: "p"}}
	rcApplyHumanize(res, []int{0, 1, 2}, got)
	for i, want := range []string{"First", "Second", "Third"} {
		if res.Allegations[i].Title != want {
			t.Errorf("position %d (id %d) titled %q, want %q", i, res.Allegations[i].ID, res.Allegations[i].Title, want)
		}
	}
}

func TestHumanizeRefusesAnAnswerWithoutJSON(t *testing.T) {
	if _, err := rcParseHumanize("I could not do that."); err == nil {
		t.Error("an answer without JSON was accepted")
	}
}
