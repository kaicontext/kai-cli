package main

import (
	"encoding/json"
	"testing"
)

// TestFindingSummaryParse guards the struct-tag contract with the control-plane
// findings API: the list endpoint returns {"findings":[…]} with these keys.
func TestFindingSummaryParse(t *testing.T) {
	body := `{"findings":[{"id":"rc-1","pr_number":38,"title":"t","author":"a","verdict":"awaiting","intent_match":"partial","reaches":3,"claims":4,"risk":2}]}`
	var resp struct {
		Findings []findingSummary `json:"findings"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(resp.Findings))
	}
	f := resp.Findings[0]
	if f.ID != "rc-1" || f.PRNumber != 38 || f.Claims != 4 || f.Risk != 2 || f.Verdict != "awaiting" || f.IntentMatch != "partial" || f.Reaches != 3 {
		t.Fatalf("bad parse: %+v", f)
	}
}

func TestFindingsTruncate(t *testing.T) {
	cases := []struct{ in, want string; n int }{
		{"hello", "hello", 10},
		{"hello world", "hell…", 5},
		{"  spaced  ", "spaced", 10},
	}
	for _, c := range cases {
		if got := findingsTruncate(c.in, c.n); got != c.want {
			t.Errorf("findingsTruncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestFindingsVerdictLabel(t *testing.T) {
	if got := findingsVerdictLabel(""); got != "awaiting" {
		t.Errorf("empty verdict = %q, want awaiting", got)
	}
	if got := findingsVerdictLabel("confirmed"); got != "confirmed" {
		t.Errorf("confirmed = %q", got)
	}
}
