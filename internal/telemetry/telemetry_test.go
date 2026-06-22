package telemetry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/posthog/posthog-go"
)

// withTempHome sets HOME to a temp dir for the duration of the test,
// so config files don't pollute the real home directory.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	os.MkdirAll(filepath.Join(tmp, ".kai"), 0o700)
	// Force a fresh singleton per test; the sink is mocked via withFakeSink.
	clientMu.Lock()
	clientInst = nil
	clientMu.Unlock()
	return tmp
}

// fakeSink captures every Enqueue call for assertion and counts closes.
type fakeSink struct {
	mu       sync.Mutex
	captures []posthog.Capture
	closes   int
}

func (f *fakeSink) Enqueue(m posthog.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := m.(posthog.Capture); ok {
		f.captures = append(f.captures, c)
	}
	return nil
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// withFakeSink swaps the PostHog client factory for the duration of a test.
func withFakeSink(t *testing.T) *fakeSink {
	t.Helper()
	fake := &fakeSink{}
	orig := newClient
	newClient = func() (sink, error) { return fake, nil }
	t.Cleanup(func() {
		newClient = orig
		clientMu.Lock()
		clientInst = nil
		clientMu.Unlock()
	})
	return fake
}

// ─── Config / opt-in tests ──────────────────────────────────────────────────

func TestLoadConfig_Missing(t *testing.T) {
	withTempHome(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Error("expected disabled config when file missing")
	}
	if cfg.Level != "basic" {
		t.Errorf("expected level=basic, got %q", cfg.Level)
	}
}

func TestLoadConfig_Exists(t *testing.T) {
	tmp := withTempHome(t)
	data := `{"enabled":true,"install_id":"test-uuid","level":"basic","created_at":"2026-01-01T00:00:00Z"}`
	os.WriteFile(filepath.Join(tmp, ".kai", "telemetry.json"), []byte(data), 0o600)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled {
		t.Error("expected enabled=true")
	}
	if cfg.InstallID != "test-uuid" {
		t.Errorf("expected install_id=test-uuid, got %q", cfg.InstallID)
	}
}

func TestSaveAndLoad(t *testing.T) {
	withTempHome(t)
	cfg := &Config{
		Enabled:   true,
		InstallID: "round-trip-id",
		Level:     "basic",
		CreatedAt: "2026-02-15T00:00:00Z",
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InstallID != "round-trip-id" {
		t.Errorf("expected round-trip-id, got %q", loaded.InstallID)
	}
	if !loaded.Enabled {
		t.Error("expected enabled=true after round-trip")
	}
}

func TestIsEnabled_DefaultIsOnWhenNoConfig(t *testing.T) {
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	if !IsEnabled() {
		t.Error("expected telemetry ON when no config file exists (opt-out default)")
	}
}

func TestIsEnabled_RespectsExplicitDisable(t *testing.T) {
	// Returning users who ran 'kai telemetry disable' must stay disabled,
	// even though the default flipped to on.
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	cfg := &Config{Enabled: false, InstallID: "prev-user", Level: "basic"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if IsEnabled() {
		t.Error("expected disabled config to be honored")
	}
}

func TestIsEnabled_EnvOverrides(t *testing.T) {
	withTempHome(t)
	t.Setenv("CI", "")

	t.Setenv("KAI_TELEMETRY", "1")
	if !IsEnabled() {
		t.Error("expected KAI_TELEMETRY=1 to enable")
	}

	Enable()
	t.Setenv("KAI_TELEMETRY", "0")
	if IsEnabled() {
		t.Error("expected KAI_TELEMETRY=0 to hard-disable")
	}
}

func TestIsEnabled_CIAutoDisable(t *testing.T) {
	withTempHome(t)
	Enable()

	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "true")
	if IsEnabled() {
		t.Error("expected CI=true to auto-disable")
	}

	t.Setenv("KAI_TELEMETRY", "1")
	if !IsEnabled() {
		t.Error("expected KAI_TELEMETRY=1 to override CI=true")
	}
}

func TestEnableDisable_InstallIDPreserved(t *testing.T) {
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")

	if err := Enable(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig()
	if !cfg.Enabled {
		t.Error("expected enabled after Enable()")
	}
	if cfg.InstallID == "" {
		t.Error("expected install_id to be generated")
	}
	firstID := cfg.InstallID

	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadConfig()
	if cfg.Enabled {
		t.Error("expected disabled after Disable()")
	}
	if cfg.InstallID != firstID {
		t.Errorf("install_id should be preserved across Disable/Enable; was %q, now %q", firstID, cfg.InstallID)
	}
}

// ─── Event / PostHog dispatch tests ─────────────────────────────────────────

func TestNewEvent_PopulatesIdentity(t *testing.T) {
	withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	SetVersion("0.12.0-test")

	e := NewEvent("capture")
	if e == nil {
		t.Fatal("expected non-nil event when enabled")
	}
	if e.Command != "capture" {
		t.Errorf("expected command=capture, got %q", e.Command)
	}
	if e.Version != "0.12.0-test" {
		t.Errorf("expected version=0.12.0-test, got %q", e.Version)
	}
	if e.OS == "" || e.Arch == "" {
		t.Error("expected OS and Arch to be populated")
	}
	if e.Result != "ok" {
		t.Errorf("expected result=ok, got %q", e.Result)
	}
}

func TestNewEvent_NilWhenDisabled(t *testing.T) {
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "0")
	e := NewEvent("capture")
	if e != nil {
		t.Error("expected nil event when disabled")
	}
	// Nil-safe methods must not panic.
	e.SetPhase("test", 100)
	e.SetStat("files", 10)
	e.SetCache("hits", 5)
	e.SetResult("error")
	e.SetErrorClass("network")
	e.Finish()
}

func TestFinish_EnqueuesToPostHog(t *testing.T) {
	withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	fake := withFakeSink(t)

	SetVersion("0.12.0-test")
	e := NewEvent("capture")
	e.SetPhase("parse", 42)
	e.SetStat("files", 7)
	e.SetCache("hit", 3)
	e.Finish()

	if got := len(fake.captures); got != 1 {
		t.Fatalf("expected 1 enqueued capture, got %d", got)
	}
	c := fake.captures[0]
	if c.Event != eventName {
		t.Errorf("expected event=%q, got %q", eventName, c.Event)
	}
	cfg, _ := LoadConfig()
	if c.DistinctId != cfg.InstallID {
		t.Errorf("expected distinct_id=%q, got %q", cfg.InstallID, c.DistinctId)
	}
	props := map[string]interface{}(c.Properties)
	if props["command"] != "capture" {
		t.Errorf("expected command=capture in properties, got %v", props["command"])
	}
	if props["phase_parse_ms"] != int64(42) {
		t.Errorf("expected phase_parse_ms=42, got %v", props["phase_parse_ms"])
	}
	if props["stat_files"] != int64(7) {
		t.Errorf("expected stat_files=7, got %v", props["stat_files"])
	}
	if props["cache_hit"] != int64(3) {
		t.Errorf("expected cache_hit=3, got %v", props["cache_hit"])
	}
	if props["result"] != "ok" {
		t.Errorf("expected result=ok, got %v", props["result"])
	}
	if _, present := props["error_class"]; present {
		t.Error("error_class should be absent when not set")
	}
}

func TestFinish_EmitsErrorClassWhenSet(t *testing.T) {
	withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	fake := withFakeSink(t)

	e := NewEvent("push")
	e.SetResult("error")
	e.SetErrorClass("network")
	e.Finish()

	if len(fake.captures) != 1 {
		t.Fatalf("expected 1 capture, got %d", len(fake.captures))
	}
	props := map[string]interface{}(fake.captures[0].Properties)
	if props["error_class"] != "network" {
		t.Errorf("expected error_class=network, got %v", props["error_class"])
	}
	if props["result"] != "error" {
		t.Errorf("expected result=error, got %v", props["result"])
	}
}

func TestFinish_NoOpWhenDisabled(t *testing.T) {
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "0")
	fake := withFakeSink(t)

	e := NewEvent("whatever") // nil because disabled
	e.Finish()

	if got := len(fake.captures); got != 0 {
		t.Errorf("expected no enqueued captures when disabled, got %d", got)
	}
}

func TestClose_FlushesAndResetsSingleton(t *testing.T) {
	withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	fake := withFakeSink(t)

	e := NewEvent("status")
	e.Finish()
	if len(fake.captures) != 1 {
		t.Fatalf("expected 1 capture before close, got %d", len(fake.captures))
	}

	Close()
	if fake.closes != 1 {
		t.Errorf("expected 1 close, got %d", fake.closes)
	}

	// A new Finish after Close must re-init — the fakeSink factory runs again
	// and the new client is a second instance (but our factory always returns
	// the same fake, so closes increments again on next Close).
	e2 := NewEvent("status")
	e2.Finish()
	if len(fake.captures) != 2 {
		t.Errorf("expected 2 captures after re-use, got %d", len(fake.captures))
	}
}

func TestClose_SafeToCallMultipleTimes(t *testing.T) {
	withTempHome(t)
	Close()
	Close()
	Close()
}

func TestNewEvent_DeletesLegacySpool(t *testing.T) {
	tmp := withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")

	// Simulate a pre-PostHog install that wrote to the spool.
	spool := filepath.Join(tmp, ".kai", "telemetry.jsonl")
	if err := os.WriteFile(spool, []byte(`{"event":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = NewEvent("capture")

	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Errorf("legacy spool should have been deleted on NewEvent; err=%v", err)
	}
}

func TestNewEvent_CreatesConfigAndInstallIDOnFirstUse(t *testing.T) {
	tmp := withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")

	// No config yet — default-on path creates one with a fresh install_id.
	if _, err := os.Stat(filepath.Join(tmp, ".kai", "telemetry.json")); !os.IsNotExist(err) {
		t.Fatalf("test setup: config should not exist yet; err=%v", err)
	}
	_ = withFakeSink(t)

	e := NewEvent("init")
	if e == nil {
		t.Fatal("expected non-nil event under default-on")
	}
	e.Finish()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if !cfg.Enabled {
		t.Error("config created by first event should have Enabled=true")
	}
	if cfg.InstallID == "" {
		t.Error("config created by first event should have a generated install_id")
	}
	if cfg.CreatedAt == "" {
		t.Error("config created by first event should stamp CreatedAt")
	}
}

func TestNewEvent_DoesNotRecreateConfig(t *testing.T) {
	// Once a config exists, NewEvent must not overwrite it — in particular,
	// a user who disabled telemetry must not have their install_id rotated
	// or their disabled flag silently flipped on by a later upgrade.
	tmp := withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "1") // forces IsEnabled() true even though cfg disabled, just so NewEvent runs
	cfg := &Config{Enabled: false, InstallID: "stable-id", Level: "basic", CreatedAt: "2020-01-01T00:00:00Z"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_ = withFakeSink(t)

	_ = NewEvent("init")

	loaded, _ := LoadConfig()
	if loaded.InstallID != "stable-id" {
		t.Errorf("install_id was rotated: got %q", loaded.InstallID)
	}
	if loaded.Enabled {
		t.Error("Enabled flag was flipped on by NewEvent; user opt-out not respected")
	}
	_ = tmp
}

func TestFlushNow_DisabledReturnsError(t *testing.T) {
	withTempHome(t)
	t.Setenv("KAI_TELEMETRY", "0")
	if err := FlushNow(); err == nil {
		t.Error("expected FlushNow to error when disabled")
	}
}

func TestFlushNow_EnabledClosesClient(t *testing.T) {
	withTempHome(t)
	Enable()
	t.Setenv("KAI_TELEMETRY", "")
	t.Setenv("CI", "")
	fake := withFakeSink(t)

	// Prime the singleton with one event.
	NewEvent("status").Finish()

	if err := FlushNow(); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}
	if fake.closes != 1 {
		t.Errorf("expected 1 close after FlushNow, got %d", fake.closes)
	}
}
