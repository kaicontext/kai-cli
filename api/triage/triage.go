// Package triage is the TUI's re-export of kai-cli/internal/triage.
package triage

import engine "kai/internal/triage"

type Request = engine.Request
type Result = engine.Result
type Sender = engine.Sender
type SenderRequest = engine.SenderRequest
type SenderResponse = engine.SenderResponse

const (
	TrackAnswer  = engine.TrackAnswer
	TrackPlan    = engine.TrackPlan
	TrackClarify = engine.TrackClarify
	TrackHost    = engine.TrackHost
)

var Classify = engine.Classify
