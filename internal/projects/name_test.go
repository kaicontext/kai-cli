package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSmartName_FromPackageJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"my-pkg","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "my-pkg" {
		t.Errorf("got %q, want my-pkg", got)
	}
}

func TestSmartName_FromGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module github.com/foo/bar\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "bar" {
		t.Errorf("got %q, want bar", got)
	}
}

func TestSmartName_FromCargoToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"),
		[]byte(`[package]
name = "rust-thing"
version = "0.1.0"

[dependencies]
serde = "1"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "rust-thing" {
		t.Errorf("got %q, want rust-thing", got)
	}
}

func TestSmartName_FromPyproject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
		[]byte(`[project]
name = "py-thing"
version = "0.1"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "py-thing" {
		t.Errorf("got %q, want py-thing", got)
	}
}

func TestSmartName_FromReadmeMarkdown(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("# My Cool Project\n\nSome text.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "My Cool Project" {
		t.Errorf("got %q, want My Cool Project", got)
	}
}

func TestSmartName_FallbackToDirName(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "some-thing")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "some-thing" {
		t.Errorf("got %q, want some-thing", got)
	}
}

func TestSmartName_PrecedencePackageJSONBeatsReadme(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"json-name"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("# Readme Name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SmartName(dir); got != "json-name" {
		t.Errorf("got %q, want json-name (package.json precedence)", got)
	}
}
