package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAbsorb_RespectsCaptureIgnoreRules pins the May 2026 moby
// regression: absorb's walker used to apply a stricter ignore set
// than `kai capture`, so any file capture excluded (vendor/, *.pem,
// build outputs) wouldn't appear in the spawn snapshot, and absorb
// would treat its absence in spawn as "agent deleted this" — and
// rm the file from main. The fix loads the same ignore.LoadFromDir
// matcher capture uses, so absorb skips the same paths capture did
// and never considers them for deletion.
//
// Setup:
//
//	main/
//	  src/main.go         (in spawn — agent left it alone)
//	  vendor/foo/foo.go   (NOT in spawn — vendor/ is default-excluded)
//	  README.md           (in spawn, with a 1-line edit)
//	spawn/
//	  src/main.go         (identical to main)
//	  README.md           (modified by agent)
//	  (no vendor/ — capture excluded it, so spawn never had it)
//
// Without the fix: absorb would delete main/vendor/foo/foo.go.
// With the fix: vendor/ is in the ignore set, walker skips it on
// the main side too, no spurious deletion.
func TestAbsorb_RespectsCaptureIgnoreRules(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	spawn := filepath.Join(tmp, "spawn")

	// Build main: src/main.go, vendor/foo/foo.go, README.md, plus a
	// .gitignore that excludes vendor/ (the conventional setup for
	// projects that don't commit their deps). After v0.20.0 kai's
	// defaults no longer hardcode vendor/ — capture trusts gitignore.
	mustMkdirAll(t, filepath.Join(main, "src"))
	mustMkdirAll(t, filepath.Join(main, "vendor", "foo"))
	mustWrite(t, filepath.Join(main, "src", "main.go"), "package main\nfunc main(){}\n")
	mustWrite(t, filepath.Join(main, "vendor", "foo", "foo.go"), "package foo\n// vendored\n")
	mustWrite(t, filepath.Join(main, "README.md"), "# title\n")
	mustWrite(t, filepath.Join(main, ".gitignore"), "vendor/\n")

	// Build spawn as if `kai checkout` materialized snap.latest:
	// src/main.go identical, README.md edited, NO vendor/ (capture
	// excluded it, so the snapshot has no vendor entries to check
	// out).
	mustMkdirAll(t, filepath.Join(spawn, "src"))
	mustWrite(t, filepath.Join(spawn, "src", "main.go"), "package main\nfunc main(){}\n")
	mustWrite(t, filepath.Join(spawn, "README.md"), "# title\n\nedited by agent\n")
	// .gitignore is tracked content; kai checkout materializes it
	// into spawn alongside the source files.
	mustWrite(t, filepath.Join(spawn, ".gitignore"), "vendor/\n")

	changed, err := absorbSpawnIntoMain(spawn, main)
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}

	// Only the README edit should be reported as changed. vendor/ is
	// gitignored, the matcher skips it, absorb never sees it, and
	// it is NOT deleted.
	wantChanged := []string{"README.md"}
	got := append([]string(nil), changed...)
	sort.Strings(got)
	sort.Strings(wantChanged)
	if strings.Join(got, ",") != strings.Join(wantChanged, ",") {
		t.Errorf("changed paths: got %v, want %v", got, wantChanged)
	}

	// Critical assertion: the vendor file must still exist on disk.
	if _, err := os.Stat(filepath.Join(main, "vendor", "foo", "foo.go")); err != nil {
		t.Fatalf("regression: vendor/foo/foo.go was wrongly deleted by absorb: %v", err)
	}

	// And the README edit landed.
	body, _ := os.ReadFile(filepath.Join(main, "README.md"))
	if !strings.Contains(string(body), "edited by agent") {
		t.Errorf("README.md was not updated; got: %q", body)
	}
}

