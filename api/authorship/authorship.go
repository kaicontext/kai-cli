// Package authorship is the TUI's re-export of kai-cli/internal/authorship.
package authorship

import engine "kai/internal/authorship"

type CheckpointWriter = engine.CheckpointWriter

var NewCheckpointWriter = engine.NewCheckpointWriter
