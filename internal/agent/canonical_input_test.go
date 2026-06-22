package agent

import (
	"testing"

	"kai/internal/projects"
)

// TestCanonicalToolInputForCache_NormalizesProjectPrefixedPaths
// pins the 2026-05-11 fix: in a multi-root workspace, two view
// calls naming the same file via different prefix conventions
// must produce the same dedupe key. Without this the dedupe cache
// misses on what's effectively a single file viewed twice.
func TestCanonicalToolInputForCache_NormalizesProjectPrefixedPaths(t *testing.T) {
	set := &projects.Set{
		DiscoveryRoot: "/spawn",
		InvokedFrom:   "/spawn",
	}
	set.SetProjectsForTest([]*projects.Project{
		{Path: "/spawn/Kai", Name: "Kai"},
		{Path: "/spawn/Kai_Server", Name: "Kai Server"},
	})

	cases := []struct {
		name string
		a    string
		b    string
		want bool // a and b should canonicalize identically
	}{
		{
			name: "project-prefixed vs already-absolute",
			a:    `{"file_path":"Kai/kai-cli/foo.go"}`,
			b:    `{"file_path":"/spawn/Kai/kai-cli/foo.go"}`,
			want: true,
		},
		{
			name: "different projects stay different",
			a:    `{"file_path":"Kai/kai-cli/foo.go"}`,
			b:    `{"file_path":"Kai Server/kai-server/foo.go"}`,
			want: false,
		},
		{
			name: "different files within same project stay different",
			a:    `{"file_path":"Kai/kai-cli/foo.go"}`,
			b:    `{"file_path":"Kai/kai-cli/bar.go"}`,
			want: false,
		},
		{
			name: "unmatched prefix leaves input alone (but JSON-canonicalized)",
			a:    `{"file_path":"unknown/foo.go"}`,
			b:    `{"file_path":"unknown/foo.go"}`,
			want: true, // identical raw → identical canonical
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ca := canonicalToolInputForCache(c.a, set)
			cb := canonicalToolInputForCache(c.b, set)
			got := ca == cb
			if got != c.want {
				t.Errorf("canonicalToolInputForCache equality\n  a: %s -> %s\n  b: %s -> %s\n  got equal=%v, want %v",
					c.a, ca, c.b, cb, got, c.want)
			}
		})
	}
}

// TestCanonicalToolInputForCache_NilSetFallsBack verifies that
// single-root or nil sets bypass the project-aware path and use
// the legacy JSON-key-order canonicalization.
func TestCanonicalToolInputForCache_NilSetFallsBack(t *testing.T) {
	a := `{"a":1,"b":2}`
	b := `{"b":2,"a":1}`
	if canonicalToolInputForCache(a, nil) != canonicalToolInputForCache(b, nil) {
		t.Errorf("nil set should still produce JSON-canonical (key-order-independent) output")
	}
}

// TestResolveProjectPath covers the small head-match helper that
// powers the path-aware canonicalization.
func TestResolveProjectPath(t *testing.T) {
	set := &projects.Set{}
	set.SetProjectsForTest([]*projects.Project{
		{Path: "/spawn/Kai", Name: "Kai"},
		{Path: "/spawn/Kai_Server", Name: "Kai Server"},
	})

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Kai/kai-cli/foo.go", "/spawn/Kai/kai-cli/foo.go", true},
		{"Kai Server/api/x.go", "/spawn/Kai_Server/api/x.go", true},
		{"unknown/foo.go", "", false},
		{"/spawn/Kai/foo.go", "/spawn/Kai/foo.go", true}, // already absolute, project owns it
		{"/elsewhere/foo.go", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := resolveProjectPath(set, c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("resolveProjectPath(%q) = (%q, %v), want (%q, %v)",
				c.in, got, ok, c.want, c.ok)
		}
	}
}
