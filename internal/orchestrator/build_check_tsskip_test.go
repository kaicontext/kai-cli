package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTSEcosystem_SkipsWhenTypescriptMissing(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "frontend")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// tsconfig.json present, but no node_modules/typescript anywhere up
	// the tree. detectTSEcosystem must return "" so runBuildCheck skips
	// the TS check entirely.
	if err := os.WriteFile(filepath.Join(sub, "tsconfig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectTSEcosystem(dir); got != "" {
		t.Errorf("expected skip when typescript not installed, got %q", got)
	}
}

func TestDetectTSEcosystem_DetectsWhenTypescriptInstalled(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "frontend")
	if err := os.MkdirAll(filepath.Join(sub, "node_modules", "typescript"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "tsconfig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectTSEcosystem(dir); got != sub {
		t.Errorf("got %q, want %q (frontend dir with typescript installed)", got, sub)
	}
}

func TestDetectTSEcosystem_WalksAncestorsForRootInstall(t *testing.T) {
	// Common monorepo layout: tsconfig in apps/web, typescript in
	// the root node_modules. Detection must walk up from the manifest
	// dir to find the install.
	dir := t.TempDir()
	app := filepath.Join(dir, "apps", "web")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "typescript"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "tsconfig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectTSEcosystem(dir); got != app {
		t.Errorf("got %q, want %q (manifest in apps/web, install at root)", got, app)
	}
}

func TestIsNPXToolMissing_RecognizesDiagnostic(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"npx tsc shim no install", "This is not the tsc command you are looking for\n", true},
		{"npm couldn't find executable", "npm ERR! could not determine executable to run\n", true},
		{"tsc not found in path", "/bin/sh: tsc: not found\n", true},
		{"real type error is not a missing tool", "main.ts:5:3: error TS2304: Cannot find name 'foo'.\n", false},
		{"empty output", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNPXToolMissing(c.output); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
