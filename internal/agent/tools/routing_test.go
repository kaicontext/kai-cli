package tools

import (
	"strings"
	"sync"
	"testing"
)

func TestTraceRouting_NoTracer(t *testing.T) {
	// Default state: no tracer installed. Calls must be safe and
	// silent.
	ClearRoutingTracer()
	TraceRouting("nothing should crash: %s", "ok") // should not panic
}

func TestTraceRouting_Installed(t *testing.T) {
	var mu sync.Mutex
	var got []string
	SetRoutingTracer(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg)
	})
	defer ClearRoutingTracer()

	TraceRouting("file in=%q → project=%s", "kai-server/foo.go", "kai-server")
	TraceRouting("kai_grep query=%q scope=*", "BraveAPIKey")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("want 2 trace lines, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "kai-server/foo.go") || !strings.Contains(got[0], "→ project=kai-server") {
		t.Errorf("first trace malformed: %q", got[0])
	}
	if !strings.Contains(got[1], "BraveAPIKey") || !strings.Contains(got[1], "scope=*") {
		t.Errorf("second trace malformed: %q", got[1])
	}
}

func TestTraceRouting_ClearStops(t *testing.T) {
	var calls int
	SetRoutingTracer(func(msg string) { calls++ })
	TraceRouting("first")
	ClearRoutingTracer()
	TraceRouting("second")
	if calls != 1 {
		t.Errorf("expected exactly 1 call after Clear, got %d", calls)
	}
}

func TestSetRoutingTracer_NilClears(t *testing.T) {
	var calls int
	SetRoutingTracer(func(msg string) { calls++ })
	TraceRouting("first")
	SetRoutingTracer(nil) // nil should be equivalent to ClearRoutingTracer
	TraceRouting("second")
	if calls != 1 {
		t.Errorf("expected exactly 1 call after SetRoutingTracer(nil), got %d", calls)
	}
}
