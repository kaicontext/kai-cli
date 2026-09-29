package main

// Review profiles: one JSON file that sets the model and the reasoning effort
// of each review stage, so a benchmark run can try a combination without a
// deploy and without touching any other run's reviews.
//
// The file travels in the reviewed repository itself. The benchmark harness
// (kaicontextbench/harness) creates one repo per benchmark PR and commits the
// run's profile to the BASE branch, so every review in the run finds it and it
// is not part of the PR's diff: the reviewer and the scorer see the same
// change they always did. Two runs going at once are two sets of repos with
// two different files — there is no shared setting for them to fight over,
// which is what switching kailab-runner-config (every org) or an org's review
// config (every run in the org) could not avoid.
//
// A repository choosing its own review models is a benchmark tool, not a
// product feature, so the file is honored only in the benchmark org
// (rcProfileOwners), identified by GITHUB_REPOSITORY_FULLNAME, which the
// runner sets from the real repository and a repo cannot change. Everywhere
// else, and when no file is present, every stage runs exactly as before.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// The review stages a profile can set, in the order a review runs them.
const (
	// The fast pass: the one-call draft over the diff, then its fact-check.
	rcStageQuickDraft     = "quick_draft"
	rcStageQuickFactcheck = "quick_factcheck"
	// The grounded review: intent reconstruction, the agent, the
	// line-by-line sweep beside it, the publication fact-check, and the
	// conclusion written from the transcript when the agent ends without one.
	rcStageIntent     = "intent"
	rcStageMain       = "main"
	rcStageSweep      = "sweep"
	rcStageFactcheck  = "factcheck"
	rcStageConclusion = "conclusion"
)

var rcProfileStages = []string{
	rcStageQuickDraft, rcStageQuickFactcheck,
	rcStageIntent, rcStageMain, rcStageSweep, rcStageFactcheck, rcStageConclusion,
}

// rcProfilePath is where the harness commits the profile, on the base branch.
const rcProfilePath = ".kai-bench/review-profile.json"

// rcProfileOwners are the GitHub accounts whose repositories may carry a
// profile: the benchmark org, the account that ran the benchmark before it
// (acetz, again since 2026-09-30 while GitHub reviews the benchmark org's
// machine account), and nothing a customer controls.
var rcProfileOwners = map[string]bool{"kaicontextbench": true, "acetz": true}

// rcEffortOff is a profile's explicit "no reasoning": it overrides an effort
// the job sets for every stage (KAI_REVIEW_REASONING_EFFORT), where leaving
// the stage's effort out keeps it.
const rcEffortOff = "off"

// rcStageSetting is one stage's entry. An empty field keeps what the stage
// would use without a profile.
type rcStageSetting struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// rcReviewProfile is a parsed profile. Stages holds only the stages the file
// sets. Source says where it came from, for the log line that lets a
// benchmark confirm what each review ran on.
type rcReviewProfile struct {
	Source string
	Stages map[string]rcStageSetting
}

// rcActiveProfile is the profile this review runs under, nil for none. It is
// set once, before any model is resolved, by rcLoadProfileFor.
var rcActiveProfile *rcReviewProfile

// A model id as the proxy routes it (the server's own rule for pinned review
// models): letters, digits and . _ : / - only.
var rcProfileModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func rcValidEffort(e string) bool {
	switch e {
	case "", rcEffortOff, "minimal", "low", "medium", "high":
		return true
	}
	return false
}

