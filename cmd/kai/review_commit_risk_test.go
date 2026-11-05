package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

func rcRiskFiles(paths ...string) []finding.DiffFile {
	out := make([]finding.DiffFile, 0, len(paths))
	for _, p := range paths {
		out = append(out, finding.DiffFile{Path: p, Added: 10, Removed: 2})
	}
	return out
}

func rcRiskDirect(n int) finding.Blast {
	var b finding.Blast
	for i := 0; i < n; i++ {
		b.Nodes = append(b.Nodes, finding.BlastNode{File: "dep", Hops: 1})
	}
	return b
}

// The rating matches kai-server's changeRisk on what it can read before a
// review: these are the server's own cases, minus the review's decisions.
func TestPreReviewRisk(t *testing.T) {
	cases := []struct {
		name  string
		files []finding.DiffFile
		blast finding.Blast
		want  string
	}{
		{"no files", nil, finding.Blast{}, ""},
		{"plain change", rcRiskFiles("app/assets/stylesheets/header.scss", "lib/topic.rb"), finding.Blast{}, "S"},
		{"tests and docs only", rcRiskFiles("pkg/auth/login_test.go", "docs/billing.md"), finding.Blast{}, "S"},
		{"auth path", rcRiskFiles("services/keycloak/authz/policy.java"), finding.Blast{}, "L"},
		{"author is not auth", rcRiskFiles("pkg/author.go"), finding.Blast{}, "S"},
		{"sql file", rcRiskFiles("db/2026_add_column.sql"), finding.Blast{}, "L"},
		{"billing word", rcRiskFiles("src/billing/invoice_total.ts"), finding.Blast{}, "L"},
		{"dependencies", rcRiskFiles("go.mod", "go.sum", "main.go"), finding.Blast{}, "M"},
		{"ci", rcRiskFiles(".github/workflows/ci.yml"), finding.Blast{}, "M"},
		{"30 direct dependents", rcRiskFiles("lib/util.go"), rcRiskDirect(30), "S"},
		{"31 direct dependents", rcRiskFiles("lib/util.go"), rcRiskDirect(31), "M"},
		{"reaches fallback", rcRiskFiles("lib/util.go"), finding.Blast{Reaches: 101}, "M"},
		{"high beats medium", rcRiskFiles("go.mod", "pkg/session/store.go"), finding.Blast{}, "L"},
	}
	for _, c := range cases {
		if got := rcPreReviewRisk(c.files, c.blast).Level; got != c.want {
			t.Errorf("%s: risk %q, want %q", c.name, got, c.want)
		}
	}
}

// A change rated S runs on KAI_REVIEW_PROFILE_LOW_RISK; anything else, or any
// change when it is unset, on KAI_REVIEW_PROFILE.
func TestLoadProfileForRisk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, model string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{"stages":{"main":{"model":"`+model+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	strong, cheap := write("strong.json", "claude-sonnet-5-5"), write("cheap.json", "zai-org/GLM-5.3-Flash")
	t.Setenv("GITHUB_REPOSITORY_FULLNAME", "acme/app")
	t.Setenv("KAI_REVIEW_PROFILE", strong)
	defer func() { rcActiveProfile = nil }()

	check := func(risk rcRisk, want string) {
		t.Helper()
		if err := rcLoadProfileForRisk("main", risk); err != nil {
			t.Fatal(err)
		}
		if got := rcStageModel(rcStageMain, "default"); got != want {
			t.Errorf("risk %q: main runs on %s, want %s", risk.Level, got, want)
		}
	}

	check(rcRisk{Level: "S"}, "claude-sonnet-5-5") // no low-risk profile named
	t.Setenv("KAI_REVIEW_PROFILE_LOW_RISK", cheap)
	check(rcRisk{Level: "S"}, "zai-org/GLM-5.3-Flash")
	check(rcRisk{Level: "M"}, "claude-sonnet-5-5")
	check(rcRisk{Level: "L"}, "claude-sonnet-5-5")
	check(rcRisk{}, "claude-sonnet-5-5") // no file list: not rated, not routed

	t.Setenv("KAI_REVIEW_PROFILE_LOW_RISK", filepath.Join(dir, "missing.json"))
	if err := rcLoadProfileForRisk("main", rcRisk{Level: "S"}); err == nil {
		t.Error("a missing low-risk profile loaded without an error")
	}
}
