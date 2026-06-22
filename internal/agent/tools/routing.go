package tools

import (
	"fmt"
	"sync/atomic"
)

// Per-process routing tracer. The runner installs a tracer at session
// start (wired to the planner/chat debug log); every routing-sensitive
// tool calls TraceRouting() at the dispatch point to record where it
// went. The output is the single best signal for diagnosing multi-root
// failures: did `view kai-server/foo.go` actually land in kai-server,
// or did it silently resolve to kai/kai-server/foo.go and 404?
//
// Stored as an atomic.Pointer so concurrent runner sessions don't race
// on the slot. Reading is a single atomic load; the cost per dispatch
// when no tracer is set is one nil-check.
var routingTracer atomic.Pointer[func(string)]

// SetRoutingTracer installs the per-session tracer. Pass nil to clear.
// The runner pairs Set on startup with a deferred Clear so a tracer
// from a previous session can't leak into the next one.
func SetRoutingTracer(fn func(string)) {
	if fn == nil {
		routingTracer.Store(nil)
		return
	}
	routingTracer.Store(&fn)
}

// ClearRoutingTracer is the explicit form of SetRoutingTracer(nil).
// Lets callers defer the cleanup without constructing a nil literal.
func ClearRoutingTracer() {
	routingTracer.Store(nil)
}

// TraceRouting records a single routing decision. format/args follow
// fmt.Sprintf. Safe and cheap (one atomic load + nil-check) when no
// tracer is installed.
func TraceRouting(format string, args ...any) {
	p := routingTracer.Load()
	if p == nil {
		return
	}
	(*p)(fmt.Sprintf(format, args...))
}
