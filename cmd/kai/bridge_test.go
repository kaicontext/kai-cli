package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/graph"
	"kai/internal/ref"
)

// ─── Unit tests ────────────────────────────────────────────────────────────

func TestKaiSnapshotTrailerRe_Matches(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"bare trailer", "subject\n\nKai-Snapshot: abc123", true},
		{"trailer with siblings", "subject\n\nKai-Snapshot: deadbeef\nKai-Assert: tests-pass", true},
		{"trailer with extra spaces", "subject\n\nKai-Snapshot:   abc123   \n", true},
		{"no trailer", "just a plain commit", false},
		{"mention in body is not a trailer line", "I wrote Kai-Snapshot: foo in my prose", false},
		{"wrong case", "subject\n\nkai-snapshot: abc123", false},
		{"non-hex value rejected", "subject\n\nKai-Snapshot: not-hex-here", false},
		{"empty message", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kaiSnapshotTrailerRe.MatchString(tc.body)
			if got != tc.want {
				t.Fatalf("match=%v, want %v; body=%q", got, tc.want, tc.body)
			}
		})
	}
}

func TestBridgeEnabled_ReturnsFalseWhenMissing(t *testing.T) {
	chdirTemp(t)
	if bridgeEnabled() {
		t.Fatal("bridgeEnabled() true with no .kai dir")
	}
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	if bridgeEnabled() {
		t.Fatal("bridgeEnabled() true without sentinel")
	}
}

func TestBridgeEnabled_ReturnsTrueWhenSentinelPresent(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".kai", "bridge-enabled"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !bridgeEnabled() {
		t.Fatal("bridgeEnabled() false after writing sentinel")
	}
}

func TestHookScripts_AllContainReEntrancyGuard(t *testing.T) {
	scripts := map[string]string{
		"preCommit":    preCommitHookScript,
		"prePush":      prePushHookScript,
		"postCommit":   postCommitHookScript,
		"postMerge":    postMergeHookScript,
		"postCheckout": postCheckoutHookScript,
	}
	for name, s := range scripts {
		if !strings.Contains(s, `"${KAI_BRIDGE_INPROGRESS:-}" = "1"`) {
			t.Errorf("%s hook is missing KAI_BRIDGE_INPROGRESS guard", name)
		}
		if !strings.Contains(s, "exit 0") {
			t.Errorf("%s hook never exits 0 (must be best-effort)", name)
		}
	}
}

func TestHookScripts_AllTaggedWithCurrentVersion(t *testing.T) {
	tag := kaiHookMarker + " " + kaiHookVersion
	for name, s := range map[string]string{
		"preCommit":    preCommitHookScript,
		"prePush":      prePushHookScript,
		"postCommit":   postCommitHookScript,
		"postMerge":    postMergeHookScript,
		"postCheckout": postCheckoutHookScript,
	} {
		if !strings.Contains(s, tag) {
			t.Errorf("%s hook missing version tag %q", name, tag)
		}
	}
}

func TestPostCommitHookScript_InvokesBridgeImport(t *testing.T) {
	if !strings.Contains(postCommitHookScript, "kai bridge import") {
		t.Fatal("post-commit hook does not call 'kai bridge import'")
	}
	if !strings.Contains(postCommitHookScript, "git rev-parse HEAD") {
		t.Fatal("post-commit hook does not read HEAD sha")
	}
}

func TestPostMergeHookScript_IteratesOrigHeadRange(t *testing.T) {
	if !strings.Contains(postMergeHookScript, "ORIG_HEAD..HEAD") {
		t.Fatal("post-merge hook does not walk ORIG_HEAD..HEAD")
	}
	if !strings.Contains(postMergeHookScript, "kai bridge import") {
		t.Fatal("post-merge hook does not call 'kai bridge import'")
	}
}

func TestPostCheckoutHookScript_OnlyActsOnBranchSwitch(t *testing.T) {
	if !strings.Contains(postCheckoutHookScript, `"$3" != "1"`) {
		t.Fatal("post-checkout hook does not short-circuit on file checkouts")
	}
	if !strings.Contains(postCheckoutHookScript, "kai bridge import") {
		t.Fatal("post-checkout hook does not call 'kai bridge import'")
	}
}

