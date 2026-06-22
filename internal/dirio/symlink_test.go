package dirio

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWalkDir_SkipsSymlinks pins the May 2026 moby fix: symlinks must
// not appear in the walked file list. Capture-then-checkout can't
// round-trip them safely (it'd convert the link to a copy of its
// target), so the safest path is to leave them outside kai's purview.
//
// Setup:
//   tmp/
//     real.txt   (regular file, content "hello")
//     link.txt   → real.txt   (symlink)
//
// Expectation: the walker yields only "real.txt"; "link.txt" is
// silently skipped. Same for symlinks pointing at directories
// (which the previous code already skipped).
func TestWalkDir_SkipsSymlinks(t *testing.T) {
	tmp := t.TempDir()

	// Regular file.
	realPath := filepath.Join(tmp, "real.txt")
	if err := os.WriteFile(realPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlink pointing at the regular file.
	if err := os.Symlink("real.txt", filepath.Join(tmp, "link.txt")); err != nil {
		t.Skipf("os.Symlink not permitted on this filesystem: %v", err)
	}

	// Regular file in a subdir + a symlink to that subdir, to cover
	// the directory-link branch.
	if err := os.Mkdir(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	subFile := filepath.Join(tmp, "sub", "kept.txt")
	if err := os.WriteFile(subFile, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(tmp, "subdir-link")); err != nil {
		t.Skipf("os.Symlink not permitted on this filesystem: %v", err)
	}

	ds, err := OpenDirectory(tmp)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	files, err := ds.GetFiles()
	if err != nil {
		t.Fatalf("get files: %v", err)
	}

	got := make(map[string]bool, len(files))
	for _, f := range files {
		got[filepath.ToSlash(f.Path)] = true
	}

	// Real files must be present.
	for _, want := range []string{"real.txt", "sub/kept.txt"} {
		if !got[want] {
			t.Errorf("expected %q in walk results, missing", want)
		}
	}
	// Symlinks must NOT be present, neither file-link nor dir-link.
	for _, unwanted := range []string{"link.txt", "subdir-link", "subdir-link/kept.txt"} {
		if got[unwanted] {
			t.Errorf("symlink %q should be skipped, found in walk results", unwanted)
		}
	}
}
