package autofix

import (
	"strings"
	"testing"

	"kai/internal/contract"
)

func boolp(b bool) *bool { return &b }

// TestDecideGate locks down the publish gate: Ready requires all three
// signals positive, and any doubt — especially semantic "unsure" — keeps
// it a draft. This is the property the whole headless loop hinges on.
func TestDecideGate(t *testing.T) {
	yes := contract.SemanticResult{Matches: boolp(true)}
	no := contract.SemanticResult{Matches: boolp(false)}
	unsure := contract.SemanticResult{Matches: nil}
	approved := ReviewResult{Approved: true}
	blocked := ReviewResult{Approved: false, Blocking: []string{"nil deref"}}

	cases := []struct {
		name      string
		det       contract.Verdict
		sem       contract.SemanticResult
		rev       ReviewResult
		wantReady bool
	}{
		{"all green", contract.VerdictCleanUnconfirmed, yes, approved, true},
		{"verified det still ready", contract.VerdictVerified, yes, approved, true},
		{"tests broken blocks", contract.VerdictBroken, yes, approved, false},
		{"semantic unsure blocks", contract.VerdictCleanUnconfirmed, unsure, approved, false},
		{"semantic no blocks", contract.VerdictCleanUnconfirmed, no, approved, false},
		{"review blocking blocks", contract.VerdictCleanUnconfirmed, yes, blocked, false},
		{"no_intent + green sem/rev still ready", contract.VerdictNoIntent, yes, approved, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.det, c.sem, c.rev)
			if got.Ready != c.wantReady {
				t.Fatalf("Ready = %v, want %v (reasons: %v)", got.Ready, c.wantReady, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Fatalf("expected reasons to be populated")
			}
		})
	}
}

func TestParseReview(t *testing.T) {
	approve := "VERDICT: approve\nNOTES: looks correct\nBLOCKING:\n- none"
	if r := ParseReview(approve); !r.Approved || len(r.Blocking) != 0 {
		t.Fatalf("approve case: %+v", r)
	}

	reject := "VERDICT: request-changes\nNOTES: see below\nBLOCKING:\n- off-by-one in loop\n- missing nil check"
	r := ParseReview(reject)
	if r.Approved {
		t.Fatalf("expected not approved")
	}
	if len(r.Blocking) != 2 {
		t.Fatalf("expected 2 blocking, got %d: %v", len(r.Blocking), r.Blocking)
	}

	// A blocking finding overrides a stray "approve".
	contradictory := "VERDICT: approve\nBLOCKING:\n- actually this is broken"
	if ParseReview(contradictory).Approved {
		t.Fatalf("blocking finding must override approve")
	}
}

func TestRepoSlugFromRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:kaicontext/kai-cli.git":   "kaicontext/kai-cli",
		"https://github.com/kaicontext/kai-cli":   "kaicontext/kai-cli",
		"https://github.com/kaicontext/kai-cli.git": "kaicontext/kai-cli",
		"git@gitlab.com:foo/bar.git":               "",
	}
	for url, want := range cases {
		if got := RepoSlugFromRemote(url); got != want {
			t.Errorf("RepoSlugFromRemote(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestRenderPRBodyDraftShowsWhy(t *testing.T) {
	e := Evidence{
		IssueNumber: 7,
		IssueTitle:  "Crash on empty input",
		Branch:      "kai/issue-7",
		Model:       "claude-sonnet-4-6",
		FilesChanged: []string{"parser.go"},
		DetVerdict:  contract.VerdictCleanUnconfirmed,
		Semantic:    contract.SemanticResult{Matches: nil, Note: "change not visible in diff"},
		Residue:     []string{"Does this handle the nil slice case too?"},
		Review:      ReviewResult{Approved: true},
		Decision:    Decide(contract.VerdictCleanUnconfirmed, contract.SemanticResult{Matches: nil}, ReviewResult{Approved: true}),
	}
	body := RenderPRBody(e)
	if !strings.Contains(body, "draft") {
		t.Errorf("draft PR body should say draft:\n%s", body)
	}
	if !strings.Contains(body, "Closes #7") {
		t.Errorf("body should close the issue")
	}
	if !strings.Contains(body, "Open questions") || !strings.Contains(body, "nil slice") {
		t.Errorf("draft body should surface residue questions:\n%s", body)
	}
	if !strings.Contains(body, marker) {
		t.Errorf("body should carry the autofix marker")
	}
}

func TestBranchNameStable(t *testing.T) {
	if BranchName(42) != "kai/issue-42" {
		t.Fatalf("unexpected branch name %q", BranchName(42))
	}
}
