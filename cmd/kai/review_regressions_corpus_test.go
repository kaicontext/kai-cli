package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// The regression corpus is read by scripts/review-regressions.sh, not by Go,
// so nothing else would notice a case that can never be graded: a truncated
// SHA, an entry with no phrases, or an at_most with no bound.
func TestReviewRegressionCorpusIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile("testdata/review-regressions/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Issue, File, Golden, Was string
		Any                      []string
		Max                      *int
	}
	var corpus struct {
		Cases []struct {
			ID, Upstream, Repo, Base, Head string
			RawExpect                      []entry `json:"expect"`
			RawForbid                      []entry `json:"forbid"`
			RawAtMost                      []entry `json:"at_most"`
		}
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("no cases")
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.ID == "" || seen[c.ID] {
			t.Errorf("case id %q missing or repeated", c.ID)
		}
		seen[c.ID] = true
		if !sha.MatchString(c.Base) || !sha.MatchString(c.Head) {
			t.Errorf("%s: base/head must be full SHAs, got %q/%q", c.ID, c.Base, c.Head)
		}
		if c.Repo == "" || c.Upstream == "" {
			t.Errorf("%s: repo and upstream are required", c.ID)
		}
		if len(c.RawExpect)+len(c.RawForbid)+len(c.RawAtMost) == 0 {
			t.Errorf("%s: grades nothing", c.ID)
		}
		for _, e := range append(append(c.RawExpect, c.RawForbid...), c.RawAtMost...) {
			if e.Issue == "" || len(e.Any) == 0 {
				t.Errorf("%s: entry %+v needs an issue and at least one phrase", c.ID, e)
			}
		}
		for _, e := range c.RawExpect {
			if e.Golden == "" {
				t.Errorf("%s: expect %s has no golden comment", c.ID, e.Issue)
			}
		}
		for _, e := range c.RawForbid {
			if e.Was == "" {
				t.Errorf("%s: forbid %s does not say what was published", c.ID, e.Issue)
			}
		}
		for _, e := range c.RawAtMost {
			if e.Max == nil {
				t.Errorf("%s: at_most %s has no max", c.ID, e.Issue)
			}
		}
	}
}
