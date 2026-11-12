package main

// review_commit_partial.go — a review that says when part of it failed.
//
// Several failures used to leave a review that looked whole: the main finder
// ending with almost nothing read, a draft with no verdict block (which threw
// the sweep's findings away), the second finder standing in for the main one,
// sweep chunks lost, a finder or the selection step failing. Each now leaves a
// reason here, and the bundle carries "partial" and "partialReasons" when the
// server asks for them (KAI_REVIEW_PARTIAL).
//
// The reviewer only records. It never words a partial review differently:
// whether users see "Kai could only finish part of this review" is the
// server's choice (the partial_reviews setting), so a week of record-only
// data can show how common each reason is before anyone sees the label.
//
// "partial" is distinct from "incomplete", which means no review at all.

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// KAI_REVIEW_PARTIAL positions. Off (or unset) is today's review, with no
// partial fields in the bundle; record and show behave the same here.
const (
	rcPartialOff    = "off"
	rcPartialRecord = "record"
	rcPartialShow   = "show"
)

var rcPartialWarnOnce sync.Once

// rcPartialMode is KAI_REVIEW_PARTIAL: off, record or show. Anything else is
// off, with a warning.
func rcPartialMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KAI_REVIEW_PARTIAL")))
	switch v {
	case rcPartialRecord, rcPartialShow:
		return v
	case "", rcPartialOff:
		return rcPartialOff
	}
	rcPartialWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "  warning: KAI_REVIEW_PARTIAL=%q is not off, record or show; treating it as off\n", v)
	})
	return rcPartialOff
}

// rcPartialOn reports whether the review records partial reasons and handles
// a draft with no verdict block honestly (review_commit_retry.go).
func rcPartialOn() bool { return rcPartialMode() != rcPartialOff }

// Partial reasons. Each makes the review partial unless rcPartialNoteOnly
// lists it. A finder that failed is "<its stage>_failed": finder2_failed,
// spec_logic_failed, ...
const (
	// The main finder ended without a verdict and its conclusion was written
	// from fewer than rcPartialMinTurns turns or with no file read.
	rcPartialThinConclusion = "thin_conclusion"
	// No finder wrote a usable verdict block.
	rcPartialNoCoda = "no_coda"
	// The second finder's review stood in for the main finder's.
	rcPartialFinder2StoodIn = "finder2_stood_in"
	// A sweep chunk failed or came back empty in every sweep that read it.
	rcPartialSweepFailed = "sweep_failed"
	// Files past the sweep's chunk cap were never swept.
	rcPartialFilesUnswept = "files_unswept"
	// The selection step failed: every confirmed finding was published and
	// the agreement check was off.
	rcPartialRankFailed = "rank_failed"

	// Recorded only: alone they do not make a review partial.
	rcPartialFailover          = "failover"
	rcPartialIntentEmpty       = "intent_empty"
	rcPartialGroupFailed       = "group_failed"
	rcPartialHumanizeFailed    = "humanize_failed"
	rcPartialDescriptionFailed = "description_failed"
)

// rcPartialMinTurns is the fewest turns a main finder must have taken for a
// conclusion written from its transcript to count as a review of the change.
// A first guess, to be tuned from the record-only week.
const rcPartialMinTurns = 3

// rcPartialNoteOnly are the reasons that are recorded but do not by
// themselves make a review partial.
var rcPartialNoteOnly = map[string]bool{
	rcPartialFailover:          true,
	rcPartialIntentEmpty:       true,
	rcPartialGroupFailed:       true,
	rcPartialHumanizeFailed:    true,
	rcPartialDescriptionFailed: true,
}

// rcFinderFailed is the reason for a finder stage that failed or timed out.
func rcFinderFailed(stage string) string { return stage + "_failed" }

// rcPartialLog collects reasons. Stages run concurrently, so it locks.
type rcPartialLog struct {
	mu      sync.Mutex
	reasons []string
}

// rcPartial is this process's log; rcRunStart resets it with the run report.
var rcPartial = &rcPartialLog{}

func rcPartialReset() {
	rcPartial.mu.Lock()
	rcPartial.reasons = nil
	rcPartial.mu.Unlock()
}

// rcNotePartial records a reason, once.
func rcNotePartial(reason string) {
	rcPartial.mu.Lock()
	defer rcPartial.mu.Unlock()
	for _, r := range rcPartial.reasons {
		if r == reason {
			return
		}
	}
	rcPartial.reasons = append(rcPartial.reasons, reason)
}

// rcPartialReasons is the reasons so far, in the order they were noted.
func rcPartialReasons() []string {
	rcPartial.mu.Lock()
	defer rcPartial.mu.Unlock()
	return append([]string{}, rcPartial.reasons...)
}

// rcIsPartial reports whether any reason makes the review partial.
func rcIsPartial(reasons []string) bool {
	for _, r := range reasons {
		if !rcPartialNoteOnly[r] {
			return true
		}
	}
	return false
}

// rcPartialFields is what the bundle carries: nil and nil when
// KAI_REVIEW_PARTIAL is off, so the bundle is exactly today's; otherwise
// "partial" and the reasons, both always present. A failover recorded in the
// run report counts as a reason.
func rcPartialFields(run *rcRunReport) (*bool, *[]string) {
	if !rcPartialOn() {
		return nil, nil
	}
	if run != nil && len(run.Failovers) > 0 {
		rcNotePartial(rcPartialFailover)
	}
	reasons := rcPartialReasons()
	partial := rcIsPartial(reasons)
	return &partial, &reasons
}

// rcDescriptionFailed reports whether a review that asked for a description
// ended without one (description_failed). The fast pass waits only briefly
// and its description is often not ready yet, which is not a failure: the
// grounded pass writes it, and records its own outcome. An incomplete review
// never collects one.
func rcDescriptionFailed(asked, incomplete, fast bool, description string) bool {
	return asked && !incomplete && !fast && description == ""
}
