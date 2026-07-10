package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/drift"
)

func TestDriftStatusLines(t *testing.T) {
	head := strings.Repeat("b", 40)
	graph := strings.Repeat("a", 40)
	cases := []struct {
		name     string
		rep      drift.Report
		want     []string // substrings that must appear, in order across lines
		wantHint bool
	}{
		{
			name: "synced",
			rep:  drift.Report{Relationship: drift.RelSynced, GitHead: head, GraphState: head},
			want: []string{"in sync"},
		},
		{
			name: "behind",
			rep: drift.Report{
				Relationship: drift.RelBehind, GitHead: head, GraphState: graph,
				Behind:                3,
				OldestUnprocessedUnix: time.Now().Add(-2 * time.Hour).Unix(),
			},
			want:     []string{"3 commits behind", "oldest unprocessed 2h ago"},
			wantHint: true,
		},
		{
			name: "ahead",
			rep:  drift.Report{Relationship: drift.RelAhead, GitHead: head, GraphState: graph, Ahead: 1},
			want: []string{"1 commit ahead", "older commit"},
		},
		{
			name: "diverged",
			rep: drift.Report{
				Relationship: drift.RelDiverged, GitHead: head, GraphState: graph,
				Behind: 2, Ahead: 1,
			},
			want:     []string{"diverged", "2 commits unprocessed", "1 commit only in graph"},
			wantHint: true,
		},
		{
			name:     "orphaned",
			rep:      drift.Report{Relationship: drift.RelOrphaned, GitHead: head, GraphState: graph},
			want:     []string{"no history", "history rewritten"},
			wantHint: true,
		},
		{
			name: "unpinned",
			rep:  drift.Report{Relationship: drift.RelUnpinned, GitHead: head},
			want: []string{"not yet pinned"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := driftStatusLines(&tc.rep)
			if len(lines) == 0 {
				t.Fatalf("no output for %s", tc.name)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("output missing %q:\n%s", want, joined)
				}
			}
			hasHint := len(lines) > 1
			if hasHint != tc.wantHint {
				t.Errorf("hint line = %v, want %v:\n%s", hasHint, tc.wantHint, joined)
			}
			// Every relationship line names the state compactly; the raw
			// 40-char SHAs must never leak into human output.
			if strings.Contains(joined, head) || strings.Contains(joined, graph) {
				t.Errorf("full SHA leaked into human output:\n%s", joined)
			}
		})
	}
}

func TestAgeString(t *testing.T) {
	now := time.Now()
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-72 * time.Hour), "3d ago"},
	}
	for _, tc := range cases {
		if got := ageString(tc.at.Unix()); got != tc.want {
			t.Errorf("ageString(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
}
