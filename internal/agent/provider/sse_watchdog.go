package provider

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// streamIdleTimeout is the HARD threshold: at this point we close the
// body to abort the stream, surfacing a real error instead of a
// silent hang. 60s is well above Anthropic / OpenAI's ~15s SSE
// keepalive cadence, so a healthy stream never trips it.
const streamIdleTimeout = 60 * time.Second

// streamSoftIdleThreshold is the OBSERVABILITY threshold: we emit a
// PhaseStreamIdle state event once the stream has been quiet this
// long, so the TUI can show "stream idle for Ns" instead of just
// silence. Bytes resuming emits PhaseStreaming again, so the TUI
// can flip the label back to "active." Set well below the hard
// timeout so the user sees the soft signal before any abort.
const streamSoftIdleThreshold = 10 * time.Second

// sseWatchdog spawns a goroutine that monitors stream activity. Two
// thresholds, both running off the same lastActivity timestamp:
//
//   - softIdle (streamSoftIdleThreshold): emit a PhaseStreamIdle
//     state event once. Bytes resuming triggers a PhaseStreaming
//     event so the TUI can flip back to "active". Observability
//     only — no abort.
//   - hardIdle (idleTimeout): close resp.Body to abort the stream.
//     Surfaced as an error after the scanner unblocks.
//
// Returns:
//   - bumpActivity: call from the scanner loop after each Scan() to
//     reset both timers. If a soft-idle event has already fired,
//     emits PhaseStreaming via onState before returning.
//   - stop: defer this; cancels the watchdog goroutine.
//   - timedOut: true after stop() if the hard threshold fired.
func sseWatchdog(resp *http.Response, idleTimeout time.Duration, onState func(RequestState)) (
	bumpActivity func(),
	stop func(),
	timedOut func() bool,
) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var hardFired atomic.Bool
	// softFired tracks whether we've emitted the most recent
	// PhaseStreamIdle. Reset by bumpActivity when bytes resume so
	// a stream that idles → active → idles again emits the event
	// each time.
	var softFired atomic.Bool
	done := make(chan struct{})

	go func() {
		// 1s ticks: fine-grained enough that "60s idle" triggers
		// within a second of the threshold, cheap on the goroutine
		// scheduler.
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				idle := time.Duration(now.UnixNano() - lastActivity.Load())
				if idle > idleTimeout {
					hardFired.Store(true)
					_ = resp.Body.Close()
					return
				}
				if idle > streamSoftIdleThreshold && !softFired.Load() {
					softFired.Store(true)
					emitStreamIdle(onState, idle)
				}
			}
		}
	}()

	bumpActivity = func() {
		lastActivity.Store(time.Now().UnixNano())
		// If we'd previously emitted a soft-idle event, the user's
		// TUI is showing "stream idle Xs". Tell it the stream is
		// back so the label flips to "streaming".
		if softFired.CompareAndSwap(true, false) && onState != nil {
			onState(RequestState{
				Phase: PhaseStreaming,
				When:  time.Now(),
			})
		}
	}
	stop = func() {
		select {
		case <-done:
			// already closed (idempotent stop)
		default:
			close(done)
		}
	}
	timedOut = func() bool { return hardFired.Load() }
	return bumpActivity, stop, timedOut
}

// handleKaiStateFrame parses one `event: kai_state` SSE frame body
// (the JSON after `data: `) and forwards it as a RequestState event.
// Kailab injects these frames into the streamed response to surface
// upstream-call lifecycle the client otherwise can't see.
//
// Wire format:
//
//	event: kai_state
//	data: {"phase":"upstream_sent","detail":"POST https://..."}
//
// Phase strings match the upstream_* RequestPhase constants 1:1 so
// no translation is needed. Unknown phases are forwarded verbatim
// (they'll render as their string value) — forward-compatible with
// future server-side phases.
//
// Best-effort: a malformed payload silently no-ops. Worst case the
// user loses one observability frame, never a real event.
func handleKaiStateFrame(data string, onState func(RequestState)) {
	if onState == nil {
		return
	}
	var frame struct {
		Phase  string `json:"phase"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return
	}
	if frame.Phase == "" {
		return
	}
	onState(RequestState{
		Phase:  RequestPhase(frame.Phase),
		Detail: frame.Detail,
		When:   time.Now(),
	})
}
