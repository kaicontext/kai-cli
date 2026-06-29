package views

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTrivialActionFastPath_PackageJSON pins the most common case:
// "run it" in a Node project with a "dev" script becomes "npm run dev"
// without any LLM round-trip.
func TestTrivialActionFastPath_PackageJSON(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "package.json"),
		[]byte(`{"scripts":{"dev":"vite","build":"vite build","start":"electron ."}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prompt string
		wantCmd string
	}{
		{"run it", "npm run dev"},
		{"run the app", "npm run dev"},
		{"start it", "npm run start"},
		{"build it", "npm run build"},
		{"dev", "npm run dev"},
		{"fire it up", "npm run dev"},
		{"launch it", "npm run dev"},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			cmd, _ := trivialActionFastPath(c.prompt, ws)
			if cmd != c.wantCmd {
				t.Errorf("trivialActionFastPath(%q) = %q, want %q", c.prompt, cmd, c.wantCmd)
			}
		})
	}
}

// TestTrivialActionFastPath_MonorepoSubPackage is the regression for
// the 2026-06-07 loom bounce: the root package.json had empty scripts
// and the runnable electron app lived in client/, so "run it" / "run
// the desktop app" bounced with "which app?" instead of resolving the
// sub-package's command.
func TestTrivialActionFastPath_MonorepoSubPackage(t *testing.T) {
	ws := t.TempDir()
	// Root: workspace manifest with no usable scripts (loom's shape).
	if err := os.WriteFile(filepath.Join(ws, "package.json"),
		[]byte(`{"scripts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "client", "package.json"),
		[]byte(`{"scripts":{"dev":"vite","start":"electron .","build":"vite build"}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prompt  string
		wantCmd string
	}{
		{"run it", "cd client && npm run dev"},
		{"run it?", "cd client && npm run dev"},
		{"run the desktop app", "cd client && npm run dev"},
		{"run the electron app", "cd client && npm run dev"},
		{"start it", "cd client && npm run start"},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			cmd, _ := trivialActionFastPath(c.prompt, ws)
			if cmd != c.wantCmd {
				t.Errorf("trivialActionFastPath(%q) = %q, want %q", c.prompt, cmd, c.wantCmd)
			}
		})
	}
}

func TestTrivialActionFastPath_CargoToml(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "Cargo.toml"), []byte(`[package]`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prompt  string
		wantCmd string
	}{
		{"run it", "cargo run"},
		{"build it", "cargo build"},
		{"test it", "cargo test"},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			cmd, _ := trivialActionFastPath(c.prompt, ws)
			if cmd != c.wantCmd {
				t.Errorf("got %q, want %q", cmd, c.wantCmd)
			}
		})
	}
}

func TestTrivialActionFastPath_GoMod(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		prompt  string
		wantCmd string
	}{
		{"run it", "go run ./..."},
		{"build it", "go build ./..."},
		{"test it", "go test ./..."},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			cmd, _ := trivialActionFastPath(c.prompt, ws)
			if cmd != c.wantCmd {
				t.Errorf("got %q, want %q", cmd, c.wantCmd)
			}
		})
	}
}

// TestTrivialActionFastPath_DoesNotFireOnCodeEditRequests is the
// anti-pattern guard. A prompt that says "test the build handler"
// is a CODE change about the build path, not a "run the build"
// command — must fall through to triage.
func TestTrivialActionFastPath_DoesNotFireOnCodeEdit(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "package.json"),
		[]byte(`{"scripts":{"dev":"vite","test":"vitest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	negatives := []string{
		"write a test for the run handler",
		"add a test that the build command exits 0",
		"fix the run command",
		"refactor the build pipeline",
		"implement a dev server proxy",
	}
	for _, p := range negatives {
		t.Run(p, func(t *testing.T) {
			cmd, _ := trivialActionFastPath(p, ws)
			if cmd != "" {
				t.Errorf("fast path should NOT fire on code-edit request: prompt=%q cmd=%q", p, cmd)
			}
		})
	}
}

// TestTrivialActionFastPath_NoManifestFallsThrough confirms the
// fast path bails when there's no manifest to anchor to.
func TestTrivialActionFastPath_NoManifestFallsThrough(t *testing.T) {
	ws := t.TempDir() // empty
	for _, p := range []string{"run it", "build it", "test it"} {
		if cmd, _ := trivialActionFastPath(p, ws); cmd != "" {
			t.Errorf("expected fall-through with no manifest; prompt=%q cmd=%q", p, cmd)
		}
	}
}

func TestMatchActionVerb(t *testing.T) {
	cases := map[string]string{
		"run":               "run",
		"run it":            "run",
		"run this":          "run",
		"run the app":       "run",
		"start the server":  "start",
		"build it":          "build",
		"test":              "test",
		"dev":               "dev",
		"fire it up":        "run",
		"kick it off":       "run",
		// 2026-05-25: nicety stripping — conversational openers
		// strip before the word-count + verb-match runs. Pins the
		// dogfood failure where "can you run the app?" missed the
		// fast-path and the agent then fabricated a fake launch.
		"can you run the app":     "run",
		"could you start the dev server": "start",
		"please run it":           "run",
		"would you build it":      "build",
		"will you test it":        "test",
		"pls run":                 "run",
		"please can you run it":   "run",
		// negatives
		"run the migration that adds a column": "",
		// Five words after the nicety strip ("run this for me please")
		// — still over the 4-word cap, intentional.
		"can you run this for me please": "",
		"write a test":                   "",
		"":                               "",
	}
	for p, want := range cases {
		t.Run(p, func(t *testing.T) {
			if got := matchActionVerb(p); got != want {
				t.Errorf("matchActionVerb(%q) = %q, want %q", p, got, want)
			}
		})
	}
}
