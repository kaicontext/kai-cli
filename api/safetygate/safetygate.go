// Package safetygate is the TUI's re-export of kai-cli/internal/safetygate.
package safetygate

import engine "kai/internal/safetygate"

type Config = engine.Config

const (
	Auto   = engine.Auto
	Block  = engine.Block
	Review = engine.Review
)

var ListHeld = engine.ListHeld
