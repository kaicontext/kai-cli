package views

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRingBuf_AppendsAndCaps(t *testing.T) {
	r := newRingBuf(10)
	r.Write([]byte("abcde"))
	if got := r.Snapshot(); got != "abcde" {
		t.Errorf("expected %q, got %q", "abcde", got)
	}
	r.Write([]byte("fghij"))
	if got := r.Snapshot(); got != "abcdefghij" {
		t.Errorf("at cap: expected %q, got %q", "abcdefghij", got)
	}
	r.Write([]byte("k"))
	if got := r.Snapshot(); got != "bcdefghijk" {
		t.Errorf("after eviction: expected %q, got %q", "bcdefghijk", got)
	}
}

func TestRingBuf_OverflowLargerThanCap(t *testing.T) {
	r := newRingBuf(5)
	r.Write([]byte("0123456789"))
	if got := r.Snapshot(); got != "56789" {
		t.Errorf("expected last 5 chars, got %q", got)
	}
}

func TestRingBuf_LenTracksContent(t *testing.T) {
	r := newRingBuf(100)
	if r.Len() != 0 {
		t.Errorf("empty Len = %d, want 0", r.Len())
	}
	r.Write([]byte("hello"))
	if r.Len() != 5 {
		t.Errorf("after write Len = %d, want 5", r.Len())
	}
}

func TestNormalizeHostProcSig_StripsAbsolutePaths(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "Error at /Users/jacobschatz/projects/kai/kai-desktop/src/AgentsView.svelte:25:12 Unexpected",
			want: "Error at AgentsView.svelte:25:12 Unexpected",
		},
		{
			in:   "/private/tmp/kai-port-/kai-desktop/src/x.svelte:1 broken",
			want: "x.svelte:1 broken",
		},
		{
			in:   "no path here, just an error",
			want: "no path here, just an error",
		},
	}
	for _, c := range cases {
		name := c.in
		if len(name) > 40 {
			name = name[:40]
		}
		t.Run(name, func(t *testing.T) {
			got := normalizeHostProcSig(c.in)
			if got != c.want {
				t.Errorf("normalizeHostProcSig(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeHostProcSig_CapsAt200Chars(t *testing.T) {
	in := "Error: " + strings.Repeat("x", 500)
	got := normalizeHostProcSig(in)
	if len(got) > 200 {
		t.Errorf("expected len <= 200, got %d", len(got))
	}
}

func TestIsDevServerCommand(t *testing.T) {
	devs := []string{
		"npm run dev",
		"cd app && npm run dev",
		"yarn dev",
		"pnpm dev",
		"vite",
		"vite build --watch",
		"webpack serve",
		"next dev",
		"electron .",
	}
	for _, d := range devs {
		if !IsDevServerCommand(d) {
			t.Errorf("expected dev-server: %q", d)
		}
	}
	nondev := []string{
		"npm install",
		"make build",
		"git pull",
		"ls -la",
	}
	for _, d := range nondev {
		if IsDevServerCommand(d) {
			t.Errorf("expected NOT dev-server: %q", d)
		}
	}
}

func TestManagedRing_PushDedupesAndEvicts(t *testing.T) {
	var ring []string
	ring = managedRingPush(ring, "a", 3)
	ring = managedRingPush(ring, "b", 3)
	ring = managedRingPush(ring, "a", 3) // dup, no-op
	if len(ring) != 2 {
		t.Errorf("expected len 2 after dedup, got %d (%v)", len(ring), ring)
	}
	ring = managedRingPush(ring, "c", 3)
	ring = managedRingPush(ring, "d", 3) // should evict 'a'
	if len(ring) != 3 {
		t.Fatalf("expected len 3 at cap, got %d (%v)", len(ring), ring)
	}
	if managedRingContains(ring, "a") {
		t.Errorf("expected 'a' evicted, ring = %v", ring)
	}
	if !managedRingContains(ring, "d") {
		t.Errorf("expected 'd' present, ring = %v", ring)
	}
}

// TestStartManagedProcess_LifecycleEvents end-to-end smoke: spawn a
// fast-exiting bash command, verify "started" and "exited" events
// fire through the channel.
func TestStartManagedProcess_LifecycleEvents(t *testing.T) {
	ch := make(chan HostProcEvent, 8)
	s := &PlannerServices{HostProcEventCh: ch}
	mp, err := StartManagedProcess(s, "echo hello && sleep 0.1")
	if err != nil {
		t.Fatalf("StartManagedProcess: %v", err)
	}
	defer StopManagedProcess(s)
	if mp.Pid <= 0 {
		t.Errorf("expected positive Pid, got %d", mp.Pid)
	}
	got := drainEvents(ch, 3*time.Second, 2)
	kinds := make([]string, 0, len(got))
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	if !sliceContains(kinds, "started") || !sliceContains(kinds, "exited") {
		t.Errorf("expected started+exited events, got %v", kinds)
	}
}

func drainEvents(ch <-chan HostProcEvent, timeout time.Duration, want int) []HostProcEvent {
	var out []HostProcEvent
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// shutUnusedFmt silences the fmt import unused warning if no test
// uses it. Cheap; keeps the import for future tests.
var _ = fmt.Sprintf

func TestHostErrorIsLowSignal(t *testing.T) {
	low := []string{
		"[62416:0602/111350:ERROR:services/network/chunked_data_pipe_upload_data_stream.cc:212] OnSizeReceived failed with Error: -2",
		"net::ERR_FAILED",
		"GPU process exited unexpectedly",
		"socket hang up",
	}
	for _, l := range low {
		if !hostErrorIsLowSignal(l) {
			t.Errorf("expected low-signal: %q", l)
		}
	}
	actionable := []string{
		"TypeError: window.therapist.chat is not a function",
		"Error: No handler registered for 'therapist:chat'",
		"Cannot find module './missing.js'",
	}
	for _, l := range actionable {
		if hostErrorIsLowSignal(l) {
			t.Errorf("expected actionable (auto-dispatch): %q", l)
		}
	}
}

// TestStartManagedProcess_RunsInWorkspace is the regression for the bug where
// a managed "run it" executed in kit's process cwd instead of the active
// project (so it launched kit's own kai-desktop instead of the user's project).
// A relative-path write must land in the workspace, proving c.Dir is pinned.
func TestStartManagedProcess_RunsInWorkspace(t *testing.T) {
	ws := t.TempDir()
	ch := make(chan HostProcEvent, 16)
	s := &PlannerServices{HostProcEventCh: ch, MainRepo: ws}
	if _, err := StartManagedProcess(s, "echo ok > marker.txt"); err != nil {
		t.Fatalf("StartManagedProcess: %v", err)
	}
	defer StopManagedProcess(s)

	marker := filepath.Join(ws, "marker.txt")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // success: relative write landed in the workspace
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("marker.txt not created in workspace %s — managed process ran in the wrong cwd", ws)
}
