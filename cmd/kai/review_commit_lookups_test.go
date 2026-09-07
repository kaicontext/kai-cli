package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lookupRepo builds a small git repo whose shape is the one that defeated the
// reviewer on kai-desktop#286: state held as object properties and
// closure-scoped variables, which the indexer does not record as symbols.
func lookupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")

	panel := `(function () {
  let lastModelJSON = null;
  let dvClosed = false;
  const api = {
    render(rows) {
      this.reviews = rows;
      lastModelJSON = JSON.stringify(rows);
    },
    close() { dvClosed = true; },
  };
  window.api = api;
})();
`
	if err := os.WriteFile(filepath.Join(dir, "panel.js"), []byte(panel), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file where a very common name lives many times over, so the
	// too-common rule has something to reject.
	var noisy strings.Builder
	for i := 0; i < 60; i++ {
		noisy.WriteString("const handler = require('./h');\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "noisy.js"), []byte(noisy.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

// The case this file exists for. An identifier the diff assigns, which is not
// a declaration (so CHANGED SYMBOLS misses it) and not a graph symbol (so
// kai_callers cannot answer it), must arrive already resolved.
func TestIdentifierLookupsResolveClosureAndPropertyState(t *testing.T) {
	dir := lookupRepo(t)
	diff := `--- a/panel.js
+++ b/panel.js
@@ -1,3 +1,4 @@
+      lastModelJSON = JSON.stringify(rows);
+      this.reviews = rows;
+      dvClosed = false;
`
	got := rcIdentifierLookups(diff, dir, nil)
	if got == "" {
		t.Fatal("no lookups resolved for identifiers that are plainly in the repo")
	}
	for _, want := range []string{"lastModelJSON", "reviews", "dvClosed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "panel.js:") {
		t.Errorf("lookups carry no path:line:\n%s", got)
	}
}

// Declarations are already seeded by CHANGED SYMBOLS, which tells the reviewer
// to open those files directly. Resolving them again would contradict that
// instruction with a list of one.
func TestIdentifierLookupsSkipWhatChangedSymbolsAlreadyCovers(t *testing.T) {
	dir := lookupRepo(t)
	diff := `--- a/panel.js
+++ b/panel.js
@@ -1,3 +1,4 @@
+  lastModelJSON = 1;
`
	got := rcIdentifierLookups(diff, dir, map[string]bool{"lastModelJSON": true})
	if strings.Contains(got, "lastModelJSON") {
		t.Fatalf("an already-seeded declaration was resolved again:\n%s", got)
	}
}

// An identifier that appears everywhere carries no signal, and would crowd out
// the ones that do. Seeding is only a win while it is smaller than the turns it
// replaces.
func TestIdentifierLookupsDropTooCommonNames(t *testing.T) {
	dir := lookupRepo(t)
	diff := `--- a/noisy.js
+++ b/noisy.js
@@ -1,3 +1,4 @@
+const handler = require('./h');
`
	got := rcIdentifierLookups(diff, dir, nil)
	if strings.Contains(got, "handler") {
		t.Fatalf("a name occurring %d+ times was seeded:\n%s", rcLookupTooCommon, got)
	}
}

// Short and ubiquitous names never enter the running: `err`, `ctx` and their
// kind resolve to noise in every repository.
func TestLookupCandidatesRejectShortAndStopWords(t *testing.T) {
	diff := `--- a/x.go
+++ b/x.go
@@ -1,3 +1,4 @@
+	if err := doThing(ctx); err != nil { return err }
+	var interestingThing = 1
`
	got := rcLookupCandidates(diff, nil)
	for _, bad := range []string{"err", "ctx", "return", "nil"} {
		for _, c := range got {
			if c == bad {
				t.Errorf("%q should never be a lookup candidate: %v", bad, got)
			}
		}
	}
	found := false
	for _, c := range got {
		if c == "interestingThing" {
			found = true
		}
	}
	if !found {
		t.Errorf("a real identifier was dropped: %v", got)
	}
}

// Ordering is by mentions and then alphabetical — deterministic, because the
// prompt is cached across turns and an unstable block would defeat that.
func TestLookupCandidatesAreDeterministic(t *testing.T) {
	diff := `--- a/x.go
+++ b/x.go
@@ -1,3 +1,4 @@
+	alphaThing = betaThing
+	alphaThing = gammaThing
`
	first := rcLookupCandidates(diff, nil)
	for i := 0; i < 5; i++ {
		if got := rcLookupCandidates(diff, nil); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("candidate order is unstable: %v vs %v", got, first)
		}
	}
	if len(first) == 0 || first[0] != "alphaThing" {
		t.Fatalf("most-mentioned identifier should sort first, got %v", first)
	}
}

// No repo, no git, no matches: the review proceeds exactly as before. This is
// an optimisation, and an optimisation that can fail a review is a defect.
func TestIdentifierLookupsAreBestEffort(t *testing.T) {
	if got := rcIdentifierLookups("+foo := barBaz()", t.TempDir(), nil); got != "" {
		t.Fatalf("expected silence outside a repo, got %q", got)
	}
	if got := rcIdentifierLookups("", ".", nil); got != "" {
		t.Fatalf("expected silence for an empty diff, got %q", got)
	}
}

// The block has to stay smaller than what it replaces.
func TestIdentifierLookupsRespectTheByteCap(t *testing.T) {
	dir := lookupRepo(t)
	var diff strings.Builder
	diff.WriteString("--- a/panel.js\n+++ b/panel.js\n@@ -1,3 +1,4 @@\n")
	for i := 0; i < 200; i++ {
		diff.WriteString("+  lastModelJSON = dvClosed;\n")
	}
	if got := rcIdentifierLookups(diff.String(), dir, nil); len(got) > rcLookupMaxBytes {
		t.Fatalf("block is %d bytes, cap is %d", len(got), rcLookupMaxBytes)
	}
}
