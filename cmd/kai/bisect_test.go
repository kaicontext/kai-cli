package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func withTempWorkDir(t *testing.T) func() {
	t.Helper()

	tmpDir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return func() {
		_ = os.Chdir(old)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return buf.String()
}

func makeHex(char string) string {
	return strings.Repeat(char, 64)
}

func TestIndexOfRef(t *testing.T) {
	refs := []bisectRef{
		{Name: "snap.a", TargetHex: makeHex("a")},
		{Name: "snap.b", TargetHex: makeHex("b")},
	}
	if got := indexOfRef(refs, makeHex("b")); got != 1 {
		t.Fatalf("expected index 1, got %d", got)
	}
	if got := indexOfRef(refs, "missing"); got != -1 {
		t.Fatalf("expected -1 for missing, got %d", got)
	}
}

func TestAdvanceBisectMidpoint(t *testing.T) {
	cleanup := withTempWorkDir(t)
	defer cleanup()

	state := bisectState{
		Refs: []bisectRef{
			{Name: "snap.a", TargetHex: makeHex("a")},
			{Name: "snap.b", TargetHex: makeHex("b")},
			{Name: "snap.c", TargetHex: makeHex("c")},
			{Name: "snap.d", TargetHex: makeHex("d")},
			{Name: "snap.e", TargetHex: makeHex("e")},
		},
		Low:     0,
		High:    4,
		Current: 0,
	}

	output := captureStdout(t, func() {
		if err := advanceBisect(state); err != nil {
			t.Fatalf("advance: %v", err)
		}
	})
	if !strings.Contains(output, "Test snapshot") {
		t.Fatalf("expected test snapshot output, got %q", output)
	}

	loaded, err := loadBisectState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.Current != 2 {
		t.Fatalf("expected midpoint current 2, got %d", loaded.Current)
	}
}

func TestAdvanceBisectTerminal(t *testing.T) {
	cleanup := withTempWorkDir(t)
	defer cleanup()

	state := bisectState{
		Refs: []bisectRef{
			{Name: "snap.good", TargetHex: makeHex("1")},
			{Name: "snap.bad", TargetHex: makeHex("2")},
		},
		Low:     0,
		High:    1,
		Current: 0,
	}

	output := captureStdout(t, func() {
		if err := advanceBisect(state); err != nil {
			t.Fatalf("advance: %v", err)
		}
	})
	if !strings.Contains(output, "First bad snapshot") {
		t.Fatalf("expected terminal output, got %q", output)
	}

	loaded, err := loadBisectState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.High != 1 {
		t.Fatalf("expected high to remain 1, got %d", loaded.High)
	}
}
