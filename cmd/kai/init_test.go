package main

import (
	"testing"

	"github.com/kaicontext/kai-engine/remote"
)

func TestPersonalSlug(t *testing.T) {
	cases := map[string]string{
		"jschatz1@gmail.com":       "jschatz1",
		"Jacob.Schatz@Example.COM": "jacob-schatz",
		"no-at-sign":               "",
		"":                         "",
	}
	for in, want := range cases {
		if got := personalSlug(in); got != want {
			t.Errorf("personalSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickPersonalOrg(t *testing.T) {
	orgs := []remote.OrgInfo{
		{Slug: "calendardev"},
		{Slug: "howth"},
		{Slug: "jschatz1"},
	}

	// Matches the personal org by email local-part, not just the first.
	if got := pickPersonalOrg(orgs, "jschatz1@gmail.com"); got == nil || got.Slug != "jschatz1" {
		t.Fatalf("expected personal org jschatz1, got %+v", got)
	}
	// No match → fall back to the first org.
	if got := pickPersonalOrg(orgs, "nobody@elsewhere.com"); got == nil || got.Slug != "calendardev" {
		t.Fatalf("expected fallback to first org, got %+v", got)
	}
	// No orgs → nil.
	if got := pickPersonalOrg(nil, "jschatz1@gmail.com"); got != nil {
		t.Fatalf("expected nil for empty org list, got %+v", got)
	}
}
