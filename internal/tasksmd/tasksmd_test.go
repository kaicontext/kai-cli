package tasksmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_MissingFile_NoOp(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error for missing file: %v", err)
	}
	if got.Path != "" || len(got.InProgress) != 0 || len(got.Pending) != 0 || len(got.Done) != 0 {
		t.Fatalf("Load returned non-zero value for missing file: %+v", got)
	}
}

func TestLoad_EmptyWorkDir(t *testing.T) {
	got, err := Load("")
	if err != nil || got.Path != "" {
		t.Fatalf("Load(\"\") = %+v, %v; want zero, nil", got, err)
	}
}

func TestLoad_MalformedMissingHeading(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "TASKS.md"),
		"## In progress\n- [ ] orphan\n")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.InProgress) != 0 {
		t.Fatalf("malformed file should yield zero tasks, got %+v", got.InProgress)
	}
}

func TestLoad_Basic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "TASKS.md"), `# Tasks

## In progress
- [ ] kai graph export — Phase 3
  Acceptance: kai graph export --json --limit 5 returns valid JSON
  Files: kai-cli/internal/graph/export.go

## Pending
- [ ] Fix duplicate-key bug
  Files: kai-cli/internal/graph/export.go
  Acceptance: emitted JSON has no key appearing twice
- [ ] Rewrite export_test.go

## Done
- [x] Initial scaffolding (2026-05-28)
- [x] Older work
`)
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Path == "" {
		t.Fatalf("Path should be set on a real load")
	}
	if len(got.InProgress) != 1 || got.InProgress[0].Subject != "kai graph export — Phase 3" {
		t.Fatalf("InProgress mismatch: %+v", got.InProgress)
	}
	if got.InProgress[0].Acceptance != "kai graph export --json --limit 5 returns valid JSON" {
		t.Fatalf("Acceptance mismatch: %q", got.InProgress[0].Acceptance)
	}
	if len(got.InProgress[0].Files) != 1 || got.InProgress[0].Files[0] != "kai-cli/internal/graph/export.go" {
		t.Fatalf("Files mismatch: %+v", got.InProgress[0].Files)
	}
	if len(got.Pending) != 2 {
		t.Fatalf("Pending count: %d", len(got.Pending))
	}
	if got.Pending[0].Acceptance != "emitted JSON has no key appearing twice" {
		t.Fatalf("Pending acceptance: %q", got.Pending[0].Acceptance)
	}
	if len(got.Done) != 2 {
		t.Fatalf("Done count: %d", len(got.Done))
	}
	if got.Done[0].Subject != "Initial scaffolding" || got.Done[0].DoneDate != "2026-05-28" {
		t.Fatalf("Done[0] mismatch: %+v", got.Done[0])
	}
	if got.Done[1].DoneDate != "" {
		t.Fatalf("Done[1] should have no date: %+v", got.Done[1])
	}
}

func TestLoad_DotKaiFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".kai", "tasks.md"),
		"# Tasks\n\n## Pending\n- [ ] from dotkai\n")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0].Subject != "from dotkai" {
		t.Fatalf("did not load .kai/tasks.md: %+v", got)
	}
}

func TestLoad_PrimaryWinsOverDotKai(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "TASKS.md"),
		"# Tasks\n\n## Pending\n- [ ] from root\n")
	writeFile(t, filepath.Join(dir, ".kai", "tasks.md"),
		"# Tasks\n\n## Pending\n- [ ] from dotkai\n")
	got, _ := Load(dir)
	if len(got.Pending) != 1 || got.Pending[0].Subject != "from root" {
		t.Fatalf("primary should win: %+v", got)
	}
}

func TestFormatForPrompt_EmptyReturnsBlank(t *testing.T) {
	if s := (Tasks{}).FormatForPrompt(); s != "" {
		t.Fatalf("empty Tasks should format to empty string, got %q", s)
	}
	// Only Done items: also empty.
	t2 := Tasks{Done: []Task{{Subject: "ignore me"}}}
	if s := t2.FormatForPrompt(); s != "" {
		t.Fatalf("Done-only Tasks should format to empty string, got %q", s)
	}
}

func TestFormatForPrompt_ShapeAndNumbering(t *testing.T) {
	tk := Tasks{
		InProgress: []Task{{Subject: "Active thing", Acceptance: "criterion 1"}},
		Pending: []Task{
			{Subject: "First pending", Files: []string{"a.go", "b.go"}},
			{Subject: "Second pending", Acceptance: "criterion 2"},
		},
	}
	out := tk.FormatForPrompt()
	for _, want := range []string{
		"Workspace task ledger (TASKS.md)",
		"## In progress",
		"- Active thing",
		"  Acceptance: criterion 1",
		"## Pending",
		"1. First pending",
		"   Files: a.go, b.go",
		"2. Second pending",
		"   Acceptance: criterion 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatForPrompt missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestRoundTrip_SavePreservesLoadedState(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "TASKS.md"), `# Tasks

## In progress
- [ ] Active
  Acceptance: must work

## Pending
- [ ] Next up
  Files: foo.go

## Done
- [x] Earlier (2026-05-27)
`)
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.InProgress) != 1 || second.InProgress[0].Acceptance != "must work" {
		t.Fatalf("InProgress lost: %+v", second.InProgress)
	}
	if len(second.Pending) != 1 || second.Pending[0].Files[0] != "foo.go" {
		t.Fatalf("Pending files lost: %+v", second.Pending)
	}
	if len(second.Done) != 1 || second.Done[0].DoneDate != "2026-05-27" {
		t.Fatalf("Done date lost: %+v", second.Done)
	}
}

func TestSave_EmptyPathRefuses(t *testing.T) {
	err := (Tasks{}).Save()
	if err == nil {
		t.Fatal("Save with empty Path should error")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