func TestHookInstall_InstallsAllThreeBridgeHooksWhenEnabled(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(".git", "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".kai", "bridge-enabled"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	initMode = true
	defer func() { initMode = false }()
	if err := runHookInstall(nil, nil); err != nil {
		t.Fatalf("hook install: %v", err)
	}
	for _, name := range []string{"post-commit", "post-merge", "post-checkout"} {
		data, err := os.ReadFile(filepath.Join(".git", "hooks", name))
		if err != nil {
			t.Fatalf("%s hook missing: %v", name, err)
		}
		if !strings.Contains(string(data), kaiHookMarker) {
			t.Errorf("%s hook is not tagged as kai-managed", name)
		}
	}
}

func TestPostCommitHookScript_RespectsMissingKaiDir(t *testing.T) {
	if !strings.Contains(postCommitHookScript, "[ ! -d .kai ]") {
		t.Fatal("post-commit hook does not no-op when .kai is missing")
	}
}

// ─── Integration tests ─────────────────────────────────────────────────────

// setupBridgeRepo sets up a tmpdir as the current working directory with:
//   - a git repo containing one commit
//   - a .kai directory marked as bridge-enabled
//
// runBridgeImport can be called directly against it; no 'kai init' is needed
// (which avoids network/auth in tests).
func setupBridgeRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	chdirTemp(t)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "bridge-test@example.com")
	run("git", "config", "user.name", "Bridge Test")
	run("git", "config", "commit.gpgsign", "false")

	if err := os.WriteFile("hello.txt", []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "initial")

	if err := os.MkdirAll(filepath.Join(".kai", "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".kai", "bridge-enabled"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeImport_NoOpWhenBridgeDisabled(t *testing.T) {
	setupBridgeRepo(t)
	// Remove sentinel: disabled
	if err := os.Remove(filepath.Join(".kai", "bridge-enabled")); err != nil {
		t.Fatal(err)
	}
	sha := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import returned error: %v", err)
	}
	// Opening the DB would have initialized it; disabled path should not
	// create a DB file or any refs.
	if hasAnyGitRef(t) {
		t.Fatal("bridge import wrote git.* refs while disabled")
	}
}

func TestBridgeImport_CreatesGitRef(t *testing.T) {
	setupBridgeRepo(t)
	sha := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import: %v", err)
	}
	refs := listGitRefs(t)
	short := sha[:12]
	if _, ok := refs["git."+short]; !ok {
		t.Fatalf("expected git.%s ref; got %v", short, keys(refs))
	}
	if _, ok := refs["git.HEAD"]; !ok {
		t.Fatalf("expected git.HEAD ref; got %v", keys(refs))
	}
}

func TestBridgeImport_IsIdempotent(t *testing.T) {
	setupBridgeRepo(t)
	sha := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import 1: %v", err)
	}
	before := listGitRefs(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import 2: %v", err)
	}
	after := listGitRefs(t)
	if len(before) != len(after) {
		t.Fatalf("ref count changed: before=%d after=%d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("ref %s changed target: before=%s after=%s", k, v, after[k])
		}
	}
}

func TestBridgeImport_SkipsCommitWithKaiSnapshotTrailer(t *testing.T) {
	setupBridgeRepo(t)
	// Make a commit whose message carries the trailer.
	if err := os.WriteFile("hello.txt", []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "fake milestone\n\nKai-Snapshot: deadbeef1234\nKai-Assert: tests-pass")

	sha := gitHeadSHA(t)
	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import: %v", err)
	}
	refs := listGitRefs(t)
	short := sha[:12]
	if _, ok := refs["git."+short]; ok {
		t.Fatalf("kai-authored commit %s was imported (should be skipped)", short)
	}
}

func TestBridgeImport_EmptySHA(t *testing.T) {
	setupBridgeRepo(t)
	if err := runBridgeImport(nil, []string{""}); err != nil {
		t.Fatalf("import with empty sha returned error: %v", err)
	}
	if hasAnyGitRef(t) {
		t.Fatal("wrote a ref for empty sha")
	}
}

func TestBridgeImport_WhitespaceSHA(t *testing.T) {
	setupBridgeRepo(t)
	if err := runBridgeImport(nil, []string{"   \t  "}); err != nil {
		t.Fatalf("import with whitespace sha: %v", err)
	}
	if hasAnyGitRef(t) {
		t.Fatal("wrote a ref for whitespace-only sha")
	}
}

func TestBridgeImport_HandlesUnknownSHA(t *testing.T) {
	setupBridgeRepo(t)
	// Not a real sha — git rev-parse will fail. Must be a no-op, not an error.
	if err := runBridgeImport(nil, []string{"0000000000000000000000000000000000000000"}); err != nil {
		t.Fatalf("import returned error on unknown sha: %v", err)
	}
	if hasAnyGitRef(t) {
		t.Fatal("wrote a ref for a bogus sha")
	}
}

