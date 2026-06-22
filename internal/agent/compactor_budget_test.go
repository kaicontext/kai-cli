package agent

import "testing"

func TestShouldCompactByBudget(t *testing.T) {
	cases := []struct {
		name  string
		used  int
		cap   int
		want  bool
	}{
		{"no cap configured — opt-in only", 100_000, 0, false},
		{"zero used", 0, 200_000, false},
		{"under threshold — 50% of 200k", 100_000, 200_000, false},
		{"exactly at threshold — 70% of 200k", 140_000, 200_000, true},
		{"over threshold — 80% of 200k", 160_000, 200_000, true},
		{"already over cap", 250_000, 200_000, true},
		// Smaller cap (e.g. cacheless planner): same ratio behavior.
		{"50k cacheless cap, under", 30_000, 50_000, false},
		{"50k cacheless cap, at 70%", 35_000, 50_000, true},
	}
	for _, c := range cases {
		if got := shouldCompactByBudget(c.used, c.cap); got != c.want {
			t.Errorf("%s: shouldCompactByBudget(%d, %d) = %v, want %v",
				c.name, c.used, c.cap, got, c.want)
		}
	}
}
