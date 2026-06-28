// Package authorship is the TUI's re-export of kai-cli/internal/authorship.
package authorship

import engine "github.com/kaicontext/kai-engine/authorship"

type CheckpointWriter = engine.CheckpointWriter

var NewCheckpointWriter = engine.NewCheckpointWriter
