package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/projects"
)

// fileToolsForWS is a tiny test helper that adapts existing
// single-workspace tests to the post-multi-root FileTools API. The
// workspace path becomes a single-project synthetic Set.
func fileToolsForWS(ws string) *FileTools {
	return &FileTools{Set: projects.Single(ws)}
}

func TestView_ReadsFileWithLineNumbers(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).View()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "view",
		Input: `{"file_path":"hello.txt"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	for _, want := range []string{"1: alpha", "2: beta", "3: gamma"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, resp.Content)
		}
	}
}

func TestView_FileNotFound(t *testing.T) {
	tool := fileToolsForWS(t.TempDir()).View()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "view",
		Input: `{"file_path":"missing.txt"}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "not found") {
		t.Errorf("expected file-not-found error, got: %+v", resp)
	}
}

func TestWrite_CreatesFileAndFiresHook(t *testing.T) {
	ws := t.TempDir()
	var hookPath, hookOp string
	hookCalled := 0
	tool := (&FileTools{
		Set: projects.Single(ws),
		OnChange: func(p, op string) {
			hookCalled++
			hookPath, hookOp = p, op
		},
	}).Write()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "write",
		Input: `{"file_path":"sub/dir/new.txt","content":"hi"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	body, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "new.txt"))
	if err != nil || string(body) != "hi" {
		t.Errorf("file not written correctly: err=%v body=%q", err, body)
	}
	if hookCalled != 1 || hookPath != "sub/dir/new.txt" || hookOp != "created" {
		t.Errorf("hook misfired: calls=%d path=%q op=%q", hookCalled, hookPath, hookOp)
	}
}

func TestWrite_OverwriteFiresModified(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var op string
	tool := (&FileTools{
		Set:      projects.Single(ws),
		OnChange: func(_, o string) { op = o },
	}).Write()
	tool.Run(context.Background(), ToolCall{
		Name:  "write",
		Input: `{"file_path":"x.txt","content":"new"}`,
	})
	if op != "modified" {
		t.Errorf("expected 'modified', got %q", op)
	}
}

func TestEdit_ReplaceUniqueMatch(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "edit",
		Input: `{"file_path":"x.txt","old_string":"world","new_string":"there"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "x.txt"))
	if string(body) != "hello there" {
		t.Errorf("edit failed: %q", string(body))
	}
}

func TestEdit_RefusesAmbiguousMatch(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "edit",
		Input: `{"file_path":"x.txt","old_string":"a","new_string":"b"}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "appears 3 times") {
		t.Errorf("expected ambiguous-match error, got: %+v", resp)
	}
}

func TestEdit_ReplaceAll(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "edit",
		Input: `{"file_path":"x.txt","old_string":"a","new_string":"b","replace_all":true}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "x.txt"))
	if string(body) != "b b b" {
		t.Errorf("replace_all failed: %q", string(body))
	}
}

// TestEdit_WhitespaceTolerantMatch: an old_string that is correct
// except for indentation (spaces vs the file's tabs) used to fail
// the exact match and send cheap models into a retry loop. The
// whitespace-tolerant fallback now rescues it.
func TestEdit_WhitespaceTolerantMatch(t *testing.T) {
	ws := t.TempDir()
	content := "func foo() {\n\tx := 1\n\ty := 2\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	in, _ := json.Marshal(map[string]string{
		"file_path":  "x.go",
		"old_string": "    x := 1\n    y := 2", // 4 spaces, file uses tabs
		"new_string": "\tx := 10\n\ty := 20",
	})
	resp, _ := tool.Run(context.Background(), ToolCall{Name: "edit", Input: string(in)})
	if resp.IsError {
		t.Fatalf("whitespace-tolerant edit should succeed, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "whitespace-insensitively") {
		t.Errorf("expected the fuzzy-match note, got: %s", resp.Content)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "x.go"))
	if want := "func foo() {\n\tx := 10\n\ty := 20\n}\n"; string(body) != want {
		t.Errorf("got %q, want %q", string(body), want)
	}
}

