package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/remote"
)

func TestPersonalSlug(t *testing.T) {
	cases := map[string]string{
		"jschatz1@gmail.com":       "jschatz1",
		"Jacob.Schatz@Example.COM": "jacob-schatz",
		"no-at-sign":               "",
		"":                         "",
	}
	for in, want := range cases {
		if got := personalSlug(in); got != want {
			t.Errorf("personalSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickPersonalOrg(t *testing.T) {
	orgs := []remote.OrgInfo{
		{Slug: "calendardev"},
		{Slug: "howth"},
		{Slug: "jschatz1"},
	}

	// Matches the personal org by email local-part, not just the first.
	if got := pickPersonalOrg(orgs, "jschatz1@gmail.com"); got == nil || got.Slug != "jschatz1" {
		t.Fatalf("expected personal org jschatz1, got %+v", got)
	}
	// No match → fall back to the first org.
	if got := pickPersonalOrg(orgs, "nobody@elsewhere.com"); got == nil || got.Slug != "calendardev" {
		t.Fatalf("expected fallback to first org, got %+v", got)
	}
	// No orgs → nil.
	if got := pickPersonalOrg(nil, "jschatz1@gmail.com"); got != nil {
		t.Fatalf("expected nil for empty org list, got %+v", got)
	}
}

func TestRunInitAlreadyInitialized(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFile)
	if err := os.WriteFile(dbPath, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to create db file: %v", err)
	}

	oldKaiDir := kaiDir
	kaiDir = dir
	defer func() { kaiDir = oldKaiDir }()

	oldInitForce := initForce
	initForce = false
	defer func() { initForce = oldInitForce }()

	oldNoRemote := initNoRemote
	initNoRemote = true
	defer func() { initNoRemote = oldNoRemote }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	err = runInit(nil, nil)
	w.Close()
	if err != nil {
		t.Fatalf("expected nil error on idempotent init, got: %v", err)
	}

	outputBytes, _ := io.ReadAll(r)
	output := string(outputBytes)
	if !strings.Contains(output, "already initialized") {
		t.Errorf("expected stderr to contain 'already initialized', got: %q", output)
	}
	if !strings.Contains(output, "kai init --force") {
		t.Errorf("expected stderr to contain 'kai init --force', got: %q", output)
	}
}