// rcParseProfile parses and checks a profile. Top-level keys other than
// "stages" are the harness's own record (run tag, notes) and are ignored; an
// unknown stage or an unknown field inside one is an error, because a typo
// that silently fell back to the default would label a benchmark run with a
// model it never used.
func rcParseProfile(data []byte, source string) (*rcReviewProfile, error) {
	var top struct {
		Stages map[string]json.RawMessage `json:"stages"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("review profile %s: %w", source, err)
	}
	p := &rcReviewProfile{Source: source, Stages: map[string]rcStageSetting{}}
	known := map[string]bool{}
	for _, s := range rcProfileStages {
		known[s] = true
	}
	for name, raw := range top.Stages {
		if !known[name] {
			return nil, fmt.Errorf("review profile %s: unknown stage %q (stages: %s)", source, name, strings.Join(rcProfileStages, ", "))
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var s rcStageSetting
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("review profile %s: stage %s: %w", source, name, err)
		}
		s.Model = strings.TrimSpace(s.Model)
		s.Effort = strings.ToLower(strings.TrimSpace(s.Effort))
		if s.Model != "" && !rcProfileModelRe.MatchString(s.Model) {
			return nil, fmt.Errorf("review profile %s: stage %s: %q is not a model id", source, name, s.Model)
		}
		if !rcValidEffort(s.Effort) {
			return nil, fmt.Errorf("review profile %s: stage %s: effort %q must be off, minimal, low, medium or high", source, name, s.Effort)
		}
		p.Stages[name] = s
	}
	return p, nil
}

// rcCheckProfileEfforts refuses a profile effort that would be dropped. An
// effort only reaches the model on the OpenAI-shaped path (provider/openai.go);
// a bare claude-* id goes out on the Anthropic Messages shape, which carries
// none, and anthropic/claude-* is the spelling that carries it. It runs on the
// models every stage RESOLVED to, not the ones the file names: a stage the
// profile gives only an effort runs on whatever its fallback is (the job's
// KAI_SWEEP_MODEL, the review model, ...), and that can be a bare claude-*
// id as well. An effort that comes from the job alone
// (KAI_REVIEW_REASONING_EFFORT) is not the profile's to police.
func rcCheckProfileEfforts(models map[string]string) error {
	if rcActiveProfile == nil {
		return nil
	}
	for _, stage := range rcProfileStages {
		if !rcProfileSetsEffort(stage) || rcStageEffort(stage) == "" {
			continue
		}
		if m := models[stage]; strings.HasPrefix(strings.ToLower(m), "claude-") {
			return fmt.Errorf("review profile %s: stage %s runs on %s, whose bare id carries no reasoning effort; use anthropic/%s or effort off",
				rcActiveProfile.Source, stage, m, m)
		}
	}
	return nil
}

// rcProfileSetsEffort reports whether the stage's effort comes from the
// profile: its own entry, or main's for intent and conclusion.
func rcProfileSetsEffort(stage string) bool {
	if s, ok := rcProfileStage(stage); ok && s.Effort != "" {
		return true
	}
	if stage == rcStageIntent || stage == rcStageConclusion {
		if s, ok := rcProfileStage(rcStageMain); ok && s.Effort != "" {
			return true
		}
	}
	return false
}

// rcLoadProfileFor finds the profile for a review of base...ref and makes it
// the active one. KAI_REVIEW_PROFILE names a local file, for trying a profile
// by hand; the review job's runner does not pass it through. Otherwise the
// file is read from the base branch, and only in a benchmark-org repository.
// No file is no profile. A file that is present but broken is an error: a
// benchmark run that quietly fell back to the defaults would report numbers
// for a configuration it never ran. (Whether each stage's effort can reach its
// model is checked once the models resolve: rcCheckProfileEfforts.)
func rcLoadProfileFor(base string) error {
	rcActiveProfile = nil
	if path := strings.TrimSpace(os.Getenv("KAI_REVIEW_PROFILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("review profile: %w", err)
		}
		p, err := rcParseProfile(data, path)
		if err != nil {
			return err
		}
		rcActiveProfile = p
		return nil
	}
	repo := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY_FULLNAME"))
	owner, _, _ := strings.Cut(repo, "/")
	if base == "" || !rcProfileOwners[strings.ToLower(owner)] {
		return nil
	}
	// Trees are in the blobless clone the review job makes; ls-tree answers
	// "is it there" without fetching anything, so an absent file is told
	// apart from a fetch that failed.
	listed, err := exec.Command("git", "ls-tree", "--name-only", base, "--", rcProfilePath).Output()
	if err != nil || strings.TrimSpace(string(listed)) == "" {
		return nil
	}
	data, err := exec.Command("git", "show", base+":"+rcProfilePath).Output()
	if err != nil {
		return fmt.Errorf("review profile: reading %s from %s: %w", rcProfilePath, base, err)
	}
	p, err := rcParseProfile(data, repo+"@"+base+":"+rcProfilePath)
	if err != nil {
		return err
	}
	rcActiveProfile = p
	return nil
}

func rcProfileStage(stage string) (rcStageSetting, bool) {
	if rcActiveProfile == nil {
		return rcStageSetting{}, false
	}
	s, ok := rcActiveProfile.Stages[stage]
	return s, ok
}

// rcStageModel is the model a stage runs on: the profile's, else fallback
// (what the stage would use without one). Intent and conclusion have always
// run on the review model, so they follow the profile's main model when they
// set none of their own.
func rcStageModel(stage, fallback string) string {
	if s, ok := rcProfileStage(stage); ok && s.Model != "" {
		return s.Model
	}
	if stage == rcStageIntent || stage == rcStageConclusion {
		if s, ok := rcProfileStage(rcStageMain); ok && s.Model != "" {
			return s.Model
		}
	}
	return fallback
}

// rcStageEffort is the reasoning effort a stage asks for, "" for none.
//
// Without a profile nothing changes: the grounded stages take
// KAI_REVIEW_REASONING_EFFORT and the fast pass takes none, because its draft
// and fact-check share a 100-second budget. A profile's explicit setting wins
// either way, "off" included. Intent and conclusion follow main's setting
// when they have none, as they follow its model.
func rcStageEffort(stage string) string {
	resolve := func(s rcStageSetting) string {
		if s.Effort == rcEffortOff {
			return ""
		}
		return s.Effort
	}
	if s, ok := rcProfileStage(stage); ok && s.Effort != "" {
		return resolve(s)
	}
	if stage == rcStageIntent || stage == rcStageConclusion {
		if s, ok := rcProfileStage(rcStageMain); ok && s.Effort != "" {
			return resolve(s)
		}
	}
	switch stage {
	case rcStageQuickDraft, rcStageQuickFactcheck:
		return ""
	}
	return rcReasoningEffort()
}

// rcReasoningMinTokens is the output limit a call gets when it asks for a
// reasoning effort. Hidden reasoning counts against max_tokens, and the calls
// with small limits were sized for a model that does not think: the fast
// pass's draft (2000), intent (600) and the conclusion (2500). The proxy
// floors GLM-family calls at 4096 on its own, but not Claude's. The first
// smoke run with the draft on anthropic/claude-haiku-4-5 at effort low spent
// all 2000 tokens and posted no quick pass.
const rcReasoningMinTokens = 8000

// rcTokensFor is maxTokens, raised to rcReasoningMinTokens when the call
// carries an effort. Without one nothing changes.
func rcTokensFor(maxTokens int, effort string) int {
	if effort != "" && maxTokens < rcReasoningMinTokens {
		return rcReasoningMinTokens
	}
	return maxTokens
}

// rcFastChallengeModel is the fast pass's fact-check model: the profile's
// quick_factcheck, else the grounded fact-check's (rcChallengeModel), which is
// what it has always shared.
func rcFastChallengeModel(reviewModel string) string {
	return rcStageModel(rcStageQuickFactcheck, rcChallengeModel(reviewModel))
}

// rcDescribeProfile is the log line that says what the active profile set
// each stage to, resolved: a benchmark reads it to confirm what a review ran
// on. Empty without a profile.
func rcDescribeProfile(models map[string]string) string {
	if rcActiveProfile == nil {
		return ""
	}
	names := make([]string, 0, len(rcActiveProfile.Stages))
	for n := range rcActiveProfile.Stages {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "  review profile %s (%s):", rcActiveProfile.Source, strings.Join(names, ", "))
	for _, stage := range rcProfileStages {
		effort := rcStageEffort(stage)
		if effort == "" {
			effort = rcEffortOff
		}
		fmt.Fprintf(&b, " %s=%s/%s", stage, models[stage], effort)
	}
	return b.String()
}
