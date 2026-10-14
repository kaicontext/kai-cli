package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func gitModes(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// `kai ship --server` must publish what git would: an executable as 100755,
// a symlink as 120000 carrying its target (never the target's content, which
// shipped the link as a copy), a dangling link as a link, and the files of a
// wholly new directory rather than the directory entry.
func TestShipServerFiles_ModesAndNewDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits and symlinks are POSIX")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	ws := t.TempDir()
	gitModes(t, ws, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "tool.sh"), []byte("#!/bin/sh\n"), 0o644)
	gitModes(t, ws, "add", "-A")
	gitModes(t, ws, "commit", "-q", "-m", "base")

	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("changed\n"), 0o644)
	os.Chmod(filepath.Join(ws, "tool.sh"), 0o755)
	os.MkdirAll(filepath.Join(ws, "bin", "sub"), 0o755)
	os.WriteFile(filepath.Join(ws, "bin", "sub", "new.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink("a.txt", filepath.Join(ws, "alias.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	os.Symlink("nowhere", filepath.Join(ws, "dangling"))

	changed, err := shipDeltaNames(ws)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(changed)
	for _, p := range changed {
		if strings.HasSuffix(p, "/") {
			t.Fatalf("directory entry %q left in the delta: %v", p, changed)
		}
	}

	files, err := shipBuildFiles(ws, nil, "", "HEAD", changed)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]shipServerFile{}
	for _, f := range files {
		got[f.Path] = f
	}
	for path, mode := range map[string]string{
		"a.txt": "100644", "tool.sh": "100755", "bin/sub/new.sh": "100755",
		"alias.txt": "120000", "dangling": "120000",
	} {
		f, ok := got[path]
		if !ok {
			t.Errorf("%s missing (delta %v)", path, changed)
			continue
		}
		if f.Mode != mode || f.Delete {
			t.Errorf("%s: mode %q delete=%v, want %q", path, f.Mode, f.Delete, mode)
		}
	}
	if b, _ := base64.StdEncoding.DecodeString(got["alias.txt"].ContentB64); string(b) != "a.txt" {
		t.Errorf("symlink shipped %q, want its target", b)
	}
}