// TestEdit_FuzzyAmbiguousMatch: when the whitespace-tolerant match
// hits more than one block, edit errors rather than guessing.
func TestEdit_FuzzyAmbiguousMatch(t *testing.T) {
	ws := t.TempDir()
	content := "a {\n\tp := 1\n}\nb {\n\tp := 1\n}\n"
	if err := os.WriteFile(filepath.Join(ws, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	in, _ := json.Marshal(map[string]string{
		"file_path":  "x.go",
		"old_string": "  p := 1", // spaces, ambiguous after trimming
		"new_string": "  p := 2",
	})
	resp, _ := tool.Run(context.Background(), ToolCall{Name: "edit", Input: string(in)})
	if !resp.IsError || !strings.Contains(resp.Content, "matched 2 blocks") {
		t.Errorf("expected ambiguous fuzzy-match error, got: %+v", resp)
	}
}

// TestEdit_FuzzyNotFound: no exact and no fuzzy match still reports
// the plain not-found error.
func TestEdit_FuzzyNotFound(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := fileToolsForWS(ws).Edit()
	in, _ := json.Marshal(map[string]string{
		"file_path":  "x.go",
		"old_string": "  totally absent line",
		"new_string": "whatever",
	})
	resp, _ := tool.Run(context.Background(), ToolCall{Name: "edit", Input: string(in)})
	if !resp.IsError || !strings.Contains(resp.Content, "not found") {
		t.Errorf("expected not-found error, got: %+v", resp)
	}
}

func TestFuzzyLineMatch(t *testing.T) {
	src := "alpha\n\tbeta\n\tgamma\ndelta\n"
	start, end, count := fuzzyLineMatch(src, "  beta\n  gamma")
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if got := src[start:end]; got != "\tbeta\n\tgamma" {
		t.Errorf("span = %q, want %q", got, "\tbeta\n\tgamma")
	}
}

// TestFileTools_ReadOnlyOmitsWriteAndEdit: ReadOnly mode should
// register only the view tool. Used by the chat-fallback path so
// "what's in this dir" answers can run view+bash without the
// possibility of an accidental write or edit.
func TestFileTools_ReadOnlyOmitsWriteAndEdit(t *testing.T) {
	rw := (&FileTools{Set: projects.Single(t.TempDir()), ReadOnly: false}).All()
	if len(rw) != 3 {
		t.Errorf("read-write should expose 3 tools, got %d", len(rw))
	}

	ro := (&FileTools{Set: projects.Single(t.TempDir()), ReadOnly: true}).All()
	if len(ro) != 1 {
		t.Fatalf("read-only should expose 1 tool, got %d", len(ro))
	}
	if ro[0].Info().Name != "view" {
		t.Errorf("read-only should expose view, got %q", ro[0].Info().Name)
	}
}

func TestResolveInSet_RejectsEscape(t *testing.T) {
	set := projects.Single("/tmp/work")
	_, _, err := resolveInSet(set, "../../../etc/passwd")
	if err == nil {
		t.Error("expected escape error")
	}
}

func TestResolveInSet_AbsoluteOutsideRefused(t *testing.T) {
	set := projects.Single("/tmp/work")
	_, _, err := resolveInSet(set, "/etc/passwd")
	if err == nil {
		t.Error("expected escape error for absolute path outside workspace")
	}
}

// TestWrite_FiresBroadcast verifies the live-sync hook fires after a
// successful write with the right digest + base64 payload. The
// orchestrator wires this to remote.SyncPushFile in production.
func TestWrite_FiresBroadcast(t *testing.T) {
	ws := t.TempDir()
	var got struct {
		path, digest, b64 string
	}
	calls := 0
	tool := (&FileTools{
		Set: projects.Single(ws),
		OnBroadcast: func(p, d, b string) {
			calls++
			got.path, got.digest, got.b64 = p, d, b
		},
	}).Write()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "write",
		Input: `{"file_path":"x.txt","content":"hello"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	if calls != 1 {
		t.Errorf("expected 1 broadcast, got %d", calls)
	}
	if got.path != "x.txt" {
		t.Errorf("path: %q", got.path)
	}
	// Digest is hex sha256 of "hello"
	if got.digest != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("digest mismatch: %s", got.digest)
	}
	// Base64 of "hello" is aGVsbG8=
	if got.b64 != "aGVsbG8=" {
		t.Errorf("base64 mismatch: %q", got.b64)
	}
}

// TestEdit_FiresBroadcast: same shape, but for the edit path. The
// digest must reflect the post-edit content, not the original.
func TestEdit_FiresBroadcast(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotB64 string
	tool := (&FileTools{
		Set:         projects.Single(ws),
		OnBroadcast: func(_, _, b string) { gotB64 = b },
	}).Edit()
	resp, _ := tool.Run(context.Background(), ToolCall{
		Name:  "edit",
		Input: `{"file_path":"x.txt","old_string":"world","new_string":"there"}`,
	})
	if resp.IsError {
		t.Fatalf("unexpected error: %s", resp.Content)
	}
	// Base64 of "hello there" is aGVsbG8gdGhlcmU=
	if gotB64 != "aGVsbG8gdGhlcmU=" {
		t.Errorf("post-edit base64 wrong: %q", gotB64)
	}
}

func TestResolveInSet_RelativeInside(t *testing.T) {
	set := projects.Single("/tmp/work")
	_, abs, err := resolveInSet(set, "subdir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(abs, "/tmp/work/subdir/file.txt") {
		t.Errorf("unexpected resolved path: %s", abs)
	}
}

// TestResolveInSet_ProjectNamePrefix pins the May-2026 fix:
// in a multi-root workspace, kai_grep / kai_tree / kai_files
// emit results prefixed with the project NAME (not its
// directory). When the agent feeds those paths back into
// view / edit / write, the resolver must recognize the
// prefix and route to the matching project's filesystem
// path. Before this fix every such read failed with
// "file not found" because filepath.Join(discoveryRoot,
// "Kai/...") produced a path that didn't exist on disk.
// TestResolveInSet_UnroutedPathHintPreservesFullPath pins the
// 2026-05-11 fix: when the agent passes a path with an unknown
// head segment ("kai-cli/internal/..."), the error message used
// to suggest the basename-only example "Kai/foo.go" (via
// filepath.Base) — which trained the agent to drop subdirs and
// retry with "view Kai/foo.go", which then errored again. The
// fix preserves the full path in the hint so the agent has an
// actionable next call on the first error.
func TestResolveInSet_UnroutedPathHintPreservesFullPath(t *testing.T) {
	set := &projects.Set{DiscoveryRoot: "/tmp/work"}
	set.SetProjectsForTest([]*projects.Project{
		{Path: "/tmp/work/kai", Name: "Kai"},
		{Path: "/tmp/work/server", Name: "Kai Server"},
	})

	_, _, err := resolveInSet(set, "kai-cli/internal/tui/views/banner.go")
	if err == nil {
		t.Fatal("expected error for unrouted path")
	}
	msg := err.Error()
	// Hint preserves the full path so the agent can retry directly.
	if !strings.Contains(msg, "Kai/kai-cli/internal/tui/views/banner.go") {
		t.Errorf("hint should include the full path prefixed with project name, got: %s", msg)
	}
	// And the kai_files alternative should be mentioned so the
	// agent has a "give up guessing and look up the right path"
	// path.
	if !strings.Contains(msg, "kai_files") || !strings.Contains(msg, "**/banner.go") {
		t.Errorf("hint should mention kai_files with the basename glob, got: %s", msg)
	}
}

// TestWriteTool_ApproveReceivesUnifiedDiff confirms the writeTool
// approval gate forwards a non-empty unified-diff string to the
// approve callback. The UI layer relies on this diff to render the
// change preview above the confirmation prompt.
func TestWriteTool_ApproveReceivesUnifiedDiff(t *testing.T) {
	ws := t.TempDir()
	priorPath := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(priorPath, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotOp, gotPath, gotDiff string
	var gotAdded, gotRemoved int
	approve := func(ctx context.Context, op, path string, added, removed int, diff string) (bool, error) {
		gotOp, gotPath, gotDiff = op, path, diff
		gotAdded, gotRemoved = added, removed
		return true, nil
	}

	tool := (&FileTools{
		Set:     projects.Single(ws),
		Approve: approve,
	}).Write()
	resp, err := tool.Run(context.Background(), ToolCall{
		Name:  "write",
		Input: `{"file_path":"notes.txt","content":"alpha\ndelta\ngamma\n"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}

	if gotOp != "edit" {
		t.Errorf("op = %q, want %q", gotOp, "edit")
	}
	if gotPath != "notes.txt" {
		t.Errorf("path = %q, want %q", gotPath, "notes.txt")
	}
	// approxLineDiff is a delta-only approximation (newLines - oldLines):
	// when one line is swapped for another the count stays at 0/0. The
	// authoritative add/remove signal lives in the diff string itself,
	// which is asserted below.
	_ = gotAdded
	_ = gotRemoved
	if gotDiff == "" {
		t.Fatal("approve received empty diff string; expected unified diff body")
	}
	// Unified diff should mention both the removed and added lines so
	// the UI can render +/- context.
	if !strings.Contains(gotDiff, "-beta") {
		t.Errorf("diff missing removed line %q, got:\n%s", "-beta", gotDiff)
	}
	if !strings.Contains(gotDiff, "+delta") {
		t.Errorf("diff missing added line %q, got:\n%s", "+delta", gotDiff)
	}
}

// TestWriteTool_ApproveCancelSkipsWrite confirms that when the
// approval callback returns false, the file on disk is not modified
// and the agent receives a non-error "cancelled" response. The diff
// argument is still forwarded so the UI prompt can show context even
// when the user is about to decline.
func TestWriteTool_ApproveCancelSkipsWrite(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "keep.txt")
	original := []byte("untouched\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}

	var sawDiff string
	approve := func(ctx context.Context, op, path string, added, removed int, diff string) (bool, error) {
		sawDiff = diff
		return false, nil
	}

	tool := (&FileTools{
		Set:     projects.Single(ws),
		Approve: approve,
	}).Write()
	resp, err := tool.Run(context.Background(), ToolCall{
		Name:  "write",
		Input: `{"file_path":"keep.txt","content":"replaced\n"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("cancel path should not return tool error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("response should mention cancellation, got: %s", resp.Content)
	}
	if sawDiff == "" {
		t.Error("approve received empty diff string even on cancel path")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("file was modified despite cancel; got %q", string(got))
	}
}

func TestResolveInSet_ProjectNamePrefix(t *testing.T) {
	// Two projects with names that DON'T match their directory
	// basenames — the realistic case that exercises the
	// project-NAME (not directory-name) prefix path.
	set := &projects.Set{DiscoveryRoot: "/tmp/work"}
	set.SetProjectsForTest([]*projects.Project{
		{Path: "/tmp/work/inner-go", Name: "Kai"},
		{Path: "/tmp/work/server-dir", Name: "Kai Server"},
	})

	cases := []struct {
		input   string
		wantAbs string
	}{
		{"Kai/cmd/main.go", "/tmp/work/inner-go/cmd/main.go"},
		{"Kai Server/api/handler.go", "/tmp/work/server-dir/api/handler.go"},
	}
	for _, c := range cases {
		proj, abs, err := resolveInSet(set, c.input)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.input, err)
			continue
		}
		if abs != c.wantAbs {
			t.Errorf("%q: resolved to %q, want %q", c.input, abs, c.wantAbs)
		}
		if proj == nil {
			t.Errorf("%q: project not returned", c.input)
		}
	}
}
