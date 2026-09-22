package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// What the agent wrote leads the PR; the provenance folds away beneath it.
func TestShipPRBody_AuthoredDescriptionLeads(t *testing.T) {
	authored := "## What this does\n\nStops the composer clipping pasted text.\n\n## How it was verified\n\n`node composer-resize.test.js`"
	body := shipPRBody(shipBodyInput{
		Branch: "kai/fix-composer-98d608", SessionID: "98d60850-dd4e", SnapHex: "abc123",
		Title: "Fix the composer", Authored: authored,
		Files:       []shipFileStat{{Path: "frontend/dist/app.js", Added: 40, Removed: 3, Known: true}},
		KnownIssues: "\n### Known issues\n\n- one\n",
	})
	if !strings.HasPrefix(body, shipSummaryMarker+"\n"+authored) {
		t.Fatalf("the authored description must open the body, after the summary marker:\n%s", body)
	}
	details := body[strings.Index(body, "<details><summary>Kai details</summary>"):]
	for _, want := range []string{"| Branch | `kai/fix-composer-98d608` |", "| Session | `98d60850-dd4e` |", "| Snapshot | `abc123` |", "- `frontend/dist/app.js` (+40 −3)"} {
		if !strings.Contains(details, want) {
			t.Errorf("Kai details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(body[:strings.Index(body, "<details>")], "## Files") {
		t.Error("beside an authored description the file list folds into Kai details")
	}
	if !strings.Contains(body, "### Known issues") || !strings.HasSuffix(body, "<!-- kai-ship -->\n") {
		t.Errorf("known issues and the kai-ship marker must survive:\n%s", body)
	}
}

// Without an authored description the body is built from what the session
// recorded: the title as a sentence, the commits' explanations, the steps,
// and the files with their counts.
func TestShipPRBody_GeneratedFromTheSession(t *testing.T) {
	body := shipPRBody(shipBodyInput{
		Branch: "kai/b-1", Title: "Keep the composer readable with pasted text",
		Commits: []shipCommitNote{
			{Subject: "Keep the composer readable with pasted text", Body: "The capsule becomes a card past one line."},
			{Subject: "Test the resize grip", Body: ""},
		},
		Files: []shipFileStat{
			{Path: "frontend/dist/app.js", Added: 120, Removed: 4, Known: true},
			{Path: "frontend/dist/logo.png"},
		},
	})
	for _, want := range []string{
		shipSummaryMarker,
		"## What this does\n\nKeep the composer readable with pasted text.\n\nThe capsule becomes a card past one line.",
		"## Changes\n\n- Test the resize grip\n",
		"## Files\n\n**2 files** · +120 −4\n\n- `frontend/dist/app.js` (+120 −4)\n- `frontend/dist/logo.png`\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "- Keep the composer readable") {
		t.Error("a step that repeats the title is not listed again")
	}
}

// PR #440's description: its only "summary" was the placeholder title, over
// a table. A placeholder names nothing; with nothing else recorded, the body
// says what the change touched instead.
func TestShipPRBody_PlaceholderTitleNamesNothing(t *testing.T) {
	files := []shipFileStat{
		{Path: "frontend/dist/app.js", Added: 3, Removed: 1, Known: true},
		{Path: "frontend/dist/style.css", Added: 2, Known: true},
		{Path: "frontend/dist/voice-tasks.js", Removed: 5, Known: true},
	}
	body := shipPRBody(shipBodyInput{Branch: "kai/s-98d60850", Title: "ship: kai/s-98d60850", Files: files})
	if strings.Contains(body[:strings.Index(body, "<details>")], "ship: kai/") {
		t.Errorf("the placeholder title leaked into the description:\n%s", body)
	}
	if !strings.Contains(body, "Changes 3 files under `frontend/dist`.") {
		t.Errorf("without a description the body should say what it touched:\n%s", body)
	}
	if !strings.Contains(body, "**3 files** · +5 −6") {
		t.Errorf("the file total is missing:\n%s", body)
	}
}

func TestShipFilesSection_CapsTheList(t *testing.T) {
	var files []shipFileStat
	for i := 0; i < shipBodyFileCap+7; i++ {
		files = append(files, shipFileStat{Path: filepath.Join("pkg", string(rune('a'+i%26))+strings.Repeat("x", i)+".go"), Added: 1, Known: true})
	}
	s := shipFilesSection(files)
	if got := strings.Count(s, "\n- `"); got != shipBodyFileCap {
		t.Errorf("listed %d files, want %d", got, shipBodyFileCap)
	}
	if !strings.Contains(s, "- …and 7 more") {
		t.Errorf("the rest must be counted:\n%s", s)
	}
	if shipFilesSection(nil) != "" {
		t.Error("no files, no section")
	}
}

func TestShipScopeSentence(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{nil, "No files changed."},
		{[]string{"a/b/c.go"}, "Changes 1 file under `a/b`."},
		{[]string{"a/b/c.go", "a/b/d/e.go"}, "Changes 2 files under `a/b`."},
		{[]string{"a/b/c.go", "a/x/e.go"}, "Changes 2 files under `a`."},
		{[]string{"a/c.go", "b/e.go"}, "Changes 2 files."},
		{[]string{"top.go", "a/b.go"}, "Changes 2 files."},
	}
	for _, c := range cases {
		var files []shipFileStat
		for _, p := range c.paths {
			files = append(files, shipFileStat{Path: p})
		}
		if got := shipScopeSentence(files); got != c.want {
			t.Errorf("%v: got %q, want %q", c.paths, got, c.want)
		}
	}
}

func TestShipAuthoredBody(t *testing.T) {
	if got, err := shipAuthoredBody("  inline  ", "", nil); err != nil || got != "inline" {
		t.Fatalf("--body: %q %v", got, err)
	}
	f := filepath.Join(t.TempDir(), "desc.md")
	if err := os.WriteFile(f, []byte("## What this does\n\nfrom a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := shipAuthoredBody("", f, nil); err != nil || got != "## What this does\n\nfrom a file" {
		t.Fatalf("--body-file: %q %v", got, err)
	}
	if got, err := shipAuthoredBody("", "-", strings.NewReader("from stdin\n")); err != nil || got != "from stdin" {
		t.Fatalf("--body-file -: %q %v", got, err)
	}
	if _, err := shipAuthoredBody("a", f, nil); err == nil {
		t.Fatal("--body with --body-file must be refused")
	}
	if _, err := shipAuthoredBody("", filepath.Join(t.TempDir(), "missing.md"), nil); err == nil || !strings.Contains(err.Error(), "--body-file") {
		t.Fatalf("a missing file must name the flag, got %v", err)
	}
	if got, err := shipAuthoredBody("", "", nil); err != nil || got != "" {
		t.Fatalf("neither flag: %q %v", got, err)
	}
}

func TestShipCommitProse_DropsTrailers(t *testing.T) {
	body := "The capsule becomes a card.\n\nVerified with node.\n\nKai-Session: abc\nKai-Snapshot: def\nCo-authored-by: x <x@y>\n"
	if got := shipCommitProse(body); got != "The capsule becomes a card.\n\nVerified with node." {
		t.Fatalf("got %q", got)
	}
	if got := shipCommitProse("Fixes: the thing\n"); got != "Fixes: the thing" {
		t.Fatalf("a sentence with a colon is prose, not a trailer: %q", got)
	}
}

// The spawn's own commits, oldest first, without kai's bookkeeping and with
// their trailers dropped; and line counts for tracked and brand-new files.
func TestShipSessionCommitsAndFileStats(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	spawn := filepath.Join(t.TempDir(), "repo")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", spawn, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := exec.Command("git", "init", "-q", spawn).Run(); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(spawn, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(spawn, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app.js", "a\nb\nc\n")
	git("add", "-A")
	git("commit", "-q", "-m", "kai spawn from 0123456789ab")
	git("tag", "kai-baseline")
	git("commit", "-q", "--allow-empty", "-m", "kai warm sync")
	write("app.js", "a\nB\nc\nd\n")
	git("commit", "-q", "-am", "Grow the composer with its text", "-m", "flex: none, so the inline height applies.", "-m", "Kai-Snapshot: abc")
	git("commit", "-q", "--allow-empty", "-m", "Merge main into the session")
	write("new/panel.js", "one\ntwo")
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SessionID: "98d60850-dd4e", Durable: true}); err != nil {
		t.Fatal(err)
	}

	commits := shipSessionCommits(spawn)
	if len(commits) != 1 || commits[0].Subject != "Grow the composer with its text" ||
		commits[0].Body != "flex: none, so the inline height applies." {
		t.Fatalf("session commits = %+v", commits)
	}
	if got := shipFirstCommitSubject(spawn); got != "Grow the composer with its text" {
		t.Fatalf("first commit subject = %q", got)
	}

	stats := shipFileStats(spawn, shipStatsBase(spawn), []string{"new/panel.js", "app.js"}, false)
	want := []shipFileStat{
		{Path: "app.js", Added: 2, Removed: 1, Known: true},
		{Path: "new/panel.js", Added: 2, Known: true},
	}
	if len(stats) != 2 || stats[0] != want[0] || stats[1] != want[1] {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := shipSessionCommits(t.TempDir()); got != nil {
		t.Fatalf("outside a spawn there are no session commits, got %+v", got)
	}
}

