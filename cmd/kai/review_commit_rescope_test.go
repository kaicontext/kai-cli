package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// sinceFrom answers rcChangedSince from a table; a commit missing from it is
// one the clone does not have.
func sinceFrom(t map[string][]string) func(string) ([]string, bool) {
	return func(c string) ([]string, bool) {
		f, ok := t[c]
		return f, ok
	}
}

var rescopePR = []string{"a.go", "b.go", "c.go", "d.go", "e.go"}

func TestReReviewFocus_FilesChangedSinceThePreviousReview(t *testing.T) {
	prior := &rcPrior{Head: "h1"}
	got := rcReReviewFocus(prior, rescopePR, sinceFrom(map[string][]string{"h1": {"c.go", "a.go"}}))
	if want := []string{"a.go", "c.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("focus = %v, want %v", got, want)
	}
}

// A file that changed since but is not in the pull request any more (the
// change was reverted, or it came in from the base) is not read again.
func TestReReviewFocus_OnlyThePullRequestsFiles(t *testing.T) {
	prior := &rcPrior{Head: "h1"}
	got := rcReReviewFocus(prior, rescopePR, sinceFrom(map[string][]string{"h1": {"b.go", "vendor/x.go"}}))
	if want := []string{"b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("focus = %v, want %v", got, want)
	}
}

// An earlier point is re-checked in its own file even when that file did not
// change, and an older open point's fix is looked for since its own commit.
func TestReReviewFocus_EarlierPoints(t *testing.T) {
	prior := &rcPrior{Head: "h2", Points: []rcPriorPoint{
		{Kind: "defect", Ref: "p1", Text: "`d.go:12` — the guard is missing (also: e.go:4)"},
		{Kind: "defect", Ref: "p2", Text: "b.go:3 — off by one", Since: "h1"},
	}}
	got := rcReReviewFocus(prior, rescopePR, sinceFrom(map[string][]string{
		"h2": {"a.go"},
		"h1": {"a.go", "b.go"},
	}))
	if want := []string{"a.go", "b.go", "d.go", "e.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("focus = %v, want %v", got, want)
	}
}

// Every case where the focus cannot be trusted reviews the whole change.
func TestReReviewFocus_WholeChange(t *testing.T) {
	cases := map[string]struct {
		prior *rcPrior
		since map[string][]string
		env   string
	}{
		"first review":        {nil, nil, ""},
		"commit not in clone": {&rcPrior{Head: "gone"}, map[string][]string{}, ""},
		"older point not in clone": {&rcPrior{Head: "h2", Points: []rcPriorPoint{{Ref: "p", Text: "a.go:1 — x", Since: "gone"}}},
			map[string][]string{"h2": {"a.go"}}, ""},
		"nothing changed since": {&rcPrior{Head: "h1"}, map[string][]string{"h1": nil}, ""},
		"everything changed":    {&rcPrior{Head: "h1"}, map[string][]string{"h1": rescopePR}, ""},
		"turned off":            {&rcPrior{Head: "h1"}, map[string][]string{"h1": {"a.go"}}, "off"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KAI_REVIEW_RESCOPE", c.env)
			if got := rcReReviewFocus(c.prior, rescopePR, sinceFrom(c.since)); got != nil {
				t.Fatalf("focus = %v, want nil (the whole change)", got)
			}
		})
	}
}

// The budgets follow the focus: a one-file fix to a big pull request gets the
// base budget, not one sized to every file.
func TestReReviewScopeSizesTheTurnBudget(t *testing.T) {
	changed := make([]string, 20)
	for i := range changed {
		changed[i] = string(rune('a'+i)) + ".go"
	}
	if got, want := rcReviewMaxTurns(len(rcReviewScope(changed, []string{"a.go"}))), rcReviewMaxTurns(1); got != want {
		t.Fatalf("scoped turns = %d, want %d", got, want)
	}
	if got, want := rcReviewMaxTurns(len(rcReviewScope(changed, nil))), rcReviewMaxTurns(20); got != want {
		t.Fatalf("whole-change turns = %d, want %d", got, want)
	}
}

// The prompt states the scoped budget and sends the reviewer from the focus
// into the unchanged files the change reaches; a whole review gets no block.
func TestReReviewFocusBlock(t *testing.T) {
	prior := &rcPrior{Head: "0123456789abcdef"}
	b := rcReReviewFocusBlock(prior, []string{"a.go", "c.go"}, 5, rcReviewMaxTurns(2))
	for _, want := range []string{
		fmt.Sprintf("you have %d turns", rcReviewMaxTurns(2)),
		"2 of 5 changed files", "a.go, c.go", rcShort(prior.Head), "kai_callers",
		"the whole diff is below. Read the focus files first.",
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("block missing %q:\n%s", want, b)
		}
	}
	if rcReReviewFocusBlock(prior, nil, 5, 45) != "" || rcReReviewFocusBlock(nil, []string{"a.go"}, 5, 33) != "" {
		t.Fatal("a whole review got a focus block")
	}
}

func TestOnlyPatchesKeepsTheFocusInDiffOrder(t *testing.T) {
	order := []string{"c.go", "a.go", "b.go"}
	patches := map[string]string{"a.go": "A", "b.go": "B", "c.go": "C"}
	o, p := rcOnlyPatches(order, patches, []string{"a.go", "c.go"})
	if !reflect.DeepEqual(o, []string{"c.go", "a.go"}) || !reflect.DeepEqual(p, map[string]string{"a.go": "A", "c.go": "C"}) {
		t.Fatalf("got %v %v", o, p)
	}
	if o, p := rcOnlyPatches(order, patches, nil); !reflect.DeepEqual(o, order) || len(p) != 3 {
		t.Fatalf("nil focus changed the patches: %v %v", o, p)
	}
}