// TestAbsorb_StillDeletesAgentDeletions confirms we didn't over-correct:
// when the agent ACTUALLY deletes a tracked (non-ignored) file, absorb
// must still propagate that deletion to main. Otherwise refactors that
// remove a file would silently leave stale copies behind.
func TestAbsorb_StillDeletesAgentDeletions(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	spawn := filepath.Join(tmp, "spawn")

	mustMkdirAll(t, filepath.Join(main, "src"))
	mustMkdirAll(t, filepath.Join(spawn, "src"))

	// Both start with the file present.
	mustWrite(t, filepath.Join(main, "src", "old.go"), "package main\n")
	// Spawn no longer has it (agent rm'd it).

	// Both have a kept file so the walker has something to compare.
	mustWrite(t, filepath.Join(main, "src", "kept.go"), "package main\nvar X = 1\n")
	mustWrite(t, filepath.Join(spawn, "src", "kept.go"), "package main\nvar X = 1\n")

	changed, err := absorbSpawnIntoMain(spawn, main)
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}

	if !contains(changed, "src/old.go") {
		t.Errorf("expected src/old.go in changed paths (deletion), got %v", changed)
	}
	if _, err := os.Stat(filepath.Join(main, "src", "old.go")); !os.IsNotExist(err) {
		t.Errorf("src/old.go should have been deleted from main")
	}
	if _, err := os.Stat(filepath.Join(main, "src", "kept.go")); err != nil {
		t.Errorf("src/kept.go should still exist: %v", err)
	}
}

// helpers

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

// TestValidateNoCaseCollisions_RefusesCaseAliasingTopLevel pins the
// 2026-05-12 case-aliasing guard. A spawn rooted at Kai/ next to a
// main at kai/ used to absorb into a duplicate tree + delete the
// original. The guard must refuse rather than apply.
func TestValidateNoCaseCollisions_RefusesCaseAliasingTopLevel(t *testing.T) {
	spawn := make(map[string]string)
	main := make(map[string]string)
	for i := 0; i < 50; i++ {
		spawn[fmt.Sprintf("Kai/kai-cli/file_%d.go", i)] = "x"
		main[fmt.Sprintf("kai-cli/file_%d.go", i)] = "x"
	}
	err := validateNoCaseCollisions(spawn, main)
	if err == nil {
		t.Fatal("expected absorb to refuse on prefix-mismatched trees")
	}
	if !strings.Contains(err.Error(), "Kai") {
		t.Errorf("error should name the foreign top-level; got: %v", err)
	}
}

// TestValidateNoCaseCollisions_AllowsSmallMixedChanges covers the
// false-positive risk: agent legitimately renames a small module
// (adds to new top-level, deletes from old). Below threshold,
// blast radius is small enough that user-visible review is cheap.
func TestValidateNoCaseCollisions_AllowsSmallMixedChanges(t *testing.T) {
	spawn := make(map[string]string)
	main := make(map[string]string)
	for i := 0; i < 5; i++ {
		spawn[fmt.Sprintf("newpkg/file_%d.go", i)] = "x"
		main[fmt.Sprintf("oldpkg/file_%d.go", i)] = "x"
	}
	if err := validateNoCaseCollisions(spawn, main); err != nil {
		t.Errorf("guard tripped on small below-threshold rename: %v", err)
	}
}

// TestValidateNoCaseCollisions_AllowsRealRefactor covers the case
// where adds happen under EXISTING top-levels. Even at high volume
// that's a real refactor and the guard must allow it.
func TestValidateNoCaseCollisions_AllowsRealRefactor(t *testing.T) {
	spawn := make(map[string]string)
	main := make(map[string]string)
	for i := 0; i < 50; i++ {
		spawn[fmt.Sprintf("kai-cli/new_%d.go", i)] = "x"
		main[fmt.Sprintf("kai-cli/old_%d.go", i)] = "x"
	}
	if err := validateNoCaseCollisions(spawn, main); err != nil {
		t.Errorf("guard false-positived on mass rename within existing top-level: %v", err)
	}
}

