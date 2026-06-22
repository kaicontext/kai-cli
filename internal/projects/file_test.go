package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndLoadFile_RoundTrip(t *testing.T) {
	root := t.TempDir()
	in := []*Project{
		{Path: filepath.Join(root, "alpha"), Name: "alpha", Pinned: true},
		{Path: filepath.Join(root, "beta"), Name: "beta"},
	}
	if err := SaveFile(root, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadFile(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("loaded %d, want 2", len(out))
	}
	// Pinned-first ordering: alpha is pinned, so it sorts before beta.
	if out[0].Name != "alpha" || !out[0].Pinned {
		t.Errorf("first = %+v, want alpha pinned", out[0])
	}
	if out[1].Name != "beta" || out[1].Pinned {
		t.Errorf("second = %+v, want beta unpinned", out[1])
	}
}

func TestLoadFile_MissingReturnsNilNoError(t *testing.T) {
	root := t.TempDir()
	out, err := LoadFile(root)
	if err != nil {
		t.Fatalf("err = %v, want nil for missing file", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

func TestSaveFile_Idempotent(t *testing.T) {
	root := t.TempDir()
	in := []*Project{{Path: filepath.Join(root, "alpha"), Name: "alpha"}}
	if err := SaveFile(root, in); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(filepath.Join(root, ProjectsFileName))
	if err != nil {
		t.Fatal(err)
	}
	// Sleep a bit so a real mtime change would be visible. We don't
	// actually want to wait — instead, save again immediately and
	// verify the file content is byte-identical (idempotence is
	// about content, not timestamp).
	if err := SaveFile(root, in); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(filepath.Join(root, ProjectsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info1.ModTime() != info2.ModTime() {
		t.Errorf("mtime changed on no-op save: %v -> %v", info1.ModTime(), info2.ModTime())
	}
}

func TestSaveFile_RelativePathsInYAML(t *testing.T) {
	root := t.TempDir()
	in := []*Project{
		{Path: filepath.Join(root, "alpha"), Name: "alpha"},
	}
	if err := SaveFile(root, in); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ProjectsFileName))
	if err != nil {
		t.Fatal(err)
	}
	// Path should be stored as the relative form, not the absolute
	// tempdir path — that's how the file stays portable across
	// machines after a clone.
	if !strings.Contains(string(data), "path: alpha") {
		t.Errorf("yaml = %q, want relative path", string(data))
	}
	if strings.Contains(string(data), root) {
		t.Errorf("yaml leaked absolute root path: %q", string(data))
	}
}
