package main

import (
	"testing"
)

func TestGraphCmdRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "graph" {
			found = true
			if cmd.GroupID != groupAdvanced {
				t.Errorf("graphCmd.GroupID = %q, want %q", cmd.GroupID, groupAdvanced)
			}
			break
		}
	}
	if !found {
		t.Error("graphCmd not registered with rootCmd")
	}
}

func TestGraphExportCmdFlags(t *testing.T) {
	f := graphExportCmd.PersistentFlags()
	for _, name := range []string{"node-cursor", "edge-cursor", "limit"} {
		if f.Lookup(name) == nil {
			t.Errorf("graphExportCmd missing flag %q", name)
		}
	}

	nc, err := f.GetInt64("node-cursor")
	if err != nil {
		t.Fatalf("node-cursor flag error: %v", err)
	}
	if nc != 0 {
		t.Errorf("node-cursor default = %d, want 0", nc)
	}

	ec, err := f.GetInt64("edge-cursor")
	if err != nil {
		t.Fatalf("edge-cursor flag error: %v", err)
	}
	if ec != 0 {
		t.Errorf("edge-cursor default = %d, want 0", ec)
	}

	lim, err := f.GetInt("limit")
	if err != nil {
		t.Fatalf("limit flag error: %v", err)
	}
	if lim != 10000 {
		t.Errorf("limit default = %d, want 10000", lim)
	}
}

func TestGraphExportCmdSubcommand(t *testing.T) {
	found := false
	for _, cmd := range graphCmd.Commands() {
		if cmd.Use == "export" {
			found = true
			break
		}
	}
	if !found {
		t.Error("graphExportCmd not registered as subcommand of graphCmd")
	}
}