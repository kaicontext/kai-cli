package main

import "testing"

// TestLastJSONLine pins that a leading update banner cannot break the
// kit-version probe. `kit version --json` prints "Update available: ..."
// above its JSON, and parsing the whole output fails.
func TestLastJSONLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"version":"1.2.3"}`, `{"version":"1.2.3"}`},
		{"Update available: 0.33.1 → 0.35.1\n{\"version\":\"1.2.3\"}", `{"version":"1.2.3"}`},
		{"a\nb\n{\"x\":1}\n", `{"x":1}`},
	} {
		if got := string(lastJSONLine([]byte(c.in))); got != c.want {
			t.Errorf("lastJSONLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsDevBuild pins the signal that stops `kai update` reverting
// unreleased work.
//
// A release build leaves GitSHA unset; the Makefile stamps it. Without
// this, a source build and a release report the SAME version — the tag
// names the last release, not the tree — so replacing one with the
// other is invisible.
func TestIsDevBuild(t *testing.T) {
	orig := GitSHA
	defer func() { GitSHA = orig }()

	for _, c := range []struct {
		sha  string
		want bool
	}{
		{"", false},        // release: pipeline sets nothing
		{"unknown", false}, // explicit default, also not a dev build
		{"6c5848d", true},  // built from a commit
		{"6c5848d.dirty.0819-1548", true},
	} {
		GitSHA = c.sha
		if got := isDevBuild(); got != c.want {
			t.Errorf("GitSHA=%q: isDevBuild() = %v, want %v", c.sha, got, c.want)
		}
		if c.want && versionString() == Version {
			t.Errorf("GitSHA=%q: version string is indistinguishable from a release", c.sha)
		}
		if !c.want && versionString() != Version {
			t.Errorf("GitSHA=%q: release build should print the bare version, got %q", c.sha, versionString())
		}
	}
}
