package views

import (
	"testing"

	"kai/api/projects"
)

// TestWorkspaceFor_PrefersInvokedFrom is the 2026-05-11 regression:
// the banner used to show the primary project's path
// (~/projects/kai/kai) even when the user invoked from a multi-root
// parent (~/projects/kai). Now it shows what the user typed `cd`
// to, exactly matching their expectation.
func TestWorkspaceFor_PrefersInvokedFrom(t *testing.T) {
	set := &projects.Set{
		DiscoveryRoot: "/Users/me/projects/kai",
		InvokedFrom:   "/Users/me/projects/kai",
	}
	set.SetProjectsForTest([]*projects.Project{
		{Path: "/Users/me/projects/kai/kai", Name: "Kai"},
		{Path: "/Users/me/projects/kai/kai-server", Name: "Kai Server"},
	})
	s := &PlannerServices{
		Projects: set,
		MainRepo: "/Users/me/projects/kai/kai", // primary.Path — what we used to show
	}
	got := workspaceFor(s)
	want := "/Users/me/projects/kai"
	if got != want {
		t.Errorf("workspaceFor = %q, want %q (the dir the user invoked from, not primary.Path)", got, want)
	}
}

// TestWorkspaceFor_FallsBackToMainRepo verifies the non-multi-root
// path: when Projects is nil, banner falls back to MainRepo as it
// always did.
func TestWorkspaceFor_FallsBackToMainRepo(t *testing.T) {
	s := &PlannerServices{MainRepo: "/Users/me/projects/single"}
	if got := workspaceFor(s); got != "/Users/me/projects/single" {
		t.Errorf("workspaceFor without Projects = %q, want MainRepo fallback", got)
	}
}

// TestWorkspaceFor_FallsBackWhenInvokedFromEmpty covers the edge
// case where Projects is set but InvokedFrom isn't (Set built via
// projects.New / Single, both leave InvokedFrom zero). MainRepo
// wins.
func TestWorkspaceFor_FallsBackWhenInvokedFromEmpty(t *testing.T) {
	set := projects.New("/discovery", []*projects.Project{{Path: "/discovery/p", Name: "p"}})
	s := &PlannerServices{Projects: set, MainRepo: "/discovery/p"}
	if got := workspaceFor(s); got != "/discovery/p" {
		t.Errorf("workspaceFor with empty InvokedFrom = %q, want MainRepo fallback", got)
	}
}

// TestProviderBannerLabel pins the banner copy for each provider
// kind. The banner is a single-line at-a-glance signal that BYOM
// users rely on to know "did my KAI_PROVIDER env actually take
// effect" — getting this wrong cost a debugging round when the
// banner kept saying "kailab" even with KAI_PROVIDER=anthropic
// set. Don't let that regress.
func TestProviderBannerLabel(t *testing.T) {
	cases := []struct {
		name        string
		kaiProvider string
		openaiBase  string
		want        string
	}{
		{"unset defaults to kailab", "", "", "kailab"},
		{"explicit kailab", "kailab", "", "kailab"},
		{"anthropic-direct", "anthropic", "", "anthropic (direct)"},
		{"openai default endpoint", "openai", "", "openai (direct)"},
		{"openai pointing at api.openai.com explicitly", "openai", "https://api.openai.com/v1", "openai (direct)"},
		{"openai pointing at Ollama", "openai", "http://localhost:11434/v1", "openai-compatible @ localhost:11434"},
		{"openai pointing at Together", "openai", "https://api.together.xyz/v1", "openai-compatible @ api.together.xyz"},
		{"openai with bare host (no scheme)", "openai", "vllm-pod:8000/v1", "openai-compatible @ vllm-pod:8000"},
		{"unknown kind falls through verbatim", "bedrock", "", "bedrock"},
		{"casing is normalized", "ANTHROPIC", "", "anthropic (direct)"},

		// Alias coverage: each of these should resolve to the
		// same canonical label as its primary spelling.
		{"alias openai-compat", "openai-compat", "http://localhost:1234/v1", "openai-compatible @ localhost:1234"},
		{"alias openai-compatible", "openai-compatible", "http://localhost:1234/v1", "openai-compatible @ localhost:1234"},
		{"alias oai-compat", "oai-compat", "http://localhost:1234/v1", "openai-compatible @ localhost:1234"},
		{"alias local", "local", "http://localhost:1234/v1", "openai-compatible @ localhost:1234"},
		{"alias anthropic-direct", "anthropic-direct", "", "anthropic (direct)"},
		{"alias claude", "claude", "", "anthropic (direct)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAI_PROVIDER", tc.kaiProvider)
			t.Setenv("KAI_OPENAI_BASE_URL", tc.openaiBase)
			if got := providerBannerLabel(); got != tc.want {
				t.Errorf("providerBannerLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}
