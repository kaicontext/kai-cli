package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

// withShipFlags sets the package-level ship flags for one test and puts
// them back after, so the cases below cannot leak into each other.
func withShipFlags(t *testing.T, session, slug, branch, title string) {
	t.Helper()
	old := [4]string{shipSession, shipSlug, shipBranch, shipTitle}
	shipSession, shipSlug, shipBranch, shipTitle = session, slug, branch, title
	t.Cleanup(func() { shipSession, shipSlug, shipBranch, shipTitle = old[0], old[1], old[2], old[3] })
}

// The branch a reviewer sees in GitHub's picker names the change; the id
// after it keeps two sessions with the same title apart. PR #440's branch
// was kai/s-98d60850 — a transport id that said nothing about the work.
func TestResolveShipBranch_NamesTheChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	cases := []struct {
		name, session, slug, branch, title, want string
	}{
		{"title names the branch", "98d60850-dd4e-4de9-a5f6-4e2a75ebbcbb", "", "", "Fix voice-chat parent title resolution", "kai/fix-voice-chat-parent-title-resolution-98d608"},
		{"conventional prefix is kept as words", "98d60850-dd4e", "", "", "fix: remove context percentage from composer", "kai/fix-remove-context-percentage-from-composer-98d608"},
		{"--slug wins over the title", "98d60850-dd4e", "orb readback", "", "Something else entirely", "kai/orb-readback-98d608"},
		{"--branch wins over everything", "98d60850-dd4e", "x", "kai/hand-picked", "y", "kai/hand-picked"},
		{"a placeholder title names nothing", "98d60850-dd4e", "", "", "ship: kai/s-98d60850", "kai/s-98d60850"},
		{"no title, no commits: the bare identity", "98d60850-dd4e", "", "", "", "kai/s-98d60850"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withShipFlags(t, c.session, c.slug, c.branch, c.title)
			got, err := resolveShipBranch(cwd)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("branch = %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveShipBranch_RejectsAnUnusableSlug(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withShipFlags(t, "98d60850", "!!!", "", "")
	if _, err := resolveShipBranch(t.TempDir()); err == nil || !strings.Contains(err.Error(), "--slug") {
		t.Fatalf("err = %v, want a --slug error", err)
	}
}

func TestShipSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix the login redirect":                  "fix-the-login-redirect",
		"  Don't   drop  rows!  ":                 "dont-drop-rows",
		"Add `kai ship --body` (PR descriptions)": "add-kai-ship-body-pr-descriptions",
		"ship: kai/s-98d60850":                    "",
		"SHIP: KAI/whatever":                      "",
		"":                                        "",
		"—":                                       "",
		// Cut on a word boundary at the cap, never mid-word.
		"Investigate why the nightly export job silently drops rows for orgs over the seat cap": "investigate-why-the-nightly-export-job-silently",
		// A single word longer than the cap is cut rather than dropped.
		strings.Repeat("a", 60): strings.Repeat("a", shipBranchSlugMax),
	}
	for in, want := range cases {
		if got := shipSlugify(in); got != want {
			t.Errorf("shipSlugify(%q) = %q, want %q", in, got, want)
		}
		if got := shipSlugify(in); len(got) > shipBranchSlugMax {
			t.Errorf("shipSlugify(%q) is %d chars, over the %d cap", in, len(got), shipBranchSlugMax)
		}
	}
}

func TestShipShortID(t *testing.T) {
	for identity, want := range map[string]string{
		"s-98d60850":   "98d608", // a session: six random hex characters
		"s-98d6":       "98d6",
		"my-workspace": "my-workspace", // a name is not random: kept whole
		"my-workflow":  "my-workflow",
		"s-not-hex":    "s-not-hex",
		"!!!":          "kai",
	} {
		if got := shipShortID(identity); got != want {
			t.Errorf("shipShortID(%q) = %q, want %q", identity, got, want)
		}
	}
}