// A local ship with no title and no commits is named by what it touched,
// not "ship: kai/<branch>" — the same shape the server uses.
func TestShipTitleFromFiles(t *testing.T) {
	st := func(paths ...string) []shipFileStat {
		var out []shipFileStat
		for _, p := range paths {
			out = append(out, shipFileStat{Path: p})
		}
		return out
	}
	cases := []struct {
		files []shipFileStat
		want  string
	}{
		{st("frontend/dist/app.js", "frontend/dist/style.css", "frontend/dist/voice-tasks.js"), "frontend/dist: update app, style and voice-tasks"},
		{st("frontend/dist/app.js", "frontend/dist/app.test.js"), "frontend/dist: update app"},
		{st("ship.go"), "Update ship"},
		{st("a/x.go", "b/y.go"), "Update x and y"},
		{st("a/1.go", "a/2.go", "a/3.go", "a/4.go", "a/5.go"), "a: update 1, 2, 3 and 2 more"},
		{st("x_test.go"), "Update x_test"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := shipTitleFromFiles(c.files); got != c.want {
			t.Errorf("shipTitleFromFiles(%v) = %q, want %q", c.files, got, c.want)
		}
	}
	long := st("some/really/deeply/nested/directory/structure/that/goes/on/alpha.go", "some/really/deeply/nested/directory/structure/that/goes/on/beta.go")
	if got := shipTitleFromFiles(long); got != "Update 2 files" {
		t.Errorf("an over-long title falls back to a count, got %q", got)
	}
}
