package planner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kai/internal/ai"
)

// recordingCompleter is a minimal Completer test fixture. Captures
// the inputs each Complete call gets so tests can assert that
// recording actually invoked the wrapped provider, and replay didn't.
type recordingCompleter struct {
	calls    int
	response string
	err      error
}

func (r *recordingCompleter) Complete(system string, msgs []ai.Message, maxTokens int) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	return r.response, nil
}

// TestRecorder_RecordSavesAndReplaysReturnsSame: drive a full record →
// replay cycle. Recorder writes the fixture; a fresh recorder pointing
// at the same path replays it without touching the wrapped completer.
func TestRecorder_RecordSavesAndReplaysReturnsSame(t *testing.T) {
	dir := t.TempDir()

	wrapped := &recordingCompleter{response: "the model said hello"}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeRecord

	resp, err := rec.Complete("be helpful", []ai.Message{
		{Role: "user", Content: "hi"},
	}, 1024)
	if err != nil {
		t.Fatalf("record call: %v", err)
	}
	if resp != "the model said hello" {
		t.Errorf("response not propagated: %q", resp)
	}
	if wrapped.calls != 1 {
		t.Errorf("wrapped should have been called once during record, got %d", wrapped.calls)
	}

	// File should exist with our label-prefix.
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 fixture file, got %d", len(files))
	}
	if !strings.HasPrefix(files[0].Name(), sanitizeLabel(t.Name())) {
		t.Errorf("fixture filename should start with sanitized label: %s", files[0].Name())
	}

	// Replay with a fresh recorder + nil wrapped — should not call
	// out, but should return the same response.
	replay := NewRecorder(nil, dir, t.Name())
	replay.Mode = ModeReplay

	got, err := replay.Complete("be helpful", []ai.Message{{Role: "user", Content: "hi"}}, 1024)
	if err != nil {
		t.Fatalf("replay call: %v", err)
	}
	if got != "the model said hello" {
		t.Errorf("replay returned different response: %q", got)
	}
}

// TestRecorder_ReplayMissingFixtureErrors: in replay mode, a missing
// fixture must return a clear "re-record with X" error, not silently
// fall through. Every CI run depends on this contract.
func TestRecorder_ReplayMissingFixtureErrors(t *testing.T) {
	rec := NewRecorder(nil, t.TempDir(), t.Name())
	rec.Mode = ModeReplay

	_, err := rec.Complete("system", []ai.Message{{Role: "user", Content: "x"}}, 1024)
	if err == nil {
		t.Fatal("expected missing-fixture error")
	}
	for _, want := range []string{"no fixture", envFixtureMode, "record"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q:\n%s", want, err)
		}
	}
}

// TestRecorder_MixedModeRecordsThenReplays: in mixed mode the first
// call hits the wrapped provider and saves; subsequent identical
// calls don't re-hit. This is the common dev workflow.
func TestRecorder_MixedModeRecordsThenReplays(t *testing.T) {
	dir := t.TempDir()
	wrapped := &recordingCompleter{response: "first"}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeMixed

	for i := 0; i < 3; i++ {
		resp, err := rec.Complete("system", []ai.Message{{Role: "user", Content: "hi"}}, 100)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if resp != "first" {
			t.Errorf("call %d response: %q", i, resp)
		}
	}
	if wrapped.calls != 1 {
		t.Errorf("wrapped should fire once in mixed mode, got %d", wrapped.calls)
	}
}