// A re-ship stays on the checked-out branch only when it is provably this
// session's: the bare identity, or a named branch with this identity's id
// whose tip carries this session's trailer.
func TestShipBranchIsSessions(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("branch", "kai/fix-login-98d608")
	git("checkout", "-q", "kai/fix-login-98d608")
	git("commit", "-q", "--allow-empty", "-m", "Fix login", "-m", "Kai-Session: 98d60850-dd4e")
	git("checkout", "-q", "-b", "kai/other-work-98d608")
	git("commit", "-q", "--allow-empty", "-m", "Other work", "-m", "Kai-Session: 98d608ff-0000")
	// Another session's branch, whose body MENTIONS this session's id in
	// prose: a substring test would take it for this session's own.
	git("checkout", "-q", "-b", "kai/mentions-it-98d608")
	git("commit", "-q", "--allow-empty", "-m", "Undo what Kai-Session: 98d60850-dd4e did", "-m", "Kai-Session: 98d608ff-0000")

	const id, sid = "s-98d60850", "98d60850-dd4e"
	cases := []struct {
		branch, identity, session string
		want                      bool
	}{
		{"kai/s-98d60850", id, sid, true},            // the bare identity
		{"kai/fix-login-98d608", id, sid, true},      // named, and its tip is this session's
		{"kai/other-work-98d608", id, sid, false},    // same six hex, another session's tip
		{"kai/mentions-it-98d608", id, sid, false},   // a body that only mentions the id
		{"kai/fix-login-aaaaaa", id, sid, false},     // another id
		{"feature/fix-login-98d608", id, sid, false}, // not a ship branch
		{"kai/fix-login-98d608", id, "", true},       // no session to check against: the id decides
		{"kai/x-my-workspace", "my-workspace", "", true},
		{"kai/x-my-workflow", "my-workspace", "", false}, // a workspace name is matched whole
		{"kai/anything", "", sid, false},
	}
	for _, c := range cases {
		if got := shipBranchIsSessions(repo, c.branch, c.identity, c.session); got != c.want {
			t.Errorf("shipBranchIsSessions(%q, %q, %q) = %v, want %v", c.branch, c.identity, c.session, got, c.want)
		}
	}
}

// A malformed --session is reported as such, not as a missing identity.
func TestResolveShipBranch_MalformedSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withShipFlags(t, "!!!", "", "", "Fix it")
	_, err := resolveShipBranch(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--session") {
		t.Fatalf("err = %v, want a --session error", err)
	}
}

// In a spawn, with no title, the branch is named from the session's first
// own commit — the part's one-line intent, which does not change between
// re-ships — skipping kai's bookkeeping commits.
func TestResolveShipBranch_FirstCommitSubjectInASpawn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	spawn := filepath.Join(t.TempDir(), "kai-desktop")
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
	git("commit", "-q", "--allow-empty", "-m", "kai spawn from 0123456789ab")
	git("tag", "kai-baseline")
	git("commit", "-q", "--allow-empty", "-m", "kai warm sync")
	// A real change whose subject merely starts with the word "merge".
	git("commit", "-q", "--allow-empty", "-m", "Merge origin/main into kai/s-98d60850")
	git("commit", "-q", "--allow-empty", "-m", "Merge pull request #12 from x/y")
	git("commit", "-q", "--allow-empty", "-m", "Keep the composer readable with pasted text")
	git("commit", "-q", "--allow-empty", "-m", "Address review: test the grip")
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SessionID: "98d60850-dd4e", Durable: true}); err != nil {
		t.Fatal(err)
	}
	withShipFlags(t, "98d60850-dd4e", "", "", "")
	got, err := resolveShipBranch(spawn)
	if err != nil {
		t.Fatal(err)
	}
	if want := "kai/keep-the-composer-readable-with-pasted-text-98d608"; got != want {
		t.Fatalf("branch = %q, want %q", got, want)
	}
	if got := shipFirstCommitSubject(t.TempDir()); got != "" {
		t.Fatalf("outside a spawn the first commit names nothing, got %q", got)
	}
}

// A registered spawn that has not committed past its baseline has no first
// commit to name a branch from — the git walk runs and finds nothing.
func TestShipFirstCommitSubject_SpawnWithoutOwnCommits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	spawn := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", spawn).Run(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", spawn, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", "kai spawn from 0123456789ab")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := spawnpkg.Add(spawnpkg.Entry{Path: spawn, SessionID: "98d60850-dd4e", Durable: true}); err != nil {
		t.Fatal(err)
	}
	if got := shipFirstCommitSubject(spawn); got != "" {
		t.Fatalf("a spawn with only its baseline has no first commit, got %q", got)
	}
	withShipFlags(t, "98d60850-dd4e", "", "", "")
	if got, err := resolveShipBranch(spawn); err != nil || got != "kai/s-98d60850" {
		t.Fatalf("branch = %q, %v; want the bare identity", got, err)
	}
}

func TestShipIsMergeSubject(t *testing.T) {
	for subject, want := range map[string]bool{
		"merge pull request #12 from x/y":             true,
		"merge branch 'main' into feat":               true,
		"merge remote-tracking branch 'origin/main'":  true,
		"merge origin/main into kai/s-98d60850":       true,
		"merge tag 'v1'":                              true,
		"merge the two handlers into one function":    true, // says "into": indistinguishable from a merge
		"merge the two handlers":                      false,
		"merges are not the subject here":             false,
		"keep the composer readable with pasted text": false,
	} {
		if got := shipIsMergeSubject(subject); got != want {
			t.Errorf("shipIsMergeSubject(%q) = %v, want %v", subject, got, want)
		}
	}
}