// TestValidateNoCaseCollisions_AllowsLegitimateNewTopLevel covers the
// case the guard must NOT false-positive on: agent legitimately adds
// a new top-level directory (e.g. a new module in a monorepo). When
// the new name doesn't case-insensitively match anything existing,
// absorb should proceed.
func TestValidateNoCaseCollisions_AllowsLegitimateNewTopLevel(t *testing.T) {
	spawn := map[string]string{
		"existing/file.go":  "x",
		"newmodule/foo.go":  "y", // legitimately new
	}
	main := map[string]string{
		"existing/file.go": "x",
	}
	if err := validateNoCaseCollisions(spawn, main); err != nil {
		t.Errorf("guard false-positived on a legitimate new top-level dir: %v", err)
	}
}

// TestValidateNoCaseCollisions_AllowsIdenticalTrees covers the
// happy path: spawn and main have the same top-level dirs and the
// guard is silent.
func TestValidateNoCaseCollisions_AllowsIdenticalTrees(t *testing.T) {
	spawn := map[string]string{"kai-cli/a.go": "1", "github.com/kaicontext/kai-core/b.go": "2"}
	main := map[string]string{"kai-cli/a.go": "1", "github.com/kaicontext/kai-core/b.go": "2"}
	if err := validateNoCaseCollisions(spawn, main); err != nil {
		t.Errorf("guard tripped on identical trees: %v", err)
	}
}

// TestAbsorb_MultiRootScopesToPrimarySubdir simulates the multi-root
// container case that broke during the 2026-05-14 diagnose-pack-bug
// run: the spawn dir contains kai/, kai-server/, kai-tui/ as
// sibling subdirs, but mainRepo is just kai-server's contents.
// Passing spawn root + mainRepo would diff at incompatible depths
// and trip validateNoCaseCollisions. Passing
// `<spawn>/<primary-basename>` + mainRepo aligns the walker depths
// and absorb completes normally.
func TestAbsorb_MultiRootScopesToPrimarySubdir(t *testing.T) {
	tmpSpawn := t.TempDir()
	tmpMain := t.TempDir()

	// Spawn layout: container with multiple project subdirs. The
	// validator refuses only when both adds and deletes exceed a
	// threshold, so generate enough files to trip it.
	writeFile := func(root, rel string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content of "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		writeFile(tmpSpawn, fmt.Sprintf("kai/internal/x%d.go", i))
		writeFile(tmpSpawn, fmt.Sprintf("kai-server/api/server%d.go", i))
		writeFile(tmpSpawn, fmt.Sprintf("kai-tui/main%d.go", i))
	}
	writeFile(tmpSpawn, "kai-server/api/server-modified.go") // the agent's edit

	// mainRepo is kai-server only (its actual contents at root) — has
	// the originals of server*.go but none of the kai/, kai-tui/ files,
	// and is missing server-modified.go.
	for i := 0; i < 25; i++ {
		writeFile(tmpMain, fmt.Sprintf("api/server%d.go", i))
	}
	// Add files only in main (will look like deletes from the bad-spawn POV).
	for i := 0; i < 25; i++ {
		writeFile(tmpMain, fmt.Sprintf("api/onlymain%d.go", i))
	}

	// Bug repro: passing the spawn ROOT trips the validator because
	// the diff looks like "spawn has kai/, kai-server/, kai-tui/ at
	// top-level that main doesn't have."
	if _, err := absorbSpawnIntoMain(tmpSpawn, tmpMain); err == nil {
		t.Errorf("expected absorb refusal when spawn root is passed (multi-root depth mismatch)")
	}

	// Fix: pass <spawnRoot>/kai-server — the primary project's
	// subdir within the spawn. Walker depths align and the agent's
	// edit shows up correctly.
	scopedSpawn := filepath.Join(tmpSpawn, "kai-server")
	changed, err := absorbSpawnIntoMain(scopedSpawn, tmpMain)
	if err != nil {
		t.Fatalf("absorb with scoped spawn root: %v", err)
	}
	// Expect: server-modified.go was created in mainRepo's api/ dir.
	if want := "api/server-modified.go"; !contains(changed, want) {
		t.Errorf("changed paths missing %q, got %v", want, changed)
	}
}

