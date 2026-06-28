package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/safetygate"
)

func TestFilterChangedForProject(t *testing.T) {
	in := []string{
		"kai/kai-cli/internal/foo.go",
		"kai/kai-cli/internal/bar.go",
		"kai-server/kailab/api/routes.go",
		"kai-server/kailab-control/internal/api/search.go",
		"kai-e2e/fixtures/x.json",
	}
	cases := []struct {
		name    string
		project string
		want    []string
	}{
		{
			name:    "primary kai",
			project: "kai",
			want: []string{
				"kai-cli/internal/foo.go",
				"kai-cli/internal/bar.go",
			},
		},
		{
			name:    "secondary kai-server",
			project: "kai-server",
			want: []string{
				"kailab/api/routes.go",
				"kailab-control/internal/api/search.go",
			},
		},
		{
			name:    "untouched project returns empty",
			project: "kai-tui",
			want:    nil,
		},
		{
			name:    "empty project name is a passthrough (single-root shape)",
			project: "",
			want:    in,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterChangedForProject(in, c.project)
			if len(got) != len(c.want) {
				t.Fatalf("len mismatch: got %d, want %d (got=%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestEscalates(t *testing.T) {
	cases := []struct {
		newV, currentV safetygate.Verdict
		want           bool
	}{
		{safetygate.Auto, safetygate.Auto, false},
		{safetygate.Review, safetygate.Auto, true},
		{safetygate.Block, safetygate.Auto, true},
		{safetygate.Block, safetygate.Review, true},
		{safetygate.Auto, safetygate.Review, false},
		{safetygate.Auto, safetygate.Block, false},
		{safetygate.Review, safetygate.Block, false},
		{safetygate.Review, safetygate.Review, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s_over_%s", c.newV, c.currentV), func(t *testing.T) {
			if got := escalates(c.newV, c.currentV); got != c.want {
				t.Errorf("escalates(%v, %v) = %v, want %v", c.newV, c.currentV, got, c.want)
			}
		})
	}
}

func TestAggregateVerdicts_WorstOfN(t *testing.T) {
	cases := []struct {
		name        string
		states      []projectState
		wantVerdict safetygate.Verdict
		wantBlast   int
	}{
		{
			name: "all-Auto stays Auto",
			states: []projectState{
				{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 2}},
				{target: absorbTarget{name: "kai-server"}, changed: []string{"b.go"}, newLatest: []byte{2}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 1}},
			},
			wantVerdict: safetygate.Auto,
			wantBlast:   3,
		},
		{
			name: "any Review escalates aggregate",
			states: []projectState{
				{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 2}},
				{target: absorbTarget{name: "kai-server"}, changed: []string{"b.go"}, newLatest: []byte{2}, verdict: safetygate.Decision{Verdict: safetygate.Review, BlastRadius: 5}},
			},
			wantVerdict: safetygate.Review,
			wantBlast:   7,
		},
		{
			name: "any Block wins regardless of order",
			states: []projectState{
				{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1}, verdict: safetygate.Decision{Verdict: safetygate.Block, BlastRadius: 9}},
				{target: absorbTarget{name: "kai-server"}, changed: []string{"b.go"}, newLatest: []byte{2}, verdict: safetygate.Decision{Verdict: safetygate.Review, BlastRadius: 5}},
				{target: absorbTarget{name: "kai-e2e"}, changed: []string{"c.go"}, newLatest: []byte{3}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 1}},
			},
			wantVerdict: safetygate.Block,
			wantBlast:   15,
		},
		{
			name: "skipped+empty states contribute nothing",
			states: []projectState{
				{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 2}},
				{target: absorbTarget{name: "kai-tui"}, gateSkipped: true}, // uninitialized, no DB
				{target: absorbTarget{name: "kai-e2e"}, changed: nil},      // touched but no net changes
			},
			wantVerdict: safetygate.Auto,
			wantBlast:   2,
		},
		{
			name: "captureFailed forces at least Review and names the project",
			states: []projectState{
				{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1}, verdict: safetygate.Decision{Verdict: safetygate.Auto, BlastRadius: 2}},
				{target: absorbTarget{name: "kai-server"}, captureFailed: true},
			},
			wantVerdict: safetygate.Review,
			wantBlast:   2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := aggregateVerdicts(c.states)
			if got.Verdict != c.wantVerdict {
				t.Errorf("verdict: got %v, want %v (reasons=%v)", got.Verdict, c.wantVerdict, got.Reasons)
			}
			if got.BlastRadius != c.wantBlast {
				t.Errorf("blast: got %d, want %d", got.BlastRadius, c.wantBlast)
			}
		})
	}
}

func TestAggregateVerdicts_ReasonsPrefixedByProject(t *testing.T) {
	// Reasons from each project's verdict should be prefixed with
	// "[<project>]" in the aggregate so the user can tell which
	// project flagged what.
	states := []projectState{
		{target: absorbTarget{name: "kai"}, changed: []string{"a.go"}, newLatest: []byte{1},
			verdict: safetygate.Decision{Verdict: safetygate.Review, Reasons: []string{"high blast"}}},
		{target: absorbTarget{name: "kai-server"}, changed: []string{"b.go"}, newLatest: []byte{2},
			verdict: safetygate.Decision{Verdict: safetygate.Review, Reasons: []string{"protected path"}}},
	}
	got := aggregateVerdicts(states)
	joined := strings.Join(got.Reasons, " | ")
	for _, want := range []string{"[kai] high blast", "[kai-server] protected path"} {
		if !strings.Contains(joined, want) {
			t.Errorf("aggregate reasons missing %q; full: %s", want, joined)
		}
	}
}

func TestAggregateVerdicts_TouchesPrefixed(t *testing.T) {
	// classifier-returned touches are project-relative; aggregate
	// should re-prefix them with the project name so cross-project
	// touch lists are unambiguous.
	states := []projectState{
		{target: absorbTarget{name: "kai"}, changed: []string{"x.go"}, newLatest: []byte{1},
			verdict: safetygate.Decision{Verdict: safetygate.Auto, Touches: []string{"kai-cli/x.go"}}},
		{target: absorbTarget{name: "kai-server"}, changed: []string{"y.go"}, newLatest: []byte{2},
			verdict: safetygate.Decision{Verdict: safetygate.Auto, Touches: []string{"kailab/y.go"}}},
	}
	got := aggregateVerdicts(states)
	wantTouches := map[string]bool{
		"kai/kai-cli/x.go":     false,
		"kai-server/kailab/y.go": false,
	}
	for _, t := range got.Touches {
		if _, ok := wantTouches[t]; ok {
			wantTouches[t] = true
		}
	}
	for path, seen := range wantTouches {
		if !seen {
			tt := t
			tt.Errorf("aggregate touches missing %q; got: %v", path, got.Touches)
		}
	}
}
