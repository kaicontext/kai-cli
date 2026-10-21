package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/ref"
)

func gitT(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runGitImport (kai init / kai import --git) must map EVERY imported commit
// — git.<sha12> plus git.HEAD for the tip — to a snapshot of exactly that
// commit's tree, without touching the working tree. It used to write no
// mapping at all, and it checked out each commit, so untracked and
// uncommitted files leaked into every historical snapshot.
func TestGitImportMapsEveryCommitFromGitObjects(t *testing.T) {
	setupBridgeRepo(t) // git repo with one commit (hello.txt) + .kai
	// Like a real repo, don't commit kai's data dir (capture always
	// excludes it, so a committed .kai/ could never match).
	if err := os.WriteFile(".gitignore", []byte(".kai/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for i, step := range []map[string]string{
		{"main.go": "package main\n"},
		{"main.go": "package main\n\nfunc main() {}\n", "run.sh": "#!/bin/sh\necho hi\n"},
		{"schema.proto": "syntax = \"proto3\";\n"},
	} {
		for name, body := range step {
			if err := os.WriteFile(name, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if i == 1 {
			if err := os.Chmod("run.sh", 0755); err != nil {
				t.Fatal(err)
			}
		}
		gitT(t, "add", "-A")
		gitT(t, "commit", "-qm", "step")
	}
	// Uncommitted edit + untracked file: neither belongs to any commit.
	if err := os.WriteFile("main.go", []byte("package main // WIP\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("secret.env", []byte("TOKEN=x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	reflogBefore := gitT(t, "reflog")

	db, err := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	initMode = true
	defer func() { initMode = false }()
	if err := runGitImport(db); err != nil {
		t.Fatalf("runGitImport: %v", err)
	}

	refs := ref.NewRefManager(db)
	commits := strings.Fields(gitT(t, "rev-list", "HEAD"))
	if len(commits) != 4 {
		t.Fatalf("fixture has %d commits, want 4", len(commits))
	}
	for _, c := range commits {
		r, _ := refs.Get("git." + c[:12])
		if r == nil {
			t.Fatalf("commit %.7s not mapped", c)
		}
		inGit := strings.Fields(gitT(t, "ls-tree", "-r", "--name-only", c))
		edges, err := db.GetEdges(r.TargetID, graph.EdgeHasFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(edges) != len(inGit) {
			t.Errorf("commit %.7s: snapshot has %d files, git tree has %d: %v", c, len(edges), len(inGit), inGit)
		}
		for _, e := range edges {
			node, _ := db.GetNode(e.Dst)
			path, _ := node.Payload["path"].(string)
			digest, _ := node.Payload["digest"].(string)
			got, err := db.ReadObject(digest)
			if err != nil {
				t.Fatalf("commit %.7s %s: %v", c, path, err)
			}
			want, err := exec.Command("git", "show", c+":"+path).Output()
			if err != nil {
				t.Errorf("commit %.7s: snapshot has %s, which is not in the commit", c, path)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("commit %.7s %s: snapshot content differs from the commit", c, path)
			}
			if path == "run.sh" && node.Payload["mode"] != "100755" {
				t.Errorf("commit %.7s run.sh mode = %v, want 100755", c, node.Payload["mode"])
			}
			if path == "schema.proto" && node.Payload["lang"] != "blob" {
				t.Errorf("schema.proto lang = %v, want the directory capture's \"blob\"", node.Payload["lang"])
			}
		}
	}
	head, _ := refs.Get("git.HEAD")
	tip, _ := refs.Get("git." + commits[0][:12])
	if head == nil || tip == nil || !bytes.Equal(head.TargetID, tip.TargetID) {
		t.Fatal("git.HEAD does not point at the tip commit's snapshot")
	}

	if b, _ := os.ReadFile("main.go"); string(b) != "package main // WIP\n" {
		t.Errorf("import changed the uncommitted main.go: %q", b)
	}
	if _, err := os.Stat("secret.env"); err != nil {
		t.Error("import removed the untracked file")
	}
	if br := gitT(t, "rev-parse", "--abbrev-ref", "HEAD"); br == "HEAD" {
		t.Error("import left HEAD detached")
	}
	if gitT(t, "reflog") != reflogBefore {
		t.Error("import moved HEAD (checked out commits)")
	}
}
