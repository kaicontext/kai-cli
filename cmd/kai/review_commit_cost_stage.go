package main

import (
	"context"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/provider"
)

// setCostClientVersion puts this build's version on the run-cost reports
// the engine uploads, so cost can be split by kai version.
func setCostClientVersion() {
	agent.SetClientVersion(Version)
}

// Cost labels for the review's model calls that are not profile stages.
// The server stores each call's label so cost can be split by stage; a
// label changes nothing about the request itself.
const (
	// The coverage gate resumes the main agent to open changed files it
	// never read.
	rcCostStageCoverageGate = "coverage_gate"
	// The grouping call folds confirmed issues that repeat one another.
	rcCostStageGroup = "group"
	// The PR description written beside the review.
	rcCostStageDescription = "description"
)

// rcCostStages is every label a review's model calls carry. The server
// accepts 1 to 40 characters of [a-z0-9_]; anything else is stored blank.
var rcCostStages = append(append([]string(nil), rcProfileStages...),
	rcCostStageCoverageGate, rcCostStageGroup, rcCostStageDescription)

// rcCostStage labels the model calls made under ctx with a review stage.
func rcCostStage(ctx context.Context, stage string) context.Context {
	return provider.WithCostStage(ctx, stage)
}
