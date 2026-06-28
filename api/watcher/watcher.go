// Package watcher is the TUI's re-export of kai-cli/internal/watcher.
package watcher

import engine "github.com/kaicontext/kai-engine/watcher"

type Watcher = engine.Watcher

var New = engine.New
