package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// These cover the gate losses traced on the 2026-09-28 run 5 benchmark:
// drafted defects lost to a failed batch, to refutations that contradicted
// themselves or the diff, and to evidence the gate could not read.

func rcSubmit(t *testing.T, answer rcChallengeAnswer) provider.Response {
	t.Helper()
	return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "s", Name: "submit_review", Input: rcTestAnswer(t, answer)}}}
}

// rcAnswerFor answers only the allegations the request asks about.
func rcAnswerFor(req provider.Request) rcChallengeAnswer {
	text := rcRequestText(req)
	all := rcCDChecks()
	out := rcChallengeAnswer{Scope: all.Scope, IntentMatch: all.IntentMatch, MergeReady: all.MergeReady, Decisions: []rcDecisionCheck{}}
	for _, c := range all.Checks {
		if strings.Contains(text, "- "+c.Issue+"\n") {
			out.Checks = append(out.Checks, c)
		}
	}
	return out
}

func TestFailedCheckIsRetriedOneAllegationAtATime(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return provider.Response{}, context.DeadlineExceeded
		}
		return rcSubmit(t, rcAnswerFor(req)), nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(rcCDIssues...), rcCDSources, nil)
	if err != nil {
		t.Fatalf("a failed check withheld the review instead of retrying: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 1 failed + 2 single-allegation retries", calls)
	}
	statuses := map[string]string{}
	for _, a := range got.Allegations {
		statuses[a.Issue] = a.Status
	}
	if statuses[rcFalseCDIssue] != rcStatusRefuted || statuses[rcEscapeIssue] != rcStatusSupported {
		t.Fatalf("retried verdicts were not kept: %+v", statuses)
	}
}

func TestFailedBatchIsRetriedAndAnUncheckedSweepProposalIsNotRefuted(t *testing.T) {
	defer func(n int) { rcChallengeBatchSize = n }(rcChallengeBatchSize)
	rcChallengeBatchSize = 1
	issues := []string{rcFalseCDIssue, rcEscapeIssue, "frontend/dist/panel-terminal.js:150 — a third defect nobody can check"}
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		if strings.Contains(rcRequestText(req), "a third defect") {
			return provider.Response{}, context.DeadlineExceeded
		}
		return rcSubmit(t, rcAnswerFor(req)), nil
	}}
	got, err := rcChallengeReview(context.Background(), p, "test", rcTestReview(issues...), rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := rcWithholdUnsettledSweep(got, issues[2:]); n != 0 {
		t.Fatalf("an allegation whose check never ran was withheld as refuted (%d)", n)
	}
	for _, a := range got.Allegations {
		if strings.Contains(a.Issue, "a third defect") && a.Status != rcStatusUnresolved {
			t.Fatalf("unchecked allegation status = %s, want unresolved", a.Status)
		}
	}
	if !strings.Contains(got.Review, "a third defect") {
		t.Fatal("the unchecked allegation is missing from Could not verify")
	}
}

func TestSettledButUnresolvedSweepProposalIsStillWithheld(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{{Issue: rcFalseCDIssue, Status: rcStatusUnresolved, Reason: "sources do not show it"}}}
	if n := rcWithholdUnsettledSweep(res, []string{rcFalseCDIssue}); n != 1 {
		t.Fatalf("withheld %d, want 1", n)
	}
}

func rcSelfContradicting() rcChallengeAnswer {
	a := rcCDChecks()
	a.Checks[0].Reason = "The cd does persist. This IS a defect. The allegation is correct."
	return a
}

