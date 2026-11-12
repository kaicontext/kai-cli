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

// rcConclusionSuffix names a finder's own conclusion call, written from its
// transcript when it ends without one: finder2_conclusion,
// spec_logic_conclusion, ... The main finder's is rcStageConclusion. Each
// runs on its finder's model, so one shared label would mix models.
const rcConclusionSuffix = "_conclusion"

// rcFinderConclusionStages are those labels, one per extra finder.
func rcFinderConclusionStages() []string {
	out := []string{rcStageFinder2 + rcConclusionSuffix}
	for _, s := range rcSpecialistStages {
		out = append(out, s+rcConclusionSuffix)
	}
	return out
}

// rcCostStages is every label a review's model calls carry. The server
// accepts 1 to 40 characters of [a-z0-9_]; anything else is stored blank.
var rcCostStages = append(append(append([]string(nil), rcProfileStages...),
	rcCostStageCoverageGate, rcCostStageGroup, rcCostStageDescription),
	rcFinderConclusionStages()...)

type rcStageKey struct{}

// rcCostStage labels the model calls made under ctx with a review stage:
// billed under it on the server (X-Kai-Stage), and reported under it in the
// bundle's "run" object (review_commit_run.go), which reads it back with
// rcStageOf. One label for both, so the two never disagree.
func rcCostStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(provider.WithCostStage(ctx, stage), rcStageKey{}, stage)
}
