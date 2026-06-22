package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGateListEntryJSONRoundtrip(t *testing.T) {
	entry := gateListEntry{
		ID:          "abc123",
		Project:     "myproject",
		Verdict:     "REVIEW",
		BlastRadius: 5,
		From:        "workspace-1",
		Timestamp:   "2025-01-15 10:30:00",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal gateListEntry: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal roundtrip: %v", err)
	}
	for _, field := range []string{"id", "project", "verdict", "blastRadius", "from", "timestamp"} {
		if _, ok := parsed[field]; !ok {
			t.Errorf("missing JSON field %q in output: %s", field, string(data))
		}
	}
	var loaded gateListEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("roundtrip unmarshal: %v", err)
	}
	if loaded.ID != entry.ID {
		t.Errorf("id: want %q, got %q", entry.ID, loaded.ID)
	}
	if loaded.Project != entry.Project {
		t.Errorf("project: want %q, got %q", entry.Project, loaded.Project)
	}
	if loaded.Verdict != entry.Verdict {
		t.Errorf("verdict: want %q, got %q", entry.Verdict, loaded.Verdict)
	}
	if loaded.BlastRadius != entry.BlastRadius {
		t.Errorf("blastRadius: want %d, got %d", entry.BlastRadius, loaded.BlastRadius)
	}
	if loaded.From != entry.From {
		t.Errorf("from: want %q, got %q", entry.From, loaded.From)
	}
	if loaded.Timestamp != entry.Timestamp {
		t.Errorf("timestamp: want %q, got %q", entry.Timestamp, loaded.Timestamp)
	}
}

func TestGateListEntryJSONArray(t *testing.T) {
	entries := []gateListEntry{
		{ID: "aaa", Project: "p1", Verdict: "REVIEW", BlastRadius: 3, From: "ws1", Timestamp: "2025-01-01 00:00:00"},
		{ID: "bbb", Project: "p2", Verdict: "BLOCK", BlastRadius: 10, From: "ws2", Timestamp: "2025-06-15 12:00:00"},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	if !strings.HasPrefix(string(data), "[") {
		t.Errorf("expected JSON array, got: %s", string(data)[:min(20, len(data))])
	}
	var loaded []gateListEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("roundtrip unmarshal: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded))
	}
	if loaded[0].ID != "aaa" || loaded[1].ID != "bbb" {
		t.Errorf("roundtrip mismatch: got %+v", loaded)
	}
}

func TestGateListEntryZeroValues(t *testing.T) {
	entry := gateListEntry{}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal zero entry: %v", err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"id":""`) {
		t.Errorf("expected empty id, got: %s", raw)
	}
	if !strings.Contains(raw, `"blastRadius":0`) {
		t.Errorf("expected zero blastRadius, got: %s", raw)
	}
}

func TestGateListJSONFlagRegistered(t *testing.T) {
	flag := gateListCmd.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("--json flag not registered on gateListCmd")
	}
	if flag.DefValue != "false" {
		t.Errorf("--json default: want 'false', got %q", flag.DefValue)
	}
}

func TestGateListJSONDefaultsFalse(t *testing.T) {
	if gateListJSON {
		t.Error("gateListJSON should default to false")
	}
}

// TestGateListSingleJSONEmptyProject verifies that a gateListEntry with an
// empty Project field (as produced by runGateListSingle) roundtrips correctly.
func TestGateListSingleJSONEmptyProject(t *testing.T) {
	entry := gateListEntry{
		ID:          "deadbeef",
		Project:     "",
		Verdict:     "REVIEW",
		BlastRadius: 3,
		From:        "/tmp/ws",
		Timestamp:   "2025-01-01 00:00:00",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Verify Project is present as empty string in JSON output
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if proj, ok := m["project"]; !ok {
		t.Fatal("missing 'project' key in JSON output")
	} else if projStr, ok := proj.(string); !ok {
		t.Fatalf("project is not a string: %T", proj)
	} else if projStr != "" {
		t.Fatalf("project should be empty string for single-DB path, got %q", projStr)
	}
	// Roundtrip
	var entry2 gateListEntry
	if err := json.Unmarshal(data, &entry2); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	if entry2.Project != "" {
		t.Fatalf("roundtrip: Project = %q, want empty string", entry2.Project)
	}
}