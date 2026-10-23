package main

import "sync"

// livePushQueue feeds locally-changed paths to the live-sync engine's push
// side as soon as the watcher has processed them.
//
// `kai live run` used to push from the watcher's OnActivity callback, which
// the watcher fires on a 30-second ticker with every file touched in the last
// five minutes. Every local edit therefore waited 0–30s (mean ~15s) before it
// left the machine, while peer edits arrived over SSE in milliseconds — the
// asymmetric "2s one way, 21s the other" latency. The queue is driven from
// OnUpdate (fired per file after the watcher's 100ms debounce) instead.
//
// Pushes run on the queue's own goroutine so network I/O never blocks the
// watcher, and paths that change while a push is in flight are coalesced into
// the next batch. In checkpoint mode nothing is pushed until Drain is called
// by an explicit checkpoint.
type livePushQueue struct {
	mu         sync.Mutex
	dirty      map[string]bool
	kick       chan struct{}
	checkpoint bool
}

func newLivePushQueue(checkpoint bool) *livePushQueue {
	return &livePushQueue{
		dirty:      make(map[string]bool),
		kick:       make(chan struct{}, 1),
		checkpoint: checkpoint,
	}
}

// Add records a changed path and, in live mode, wakes the pusher.
func (q *livePushQueue) Add(path string) {
	q.mu.Lock()
	q.dirty[path] = true
	q.mu.Unlock()
	if q.checkpoint {
		return
	}
	select {
	case q.kick <- struct{}{}:
	default: // a wake-up is already pending; it will pick this path up
	}
}

// Drain returns and clears every pending path.
func (q *livePushQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	paths := make([]string, 0, len(q.dirty))
	for p := range q.dirty {
		paths = append(paths, p)
	}
	q.dirty = make(map[string]bool)
	return paths
}

// Run pushes pending paths whenever Add signals, until stop is closed.
func (q *livePushQueue) Run(stop <-chan struct{}, push func([]string)) {
	for {
		select {
		case <-stop:
			return
		case <-q.kick:
			if paths := q.Drain(); len(paths) > 0 {
				push(paths)
			}
		}
	}
}
