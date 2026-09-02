package main

import (
	"strings"
	"testing"
)

func TestFormatChangeReviewTable(t *testing.T) {
	out := formatChangeReviewTable([]serverChangeReview{
		{ID: "0123456789abcdef", State: "open", PRNumber: 7, Title: "Add MFA support to the login flow for everyone", Branch: "kai/s-abc"},
		{ID: "fedcba", State: "merged", Branch: "kai/s-def"},
	})
	for _, want := range []string{"0123456789ab", "open", "#7", "Add MFA support to the logi...", "kai/s-abc", "fedcba", "merged", "(untitled)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0123456789abc") {
		t.Errorf("id not shortened:\n%s", out)
	}
}

func TestFormatChangeReviewDetail(t *testing.T) {
	d := &serverChangeDetail{
		serverChangeReview: serverChangeReview{ID: "cr-1", State: "changes_requested", Mode: "github", Branch: "kai/s-abc",
			BaseGitSHA: "aaaaaaaaaaaaaaaa", HeadSHA: "bbbbbbbbbbbbbbbb", PRNumber: 7, PRURL: "https://github.com/o/r/pull/7"},
		Events: []serverChangeEvent{
			{Kind: "shipped", Body: "shipped bbbbbbbbbbbb to PR #7", Actor: "u-1", CreatedAt: 1},
			{Kind: "state", FromState: "open", ToState: "changes_requested", Actor: "octocat", Source: "github", CreatedAt: 2},
		},
		Findings: []serverChangeFinding{{ID: "rc-1", Title: "Hollow test", Verdict: "awaiting", Risk: 2}},
	}
	out := formatChangeReviewDetail(d)
	for _, want := range []string{"kai/s-abc", "state:   changes_requested", "aaaaaaaaaaaa → bbbbbbbbbbbb", "#7 https://github.com/o/r/pull/7",
		"Hollow test", "awaiting", "open → changes_requested  (octocat via GitHub)", "shipped bbbbbbbbbbbb to PR #7  (u-1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}

func TestFormatVerbOutcome(t *testing.T) {
	out := &serverVerbResponse{Review: serverChangeReview{ID: "0123456789abcdef", State: "approved"}, Mirrored: true}
	if got := formatVerbOutcome("approve", out); got != "Review 0123456789ab: approved (state approved) — mirrored to GitHub\n" {
		t.Errorf("mirrored = %q", got)
	}
	land := &serverVerbResponse{Review: serverChangeReview{ID: "cr", State: "merged"}, LandSHA: "abcdef0123456789", Base: "main", Rebased: true}
	if got := formatVerbOutcome("land", land); !strings.Contains(got, "landed") || !strings.Contains(got, "main is now abcdef012345") || !strings.Contains(got, "replayed") {
		t.Errorf("land = %q", got)
	}
	out = &serverVerbResponse{Review: serverChangeReview{ID: "cr", State: "changes_requested"}, MirrorError: "org/app is not GitHub-linked here"}
	if got := formatVerbOutcome("request_changes", out); !strings.Contains(got, "changes requested") || !strings.Contains(got, "GitHub was not updated: org/app") {
		t.Errorf("unmirrored = %q", got)
	}
}
