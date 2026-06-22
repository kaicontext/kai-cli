package views

import (
	"strings"
	"testing"
)

// TestDispatch_VersionPrintsVersion checks the /version slash command
// writes the version held on PlannerServices into the scrollback.
func TestDispatch_VersionPrintsVersion(t *testing.T) {
	r := NewREPL("/usr/bin/true", "/tmp", &PlannerServices{Version: "9.9.9-test"})
	r2, _ := r.dispatch("/version")
	out := strings.Join(r2.pendingPrints, "")
	if !strings.Contains(out, "9.9.9-test") {
		t.Errorf("/version should print the version, got: %q", out)
	}
}

// TestVersion_InSlashCommands: "version" is registered for autocomplete.
func TestVersion_InSlashCommands(t *testing.T) {
	for _, c := range slashCommands {
		if c == "version" {
			return
		}
	}
	t.Error(`"version" should be in slashCommands`)
}

// TestExit_InSlashCommands: "exit" is registered for autocomplete.
func TestExit_InSlashCommands(t *testing.T) {
	for _, c := range slashCommands {
		if c == "exit" {
			return
		}
	}
	t.Error(`"exit" should be in slashCommands`)
}
