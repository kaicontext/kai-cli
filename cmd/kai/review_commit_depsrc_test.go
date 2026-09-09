package main

import (
	"context"
	"strings"
	"testing"
)

// Guessing a repo from a module path is how you fetch the wrong file and then
// state it with confidence, which is worse than fetching nothing.
func TestGitHubRepoOnlyAcceptsThePlainForm(t *testing.T) {
	owner, repo, ok := rcGitHubRepo("github.com/kaicontext/kai-engine")
	if !ok || owner != "kaicontext" || repo != "kai-engine" {
		t.Errorf("= %q/%q ok=%v, want kaicontext/kai-engine", owner, repo, ok)
	}
	for _, m := range []string{
		"github.com/kaicontext/kai-engine/v2", // major-version suffix
		"github.com/kaicontext",               // not a repo
		"golang.org/x/tools",                  // not github
		"gopkg.in/yaml.v3",
		"",
	} {
		if _, _, ok := rcGitHubRepo(m); ok {
			t.Errorf("rcGitHubRepo(%q) accepted, want refused", m)
		}
	}
}

// Without a token, a commit, an importable package, or a github.com module
// there is nothing to ask for — and the dependency has to come back as
// unresolved so the limitation block still covers it. Nothing here touches
// the network.
func TestFetchLeavesWhatItCannotAskForUnresolved(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	deps := []rcDepChange{
		{Module: "github.com/kaicontext/kai-engine", To: "v0.6.59-0.20260908192613-5ab1b102fc2f", Commit: "5ab1b102fc2f", Pkgs: []string{"kaipath"}},
		{Module: "golang.org/x/tools", To: "v0.1.0", Commit: "aaaaaaaaaaaa", Pkgs: []string{"go/packages"}},
		{Module: "github.com/kaicontext/kai-engine", To: "v0.6.58"}, // no commit, no pkgs
	}
	got, unresolved := rcFetchDepSources(context.Background(), deps)
	if len(got) != 0 {
		t.Errorf("fetched %d files with no token, want 0", len(got))
	}
	if len(unresolved) != len(deps) {
		t.Errorf("unresolved = %d, want all %d — the limitation block must still cover them", len(unresolved), len(deps))
	}
}

// The source block and the limitation block are rendered from one result. A
// block telling the reviewer it cannot read a module, printed beside one
// containing that module's source, would be its own contradiction.
func TestSourceAndLimitationBlocksDoNotOverlap(t *testing.T) {
	src := []rcDepSource{{
		Module: "github.com/kaicontext/kai-engine",
		Pkg:    "kaipath",
		Path:   "kaipath/user.go",
		Body:   "package kaipath\n\nfunc UserPath(home string, parts ...string) string { return \"\" }\n",
	}}
	unresolved := []rcDepChange{{Module: "golang.org/x/tools", From: "v0.1.0", To: "v0.2.0"}}

	source := rcDepSourceBlock(src)
	for _, want := range []string{"user.go", "package kaipath", "func UserPath", "do not go looking for it"} {
		if !strings.Contains(source, want) {
			t.Errorf("source block missing %q:\n%s", want, source)
		}
	}
	// The fetched module must not also be announced as unreadable.
	limits := rcDepLimitsBlock(unresolved)
	if strings.Contains(limits, "kai-engine") {
		t.Errorf("limitation block claims a module whose source was fetched:\n%s", limits)
	}
	if !strings.Contains(limits, "golang.org/x/tools") {
		t.Errorf("limitation block dropped the module that really was unread:\n%s", limits)
	}

	// Nothing fetched: no source block at all, rather than an empty heading.
	if got := rcDepSourceBlock(nil); got != "" {
		t.Errorf("empty source block = %q, want nothing", got)
	}
}
