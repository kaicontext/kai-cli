// Package gatereview is the TUI's re-export of kai-cli/internal/gatereview.
package gatereview

import engine "kai/internal/gatereview"

type Result = engine.Result
type Issue = engine.Issue
type Inputs = engine.Inputs
type FixInputs = engine.FixInputs
type FixResult = engine.FixResult
type Recommendation = engine.Recommendation

const (
	RecApprove        = engine.RecApprove
	RecReject         = engine.RecReject
	RecAsk            = engine.RecAsk
	RecFixThenApprove = engine.RecFixThenApprove
)

var (
	Review            = engine.Review
	Fix               = engine.Fix
	HeldSnapshotDiff  = engine.HeldSnapshotDiff
)
