package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
)

func TestSanitizeSubdir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Kai", "Kai"},
		{"kai-cli", "kai-cli"},
		{"Kai Server", "Kai_Server"},
		{"a/b/c", "a_b_c"},
		{"with  multiple   spaces", "with_multiple_spaces"},
		{"trailing space ", "trailing_space"},
		{"", "project"},
		{"   ", "project"},
	}
	for _, c := range cases {
		got := sanitizeSubdir(c.in)
		if got != c.want {
			t.Errorf("sanitizeSubdir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteSpawnProjectsYAML verifies the format matches what
// projects.LoadFile expects (relative paths, name, indent).
// Round-trip is the cheapest correctness check.
func TestWriteSpawnProjectsYAML(t *testing.T) {
	spawnDir := t.TempDir()
	ps := []*projects.Project{
		{Path: filepath.Join(spawnDir, "Kai"), Name: "Kai"},
		{Path: filepath.Join(spawnDir, "Kai_Server"), Name: "Kai Server"},
	}
	if err := writeSpawnProjectsYAML(spawnDir, ps); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Re-load via projects.LoadFile and compare names + paths.
	entries, err := projects.LoadFile(spawnDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	gotNames := map[string]string{} // name -> path
	for _, e := range entries {
		gotNames[e.Name] = e.Path
	}
	if gotNames["Kai"] == "" || !strings.HasSuffix(gotNames["Kai"], "Kai") {
		t.Errorf("Kai entry missing or wrong path: %v", gotNames)
	}
	if gotNames["Kai Server"] == "" || !strings.HasSuffix(gotNames["Kai Server"], "Kai_Server") {
		t.Errorf("Kai Server entry missing or wrong path: %v", gotNames)
	}

	// Sanity check the file is readable as-is for a human (catches
	// regressions in the indent/format if someone changes the
	// writer without updating the loader).
	body, err := os.ReadFile(filepath.Join(spawnDir, "kai.projects.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)
	for _, want := range []string{"projects:", "name: Kai\n", "name: Kai Server\n", "path: Kai\n", "path: Kai_Server\n"} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("yaml missing %q. Full body:\n%s", want, bodyStr)
		}
	}
}

// TestSpawnForMulti_RejectsSingleRoot guards the contract that
// callers must dispatch single-project sets to spawnFor, not
// spawnForMulti. Falling through silently would produce a spawn
// dir with one project but only ONE subdir — confusing the layout
// downstream.
func TestSpawnForMulti_RejectsSingleRoot(t *testing.T) {
	cases := []*projects.Set{
		nil,
		projects.New("/tmp", nil),
		projects.New("/tmp", []*projects.Project{{Path: "/tmp/a", Name: "a"}}),
	}
	for i, set := range cases {
		_, _, _, err := spawnForMulti(nil, "task", Config{}, set)
		if err == nil {
			t.Errorf("case %d: expected error for non-multi-root set, got nil", i)
		}
	}
}

// TestIsNoSnapshotsCheckoutError pins the patterns the multi-root
// spawn treats as "skip this project, it has no captures yet"
// versus a real checkout failure. Surfaced live in dogfood
// (2026-05-12) when a multi-root workspace included an empty
// placeholder project (kai-tui) that had never been captured: the
// spawn aborted the entire agent run with the @snap:last~0 error
// instead of just dropping the empty project and continuing.
func TestIsNoSnapshotsCheckoutError(t *testing.T) {
	skips := []string{
		// Real output captured from `kai checkout @snap:last` in an
		// uncaptured project.
		"Error: resolving snapshot ID: not found: @snap:last~0",
		// Variant phrasing from the older resolver.
		"no snapshots in DB",
		// Surrounded by other text (the combined output may include
		// stderr noise above the error line).
		"Scanning files...\nError: resolving snapshot ID: not found: @snap:last\n",
	}
	for _, s := range skips {
		if !isNoSnapshotsCheckoutError(s) {
			t.Errorf("expected skip match for %q", s)
		}
	}

	keeps := []string{
		// Real failures the spawn MUST surface, not silently skip.
		"Error: opening graph: permission denied",
		"Error: invalid checkout target: unknown ref",
		"",
		"Error: connection refused",
	}
	for _, s := range keeps {
		if isNoSnapshotsCheckoutError(s) {
			t.Errorf("must NOT match (would silently swallow real error): %q", s)
		}
	}
}

// TestSummarizeCheckoutError pins the classification map. Each
// failure mode should produce a recognizable, action-oriented
// one-liner — a user reading "skipping kai-tui because..." should
// know what to do next. Uncategorized failures fall through to a
// truncated raw line so we never lose information for new modes.
func TestSummarizeCheckoutError(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		err     error
		wantSub string
	}{
		{"no captures", "Error: not found: @snap:last~0", errStub("exit 1"), "no captures yet"},
		{"permission denied", "Error: open .kai/db.sqlite: permission denied", errStub("exit 1"), "permission denied"},
		{"db locked", "database is locked", errStub("exit 1"), "kai database locked"},
		{"cancelled", "context canceled", errStub("signal: interrupt"), "canceled"},
		{"uncategorized first line wins", "Error: weird thing happened\n  stack trace line\n  another", errStub("exit 1"), "weird thing happened"},
		{"empty falls back to err", "", errStub("signal: killed"), "checkout failed"},
	}
	for _, c := range cases {
		got := summarizeCheckoutError(c.out, c.err)
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("%s: summary = %q, want substring %q", c.name, got, c.wantSub)
		}
	}
}

func errStub(s string) error { return errors.New(s) }

// TestProjectDirBasename_PrefersPathOverName pins the spawn-dir
// naming fix from the 2026-05-12 incident. A project whose
// kai.projects.yaml says `name: Kai` but lives on disk at `kai/`
// must spawn into `kai/`, not `Kai/`. Otherwise the snapshot
// carries paths that don't match the workspace, and apply-back
// creates a sibling tree instead of overwriting in place — which
// took ~30 minutes to recover from in the dogfood loop.
func TestProjectDirBasename_PrefersPathOverName(t *testing.T) {
	root := t.TempDir()
	onDisk := filepath.Join(root, "kai-cli")
	if err := os.MkdirAll(onDisk, 0o755); err != nil {
		t.Fatal(err)
	}
	// Different casing on Name vs the on-disk basename. Pre-fix
	// behavior would return "Kai-CLI" (from Name); the fix returns
	// the basename of Path.
	got := projectDirBasename(&projects.Project{Name: "Kai-CLI", Path: onDisk})
	if got != "kai-cli" {
		t.Errorf("expected basename of Path (kai-cli), got %q", got)
	}
}

// TestProjectDirBasename_ResolvesDotPath covers the literal repro
// from the 2026-05-12 incident: a project entry with `path: .`
// resolves to the current working directory's basename, never to
// the literal "." string (which would produce a junk subdir name).
func TestProjectDirBasename_ResolvesDotPath(t *testing.T) {
	tmp := t.TempDir()
	// chdir so "." resolves to a stable known dir for the test.
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	got := projectDirBasename(&projects.Project{Name: "Kai", Path: "."})
	want := filepath.Base(tmp)
	if got != want {
		t.Errorf("dot-path: got %q, want %q (basename of cwd)", got, want)
	}
}

// TestProjectDirBasename_FallsBackToName covers the pathless case
// (test fixtures, malformed yaml). The Name fallback is acceptable
// here because there's no on-disk truth to anchor against.
func TestProjectDirBasename_FallsBackToName(t *testing.T) {
	got := projectDirBasename(&projects.Project{Name: "fixture-name", Path: ""})
	if got != "fixture-name" {
		t.Errorf("expected fallback to Name, got %q", got)
	}
}