func TestHookInstall_SkipsPostCommitWhenBridgeDisabled(t *testing.T) {
	chdirTemp(t)
	// git repo present, but no bridge sentinel.
	if err := os.MkdirAll(filepath.Join(".git", "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	initMode = true
	defer func() { initMode = false }()
	if err := runHookInstall(nil, nil); err != nil {
		t.Fatalf("hook install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".git", "hooks", "post-commit")); err == nil {
		t.Fatal("post-commit hook was installed even though bridge is disabled")
	}
	// pre-commit should still be installed.
	if _, err := os.Stat(filepath.Join(".git", "hooks", "pre-commit")); err != nil {
		t.Fatal("pre-commit hook was not installed")
	}
}

func TestHookInstall_InstallsPostCommitWhenBridgeEnabled(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(".git", "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".kai", "bridge-enabled"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	initMode = true
	defer func() { initMode = false }()
	if err := runHookInstall(nil, nil); err != nil {
		t.Fatalf("hook install: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(".git", "hooks", "post-commit"))
	if err != nil {
		t.Fatalf("post-commit hook missing: %v", err)
	}
	if !strings.Contains(string(data), kaiHookMarker) {
		t.Fatal("installed post-commit hook is not tagged as kai-managed")
	}
}

func TestHookInstall_PreservesForeignPostCommitHook(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(".git", "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".kai", "bridge-enabled"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\n# foreign hook — not ours\nexit 0\n"
	hookPath := filepath.Join(".git", "hooks", "post-commit")
	if err := os.WriteFile(hookPath, []byte(foreign), 0755); err != nil {
		t.Fatal(err)
	}
	initMode = true
	defer func() { initMode = false }()
	if err := runHookInstall(nil, nil); err != nil {
		t.Fatalf("hook install: %v", err)
	}
	data, _ := os.ReadFile(hookPath)
	if string(data) != foreign {
		t.Fatal("foreign post-commit hook was overwritten")
	}
}

func TestBridgeImport_AcceptsShortSHA(t *testing.T) {
	setupBridgeRepo(t)
	sha := gitHeadSHA(t)
	short := sha[:8]
	if err := runBridgeImport(nil, []string{short}); err != nil {
		t.Fatalf("import with short sha: %v", err)
	}
	refs := listGitRefs(t)
	if _, ok := refs["git."+sha[:12]]; !ok {
		t.Fatalf("short-sha input did not resolve to full sha; refs=%v", keys(refs))
	}
}

// ─── Milestone → git commit ────────────────────────────────────────────────

func TestBridgeMilestone_CreatesCommitWithLabelAndTrailers(t *testing.T) {
	setupBridgeRepo(t)
	captureForTest(t)

	milestoneLabel = "billing feature complete"
	milestoneAssert = "tests-pass"
	milestonePlanHash = "plan123abc"
	defer func() { milestoneLabel, milestoneAssert, milestonePlanHash = "", "", "" }()

	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}

	out, err := exec.Command("git", "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, "billing feature complete") {
		t.Errorf("subject missing from commit: %q", body)
	}
	if !strings.Contains(body, "Kai-Assert: tests-pass") {
		t.Errorf("assert trailer missing: %q", body)
	}
	if !strings.Contains(body, "Kai-Plan-Hash: plan123abc") {
		t.Errorf("plan-hash trailer missing: %q", body)
	}
	if !kaiSnapshotTrailerRe.MatchString(body) {
		t.Errorf("Kai-Snapshot trailer missing or malformed: %q", body)
	}
}

func TestBridgeMilestone_TrailerMatchesCommittedState(t *testing.T) {
	// Regression: the Kai-Snapshot trailer must reference a snapshot of the
	// code being committed, not the snapshot that existed *before* the
	// milestone change. Without an explicit capture inside runBridgeMilestone,
	// the trailer would reference the stale snap.latest (pre-commit hook
	// is silenced by KAI_BRIDGE_INPROGRESS so it can't capture for us).
	setupBridgeRepo(t)
	captureForTest(t)

	// Read snap.latest BEFORE the milestone change.
	db1, _ := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	before, _ := ref.NewRefManager(db1).Get("snap.latest")
	db1.Close()

	// Change the tree, then fire the milestone.
	if err := os.WriteFile("hello.txt", []byte("new content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	milestoneLabel = "new content milestone"
	defer func() { milestoneLabel = "" }()
	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}

	// Extract the Kai-Snapshot trailer value from the new commit.
	out, _ := exec.Command("git", "log", "-1", "--format=%B").Output()
	m := kaiSnapshotTrailerRe.FindStringSubmatch(string(out))
	if len(m) < 2 {
		t.Fatalf("Kai-Snapshot trailer missing from commit: %q", out)
	}
	trailerHex := m[1]
	if before != nil && trailerHex == hexStr(before.TargetID) {
		t.Fatalf("trailer references pre-milestone snap.latest (%s); should reference the post-change snapshot", trailerHex)
	}
	// Double-check: the trailer hex equals the current snap.latest.
	db2, _ := graph.Open(filepath.Join(".kai", "db.sqlite"), filepath.Join(".kai", "objects"))
	after, _ := ref.NewRefManager(db2).Get("snap.latest")
	db2.Close()
	if after == nil || trailerHex != hexStr(after.TargetID) {
		t.Fatalf("trailer %s does not match current snap.latest %s", trailerHex, hexStr(after.TargetID))
	}
}

func TestBridgeMilestone_OmitsEmptyTrailers(t *testing.T) {
	setupBridgeRepo(t)
	milestoneLabel = "just a label"
	defer func() { milestoneLabel = "" }()

	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}
	out, _ := exec.Command("git", "log", "-1", "--format=%B").Output()
	body := string(out)
	if strings.Contains(body, "Kai-Assert:") {
		t.Errorf("Kai-Assert should be omitted: %q", body)
	}
	if strings.Contains(body, "Kai-Plan-Hash:") {
		t.Errorf("Kai-Plan-Hash should be omitted: %q", body)
	}
}

func TestBridgeMilestone_RequiresLabel(t *testing.T) {
	setupBridgeRepo(t)
	milestoneLabel = "  "
	defer func() { milestoneLabel = "" }()

	err := runBridgeMilestone(nil, nil)
	if err == nil {
		t.Fatal("expected error for blank label, got nil")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Errorf("error should mention label, got: %v", err)
	}
}

func TestBridgeMilestone_NoOpWhenBridgeDisabled(t *testing.T) {
	setupBridgeRepo(t)
	if err := os.Remove(filepath.Join(".kai", "bridge-enabled")); err != nil {
		t.Fatal(err)
	}
	shaBefore := gitHeadSHA(t)

	milestoneLabel = "should not commit"
	defer func() { milestoneLabel = "" }()

	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}
	shaAfter := gitHeadSHA(t)
	if shaBefore != shaAfter {
		t.Fatal("milestone created a commit even though bridge is disabled")
	}
}

func TestBridgeMilestone_AllowsEmptyCommit(t *testing.T) {
	setupBridgeRepo(t)
	// No file changes between setup and milestone — the only way this should
	// produce a commit is --allow-empty.
	shaBefore := gitHeadSHA(t)
	milestoneLabel = "trust assertion, no code changes"
	defer func() { milestoneLabel = "" }()

	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}
	shaAfter := gitHeadSHA(t)
	if shaBefore == shaAfter {
		t.Fatal("milestone did not produce a commit on clean tree (expected --allow-empty)")
	}
}

func TestBridgeMilestone_MilestoneCommitIsSkippedByImport(t *testing.T) {
	// Full round-trip: create a milestone commit, then try to import it.
	// The Kai-Snapshot trailer on the commit must cause import to skip,
	// otherwise we'd loop.
	setupBridgeRepo(t)
	captureForTest(t)
	milestoneLabel = "round trip"
	defer func() { milestoneLabel = "" }()

	if err := runBridgeMilestone(nil, nil); err != nil {
		t.Fatalf("milestone: %v", err)
	}
	sha := gitHeadSHA(t)

	// Verify the commit message has the trailer (sanity check).
	out, _ := exec.Command("git", "log", "-1", "--format=%B").Output()
	if !kaiSnapshotTrailerRe.MatchString(string(out)) {
		t.Fatalf("milestone commit missing Kai-Snapshot trailer: %q", out)
	}

	if err := runBridgeImport(nil, []string{sha}); err != nil {
		t.Fatalf("import: %v", err)
	}
	refs := listGitRefs(t)
	short := sha[:12]
	if _, ok := refs["git."+short]; ok {
		t.Fatal("milestone commit was imported back into kai — would cause a loop")
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// captureForTest runs kai capture in the current dir so snap.latest exists
// and milestone commits can write a Kai-Snapshot: trailer.
func captureForTest(t *testing.T) {
	t.Helper()
	initMode = true
	defer func() { initMode = false }()
	if err := runCapture(nil, nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
}

func chdirTemp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return tmp
}

func gitHeadSHA(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// listGitRefs returns all refs whose name starts with "git." as a
// name→hex-target map.
func listGitRefs(t *testing.T) map[string]string {
	t.Helper()
	dbPath := filepath.Join(".kai", "db.sqlite")
	objPath := filepath.Join(".kai", "objects")
	if _, err := os.Stat(dbPath); err != nil {
		// DB may not exist when import was a no-op.
		return map[string]string{}
	}
	db, err := graph.Open(dbPath, objPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	all, err := ref.NewRefManager(db).List(nil)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	out := map[string]string{}
	for _, r := range all {
		if strings.HasPrefix(r.Name, "git.") {
			out[r.Name] = hexStr(r.TargetID)
		}
	}
	return out
}

func hasAnyGitRef(t *testing.T) bool {
	return len(listGitRefs(t)) > 0
}

func hexStr(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = digits[x>>4]
		out[i*2+1] = digits[x&0x0f]
	}
	return string(out)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