func TestContradictoryRefutationIsSentBackThenDemoted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second func(*testing.T) rcChallengeAnswer
		want   string
	}{
		{"corrected-to-supported", func(t *testing.T) rcChallengeAnswer {
			a := rcCDChecks()
			a.Checks[0].Verdict, a.Checks[0].Finding = "supported", "later lines run outside the workspace"
			a.Checks[0].Reason = "The cd persists for later lines."
			return a
		}, rcStatusSupported},
		{"still-contradicting", func(*testing.T) rcChallengeAnswer { return rcSelfContradicting() }, rcStatusUnresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var feedback string
			p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
				calls++
				if calls == 1 {
					return rcSubmit(t, rcSelfContradicting()), nil
				}
				last := req.Messages[len(req.Messages)-1].Parts[0]
				if tr, ok := last.(message.ToolResult); ok {
					feedback = tr.Content
				}
				return rcSubmit(t, tc.second(t)), nil
			}}
			got, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcCDIssues...), rcCDSources, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !strings.Contains(feedback, "says the allegation is correct") {
				t.Fatalf("the contradiction was not sent back (calls %d): %s", calls, feedback)
			}
			if got.Allegations[0].Status != tc.want {
				t.Fatalf("status = %s, want %s", got.Allegations[0].Status, tc.want)
			}
		})
	}
}

func TestNegatedReasonIsNotAContradiction(t *testing.T) {
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{Issue: rcFalseCDIssue, Status: rcStatusRefuted, Reason: "This is not a defect: the cd applies to both lines."},
		{Issue: rcEscapeIssue, Status: rcStatusRefuted, Reason: "It would be a defect if the path were user-controlled; it is not."},
	}}
	if p := rcVerdictProblems(res, nil); len(p) != 0 {
		t.Fatalf("negated reasons flagged: %+v", p)
	}
}

func TestPreexistingRefutationNearTheChangeIsFlagged(t *testing.T) {
	repo := &rcRepo{changed: rcChangedRanges("+++ b/pkg/web/webassets.go\n@@ -70,3 +70,9 @@ func x\n")}
	res := &rcChallengeResult{Allegations: []rcAllegationResult{
		{Issue: "pkg/web/webassets.go:82 — a failed fetch stores nil over the cached value", Status: rcStatusRefuted, Reason: "Unchanged context: this is pre-existing, not this change's defect."},
		{Issue: "pkg/web/other.go:82 — something in a file the change never touched", Status: rcStatusRefuted, Reason: "Pre-existing code."},
		{Issue: "pkg/web/webassets.go:400 — far from the change", Status: rcStatusRefuted, Reason: "Pre-existing code."},
	}}
	p := rcVerdictProblems(res, repo)
	if len(p) != 1 || p[0].Index != 0 || !strings.Contains(p[0].Text, "70-78") {
		t.Fatalf("problems = %+v, want only the allegation beside lines 70-78", p)
	}
}

func TestChangedRanges(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n-x\n+y\n+z\n@@ -10,2 +11,0 @@\n-gone\n-gone\n--- a/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"
	got := rcChangedRanges(diff)
	if len(got["a.go"]) != 2 || got["a.go"][0] != [2]int{1, 2} || got["a.go"][1] != [2]int{11, 11} || len(got) != 1 {
		t.Fatalf("ranges = %v", got)
	}
}

// rcTempRepo is a one-commit repository; it returns the commit hash and
// switches the working directory into it for the test.
func rcTempRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	t.Chdir(dir)
	return run("rev-parse", "HEAD")
}

func TestRepoViewIsCitableByFileLines(t *testing.T) {
	hash := rcTempRepo(t, map[string]string{"pkg/a.go": "one\ntwo\nthree\nfour\n"})
	repo := &rcRepo{hash: hash}
	input := `{"file_path":"pkg/a.go","offset":1,"limit":2}`
	out, err := repo.view(input)
	if err != nil {
		t.Fatal(err)
	}
	src := rcToolSource("kai_view", input, out)
	if src.Coord != rcCoordFile || src.First != 2 || len(src.Rows) != 2 || src.Rows[1] != "three" {
		t.Fatalf("view is not file-addressed: %+v\n%s", src, out)
	}
	if !strings.Contains(out, "call again with offset 3") {
		t.Fatalf("truncation not stated: %s", out)
	}
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "-p", ""} {
		if _, err := repo.view(`{"file_path":"` + bad + `"}`); err == nil {
			t.Errorf("path %q was accepted", bad)
		}
	}
}

