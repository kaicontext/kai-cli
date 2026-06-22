// Package watcher is the TUI's re-export of kai-cli/internal/watcher.
package watcher

import engine "kai/internal/watcher"

type Watcher = engine.Watcher

var New = engine.New
