package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/kaicontext/kai-engine/provider"
)

// rcIncompleteReason is the machine-readable half of an unfinished review:
// which stage gave up, what kind of failure stopped it, and on which model.
//
// It exists because the prose (rcIncompleteProse) is written for the person on
// the pull request and drops the error, and the error itself goes to stderr,
// which only the job log keeps. The server classified unfinished reviews by
// searching that prose for fixed sentences, so the best it could log was
// "fact_check_failed", never whether that was a 429, a timeout or an answer it
// could not parse (falkordb/falkordb#3049, 2026-10-03).
//
// Every field is a fixed vocabulary or a model id, never error text: the error
// can quote the provider's response body, and the bundle is stored and logged.
type rcIncompleteReason struct {
	// Stage is where the run stopped: "challenge" (the publication gate could
	// not check the draft), "conclusion" (the run wrote nothing down and the
	// request for a conclusion failed) or "review" (it wrote nothing down and
	// no conclusion was asked for).
	Stage string `json:"stage"`
	// Category is rcFailureCategory of the stage's error.
	Category string `json:"category,omitempty"`
	// Model is the model the failing stage called.
	Model string `json:"model,omitempty"`
	// FinishReason is how the review agent's own run ended.
	FinishReason string `json:"finishReason,omitempty"`
}

// rcProseRunFailed is the sentence an outright run failure writes. kai-server
// reads it (cliProseRunFailed) to class a bundle from a kai-cli whose reason it
// does not parse, so it is named once and a test holds it.
const rcProseRunFailed = "The review could not get an answer from the model"

const (
	rcStageNameChallenge  = "challenge"
	rcStageNameConclusion = "conclusion"
	rcStageNameReview     = "review"
)

// rcIncompleteReasonOf is the bundle's record of why inc did not finish. Nil
// for a run that left no facts about its ending.
func rcIncompleteReasonOf(inc *rcIncomplete) *rcIncompleteReason {
	if inc == nil {
		return nil
	}
	r := &rcIncompleteReason{FinishReason: inc.FinishReason}
	switch {
	case inc.RunFailure != "":
		r.Stage, r.Category, r.Model = rcStageNameReview, inc.RunCategory, inc.Model
	case inc.ChallengeFailure != "":
		r.Stage, r.Category, r.Model = rcStageNameChallenge, inc.ChallengeCategory, inc.ChallengeModel
	case inc.ConclusionCategory != "":
		r.Stage, r.Category, r.Model = rcStageNameConclusion, inc.ConclusionCategory, inc.ConclusionModel
	default:
		r.Stage, r.Model = rcStageNameReview, inc.Model
	}
	return r
}

// rcUpstreamStatusRe reads the HTTP status out of a kai-engine provider error,
// which carries it only in its text: "kailab provider: 503: …",
// "openrouter provider: 429: …", "kailab provider (openai stream): 500: …".
var rcUpstreamStatusRe = regexp.MustCompile(`provider(?: \([a-z ]+\))?: (\d{3}):`)

// rcFailureCategory places a stage's error in a fixed vocabulary. Typed checks
// first; the provider's status code from its message where there is no type;
// then the gate's own failures, which this package writes and a test holds to.
func rcFailureCategory(err error) string {
	switch {
	case err == nil:
		return ""
	case provider.IsCapExceeded(err):
		return "usage_limit"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case provider.IsContextOverflow(err):
		return "context_overflow"
	}
	if m := rcUpstreamStatusRe.FindStringSubmatch(err.Error()); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch {
		case code == 429:
			return "rate_limited"
		case code >= 500:
			return "upstream_5xx"
		default:
			return "upstream_4xx"
		}
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "parsing response"):
		return "unparseable"
	case provider.IsTransient(err):
		return "upstream_network"
	case strings.Contains(msg, "was truncated at"):
		return "truncated"
	case strings.Contains(msg, "evidence exceeds"):
		return "evidence_too_large"
	case strings.Contains(msg, "past its limit"),
		strings.Contains(msg, "unavailable tool"),
		strings.Contains(msg, "before its pending experiments"):
		return "tool_loop"
	case strings.Contains(msg, "ended without a complete answer"):
		return "no_answer"
	}
	return "other"
}