func TestRepoGrepSearchesTheCommit(t *testing.T) {
	hash := rcTempRepo(t, map[string]string{"pkg/a.go": "func Target() {}\n", "pkg/b.go": "Target()\n"})
	repo := &rcRepo{hash: hash}
	out, err := repo.grep(`{"query":"Target(","path":"pkg"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, hash) || !strings.Contains(out, "pkg/a.go:1:func Target() {}") || !strings.Contains(out, "pkg/b.go:1:Target()") {
		t.Fatalf("grep output = %q", out)
	}
	if out, _ := repo.grep(`{"query":"nothing-matches-this"}`); !strings.Contains(out, "no matches") {
		t.Fatalf("empty grep = %q", out)
	}
}

// A lookup's result becomes a numbered source the submission can cite.
func TestGateLookupBecomesACitableSource(t *testing.T) {
	hash := rcTempRepo(t, map[string]string{"frontend/dist/panel-terminal.js": "cd \"$WS\"\nrm -rf build\n"})
	ctx := rcWithRepo(context.Background(), &rcRepo{hash: hash, changed: map[string][][2]int{}})
	calls := 0
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		calls++
		switch calls {
		case 1:
			names := map[string]bool{}
			for _, tool := range req.Tools {
				names[tool.Name] = true
			}
			if !names["kai_view"] || !names["kai_grep"] || !strings.Contains(req.System, "read the repository at the reviewed commit") {
				t.Fatalf("lookup tools not offered: %v", names)
			}
			return provider.Response{Parts: []message.ContentPart{message.ToolCall{ID: "v", Name: "kai_view", Input: `{"file_path":"frontend/dist/panel-terminal.js"}`}}}, nil
		case 2:
			a := rcCDChecks()
			// Source 3 is the lookup: file line 2 of the viewed file.
			a.Checks[1].Evidence = []rcCheckEvidence{{Source: 3, LineStart: 2, LineEnd: 2}}
			return rcSubmit(t, a), nil
		}
		t.Fatal("unexpected call")
		return provider.Response{}, errors.New("unreachable")
	}}
	got, err := rcChallengeDraft(ctx, p, "test", rcTestReview(rcCDIssues...), rcCDSources, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := got.Allegations[1].Evidence
	if got.Allegations[1].Status != rcStatusSupported || len(ev) != 1 || ev[0].Path != "frontend/dist/panel-terminal.js" || ev[0].Coord != rcCoordFile {
		t.Fatalf("lookup evidence not accepted: %+v", got.Allegations[1])
	}
}

// Without a repository (the fast pass) the gate offers no lookups.
func TestNoLookupsWithoutARepo(t *testing.T) {
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		for _, tool := range req.Tools {
			if tool.Name == "kai_view" || tool.Name == "kai_grep" {
				t.Fatal("lookup tools offered without a repository")
			}
		}
		return rcSubmit(t, rcCDChecks()), nil
	}}
	if _, err := rcChallengeDraft(context.Background(), p, "test", rcTestReview(rcCDIssues...), rcCDSources, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReasoningEffortSetting(t *testing.T) {
	for in, want := range map[string]string{"": "", "LOW": "low", " medium ": "medium", "high": "high", "extreme": "", "minimal": "minimal"} {
		t.Setenv("KAI_REVIEW_REASONING_EFFORT", in)
		if got := rcReasoningEffort(); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// The grounded gate thinks when asked; the fast pass's gate (no repository on
// its context) never does.
func TestGateEffortOnlyOnTheGroundedPath(t *testing.T) {
	t.Setenv("KAI_REVIEW_REASONING_EFFORT", "medium")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"grounded", rcWithRepo(context.Background(), &rcRepo{}), "medium"},
		{"fast", context.Background(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
				if req.ReasoningEffort != tc.want {
					t.Errorf("effort = %q, want %q", req.ReasoningEffort, tc.want)
				}
				return rcSubmit(t, rcCDChecks()), nil
			}}
			if _, err := rcChallengeDraft(tc.ctx, p, "test", rcTestReview(rcCDIssues...), rcCDSources, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
