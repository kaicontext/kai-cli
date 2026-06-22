package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCmd_Default(t *testing.T) {
	out := captureStdout(t, func() {
		versionShort = false
		cmd := *versionCmd // shallow copy so flags don't leak
		cmd.Run(&cmd, nil)
	})
	got := strings.TrimSpace(out)
	want := "kai " + Version
	if got != want {
		t.Errorf("version default: got %q, want %q", got, want)
	}
}

func TestVersionCmd_Short(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"with pre-release", "0.28.7-dogfood", "0.28.7"},
		{"with pre-release and build", "0.28.7-dogfood+abc123", "0.28.7"},
		{"with build only", "1.2.3+build456", "1.2.3"},
		{"plain semver", "2.0.0", "2.0.0"},
		{"no match falls back", "latest", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origVersion := Version
			Version = tt.version
			defer func() { Version = origVersion }()

			versionShort = true
			defer func() { versionShort = false }()

			out := captureStdout(t, func() {
				cmd := *versionCmd
				cmd.Run(&cmd, nil)
			})
			got := strings.TrimSpace(out)
			if got != tt.want {
				t.Errorf("version --short with %q: got %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestVersionCmd_ShortNoPrefix(t *testing.T) {
	// --short should NOT include the "kai " prefix
	origVersion := Version
	Version = "1.0.0-beta"
	defer func() { Version = origVersion }()

	versionShort = true
	defer func() { versionShort = false }()

	out := captureStdout(t, func() {
		cmd := *versionCmd
		cmd.Run(&cmd, nil)
	})
	got := strings.TrimSpace(out)
	if strings.HasPrefix(got, "kai ") {
		t.Errorf("version --short should not have 'kai ' prefix, got %q", got)
	}
	if got != "1.0.0" {
		t.Errorf("version --short: got %q, want %q", got, "1.0.0")
	}
}

func TestVersionCmd_JSON(t *testing.T) {
	type versionOut struct {
		Version string `json:"version"`
		Build   string `json:"build"`
		Commit  string `json:"commit"`
	}

	t.Run("with pre-release and build metadata", func(t *testing.T) {
		origVersion := Version
		Version = "0.28.7-dogfood+14d4b34"
		defer func() { Version = origVersion }()

		versionJSON = true
		defer func() { versionJSON = false }()

		out := captureStdout(t, func() {
			cmd := *versionCmd
			cmd.Run(&cmd, nil)
		})

		var got versionOut
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
			t.Fatalf("--json output is not valid JSON: %v\noutput: %q", err, out)
		}
		if got.Version != "0.28.7" {
			t.Errorf("version field: got %q, want %q", got.Version, "0.28.7")
		}
		if got.Build != "dogfood" {
			t.Errorf("build field: got %q, want %q", got.Build, "dogfood")
		}
		if got.Commit != "14d4b34" {
			t.Errorf("commit field: got %q, want %q", got.Commit, "14d4b34")
		}
	})

	t.Run("with pre-release only no build metadata", func(t *testing.T) {
		origVersion := Version
		Version = "0.28.7-dogfood"
		defer func() { Version = origVersion }()

		versionJSON = true
		defer func() { versionJSON = false }()

		out := captureStdout(t, func() {
			cmd := *versionCmd
			cmd.Run(&cmd, nil)
		})

		var got versionOut
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
			t.Fatalf("--json output is not valid JSON: %v\noutput: %q", err, out)
		}
		if got.Version != "0.28.7" {
			t.Errorf("version field: got %q, want %q", got.Version, "0.28.7")
		}
		if got.Build != "dogfood" {
			t.Errorf("build field: got %q, want %q", got.Build, "dogfood")
		}
		if got.Commit != "" {
			t.Errorf("commit field: got %q, want empty string (no build metadata)", got.Commit)
		}
	})

	t.Run("plain semver", func(t *testing.T) {
		origVersion := Version
		Version = "1.2.3"
		defer func() { Version = origVersion }()

		versionJSON = true
		defer func() { versionJSON = false }()

		out := captureStdout(t, func() {
			cmd := *versionCmd
			cmd.Run(&cmd, nil)
		})

		var got versionOut
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
			t.Fatalf("--json output is not valid JSON: %v\noutput: %q", err, out)
		}
		if got.Version != "1.2.3" {
			t.Errorf("version field: got %q, want %q", got.Version, "1.2.3")
		}
		if got.Build != "" {
			t.Errorf("build field: got %q, want empty string", got.Build)
		}
		if got.Commit != "" {
			t.Errorf("commit field: got %q, want empty string", got.Commit)
		}
	})

	t.Run("json takes priority over short", func(t *testing.T) {
		// --json fires before --short; if both are set, JSON output wins.
		origVersion := Version
		Version = "0.1.0-rc+abc"
		defer func() { Version = origVersion }()

		versionJSON = true
		defer func() { versionJSON = false }()
		versionShort = true
		defer func() { versionShort = false }()

		out := captureStdout(t, func() {
			cmd := *versionCmd
			cmd.Run(&cmd, nil)
		})

		// Output must be valid JSON (not the bare semver that --short would emit).
		var got versionOut
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
			t.Fatalf("expected JSON output when --json is set, got %q: %v", out, err)
		}
		if got.Version != "0.1.0" {
			t.Errorf("version field: got %q, want %q", got.Version, "0.1.0")
		}
	})
}