// TestRecorder_RecordOverwritesExistingFixture: re-recording should
// replace the old fixture cleanly, not error or append.
func TestRecorder_RecordOverwritesExistingFixture(t *testing.T) {
	dir := t.TempDir()
	wrapped := &recordingCompleter{response: "first version"}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeRecord

	_, _ = rec.Complete("sys", []ai.Message{{Role: "user", Content: "x"}}, 100)
	wrapped.response = "second version"
	_, _ = rec.Complete("sys", []ai.Message{{Role: "user", Content: "x"}}, 100)

	// Replay must read the latest version.
	replay := NewRecorder(nil, dir, t.Name())
	replay.Mode = ModeReplay
	got, err := replay.Complete("sys", []ai.Message{{Role: "user", Content: "x"}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "second version" {
		t.Errorf("replay got stale value: %q", got)
	}
}

// TestRecorder_RequestVariationProducesDifferentFixtures: changing
// the prompt should produce a different fixture file (different
// hash). Tests this by recording two different prompts and confirming
// the dir contains two files.
func TestRecorder_RequestVariationProducesDifferentFixtures(t *testing.T) {
	dir := t.TempDir()
	wrapped := &recordingCompleter{response: "ok"}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeRecord

	_, _ = rec.Complete("sys A", []ai.Message{{Role: "user", Content: "x"}}, 100)
	_, _ = rec.Complete("sys B", []ai.Message{{Role: "user", Content: "x"}}, 100)
	_, _ = rec.Complete("sys A", []ai.Message{{Role: "user", Content: "y"}}, 100)
	// max_tokens variation
	_, _ = rec.Complete("sys A", []ai.Message{{Role: "user", Content: "x"}}, 200)

	files, _ := os.ReadDir(dir)
	if len(files) != 4 {
		t.Errorf("expected 4 distinct fixtures, got %d: %v", len(files), filenames(files))
	}
}

// TestRecorder_RecordWithNilWrappedErrors: trying to record without
// a wrapped completer is operator error; surface clearly.
func TestRecorder_RecordWithNilWrappedErrors(t *testing.T) {
	rec := NewRecorder(nil, t.TempDir(), t.Name())
	rec.Mode = ModeRecord
	_, err := rec.Complete("s", []ai.Message{{Role: "user", Content: "x"}}, 100)
	if err == nil || !strings.Contains(err.Error(), "wrapped") {
		t.Errorf("expected nil-wrapped error, got %v", err)
	}
}

// TestRecorder_PropagatesWrappedError: if the real provider errors
// during record, surface that — don't save a fixture.
func TestRecorder_PropagatesWrappedError(t *testing.T) {
	dir := t.TempDir()
	wrapped := &recordingCompleter{err: errors.New("rate limited")}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeRecord

	_, err := rec.Complete("s", []ai.Message{{Role: "user", Content: "x"}}, 100)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected wrapped error to propagate, got %v", err)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Errorf("no fixture should be saved on error, got %d files", len(files))
	}
}

// TestRecorder_DefaultModeIsReplay: with no env var and no explicit
// mode, the safe default is replay. Catches the regression where
// someone changes the default and accidentally costs tokens.
func TestRecorder_DefaultModeIsReplay(t *testing.T) {
	t.Setenv(envFixtureMode, "")
	rec := NewRecorder(nil, t.TempDir(), t.Name())
	if got := rec.resolveMode(); got != ModeReplay {
		t.Errorf("default mode should be replay, got %q", got)
	}
}

// TestRecorder_EnvVarPicksMode covers the env→mode wiring including
// invalid values (which fall back to replay, the safe default).
func TestRecorder_EnvVarPicksMode(t *testing.T) {
	cases := map[string]RecorderMode{
		"":              ModeReplay,
		"replay":        ModeReplay,
		"record":        ModeRecord,
		"mixed":         ModeMixed,
		"INVALID_VALUE": ModeReplay, // safe fallback
		"REPLAY":        ModeReplay, // case-sensitive — uppercase isn't recognized; fall through
	}
	for env, want := range cases {
		t.Setenv(envFixtureMode, env)
		rec := NewRecorder(nil, t.TempDir(), t.Name())
		if got := rec.resolveMode(); got != want {
			t.Errorf("KAI_LLM_FIXTURES=%q: got %q, want %q", env, got, want)
		}
	}
}

// TestRecorder_StructModeOverridesEnv: an explicit Mode field on the
// struct beats the env var. Tests use this to pin behavior regardless
// of dev environment.
func TestRecorder_StructModeOverridesEnv(t *testing.T) {
	t.Setenv(envFixtureMode, "record")
	rec := NewRecorder(nil, t.TempDir(), t.Name())
	rec.Mode = ModeReplay
	if got := rec.resolveMode(); got != ModeReplay {
		t.Errorf("struct override should win, got %q", got)
	}
}

// TestRecorder_FixtureSchemaMismatchErrors: an old fixture format
// after a schema bump must error loudly so we don't silently consume
// stale data with the wrong shape.
func TestRecorder_FixtureSchemaMismatchErrors(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(nil, dir, t.Name())
	rec.Mode = ModeReplay

	// Hand-craft a fixture with an old/unknown schema. Use the
	// canonical hash so the filename matches what a Complete call
	// would look up.
	hash, _ := canonicalHash("s", []ai.Message{{Role: "user", Content: "x"}}, 100)
	path := rec.fixturePath(hash)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(`{"schema":"kai-llm-fixture-v0","response":"old"}`), 0o644)

	_, err := rec.Complete("s", []ai.Message{{Role: "user", Content: "x"}}, 100)
	if err == nil || !strings.Contains(err.Error(), "unknown fixture schema") {
		t.Errorf("expected schema-mismatch error, got %v", err)
	}
}

// TestRecorder_FixtureWithMismatchedHashErrors: someone hand-edited a
// fixture's request body but didn't update the response. Catch this
// as drift in replay mode.
func TestRecorder_FixtureWithMismatchedHashErrors(t *testing.T) {
	dir := t.TempDir()
	wrapped := &recordingCompleter{response: "saved"}
	rec := NewRecorder(wrapped, dir, t.Name())
	rec.Mode = ModeRecord
	_, _ = rec.Complete("s", []ai.Message{{Role: "user", Content: "original"}}, 100)

	// Replay against a different request — same label but different
	// hash → file naming differs. So this naturally misses (covered
	// elsewhere). To exercise the "fixture has a stale Hash field"
	// path, hand-corrupt the saved file.
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("setup: expected 1 fixture, got %d", len(files))
	}
	path := filepath.Join(dir, files[0].Name())
	body, _ := os.ReadFile(path)
	// MarshalIndent emits `"hash": "..."` with a space; match that.
	corrupted := strings.Replace(string(body), `"hash": "`, `"hash": "deadbeef`, 1)
	if corrupted == string(body) {
		t.Fatalf("setup failed: didn't find hash field to corrupt in:\n%s", string(body))
	}
	_ = os.WriteFile(path, []byte(corrupted), 0o644)

	replay := NewRecorder(nil, dir, t.Name())
	replay.Mode = ModeReplay
	_, err := replay.Complete("s", []ai.Message{{Role: "user", Content: "original"}}, 100)
	if err == nil || !strings.Contains(err.Error(), "request differs") {
		t.Errorf("expected drift detection, got %v", err)
	}
}

// TestSanitizeLabel covers the path-safety conversion. Test names from
// table-driven tests look like "TestPlan/case1" — the slash must not
// land in a filename.
func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"":                 "fixture",
		"TestFoo":          "TestFoo",
		"TestFoo/case_1":   "TestFoo_case_1",
		"Test Foo Bar":     "Test_Foo_Bar",
		"Test:With,Punct.": "Test_With_Punct_",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q): got %q, want %q", in, got, want)
		}
	}
}

func filenames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
