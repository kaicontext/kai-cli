package main

import (
	"github.com/kaicontext/kai-engine/finding"
	"strings"
	"testing"
)

// The fixture from kai-server#434/#437/#440.
var rcNitFixture = map[string][]string{
	"kailab-control/internal/smoketest/excerpt.go": strings.Split(`// Package smoketest is a throwaway change used to exercise the PR reviewer.
// It is not wired into anything. Do not merge.
package smoketest

import "strings"

// Excerpt returns at most limit characters of text, followed by "…" when it
// had to cut. The max argument must be positive.
func Excerpt(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}`, "\n"),
	"build.go": {"//go:build linux", "package x"},
}

func rcNitRead(path string) ([]string, bool) { l, ok := rcNitFixture[path]; return l, ok }

// The doc-comment point came out as a 🐞 three runs in a row; by where it is,
// it is a nit. A point on code, or on a build directive, is not.
func TestCommentLinePointsBecomeNits(t *testing.T) {
	raw := "Prose.\n" + rcReviewDataMarker + "\nINTENT_MATCH: verified\nMERGE_READY: 3\nISSUES:\n" +
		"- kailab-control/internal/smoketest/excerpt.go:8 — doc comment refers to a `max` argument but the parameter is `limit`\n" +
		"- kailab-control/internal/smoketest/excerpt.go:14 — `text[:limit]` panics on a negative `limit`\n" +
		"- kailab-control/internal/smoketest/excerpt.go:7 — the doc says characters (also: kailab-control/internal/smoketest/excerpt.go:14)\n" +
		"- build.go:1 — the build tag excludes darwin\n" +
		"- kailab-control/internal/smoketest/excerpt.go:8 — the doc names `max` (also: kailab-control/internal/smoketest/excerpt.go:7) — compare build.go:2\n" +
		"- nit: kailab-control/internal/smoketest/excerpt.go:1 — already a nit\n" +
		"DECISIONS:\n- kailab-control/internal/smoketest/excerpt.go:8 — not an issue bullet\n"
	got := rcMarkCommentNits(raw, rcNitRead)
	_, issues, decisions, _, _, _ := rcParseReviewOutput(got)
	want := []bool{true, false, false, false, true, true}
	if len(issues) != len(want) {
		t.Fatalf("issues = %q", issues)
	}
	for i, w := range want {
		if _, nit := rcSplitNit(issues[i]); nit != w {
			t.Errorf("issue %d nit=%v, want %v: %q", i, nit, w, issues[i])
		}
	}
	if strings.Count(got, "nit: nit:") != 0 {
		t.Error("an existing nit was marked twice")
	}
	if len(decisions) != 1 || strings.HasPrefix(decisions[0], "nit:") {
		t.Errorf("a decision was touched: %q", decisions)
	}
}

// The sweep marks nits too; its parser used to read "nit" as the path and
// drop every one of them.
func TestSweepKeepsNits(t *testing.T) {
	out := "ISSUES:\n- nit: a.go:3 — the comment names the wrong field\n- a.go:9 — `n` is never checked\n- nit: other.go:1 — not in this change\n"
	got := rcSweepIssues(out, map[string]bool{"a.go": true})
	if len(got) != 2 || got[0] != "nit: a.go:3 — the comment names the wrong field" || got[1] != "a.go:9 — `n` is never checked" {
		t.Errorf("sweep issues = %q", got)
	}
}

// A confirmed nit is published and does not cap the score: a review whose
// only confirmed point is a nit keeps the 5 the challenger proposed.
func TestConfirmedNitDoesNotCapReadiness(t *testing.T) {
	a := rcCDChecks()
	a.MergeReady = 5
	a.Checks = a.Checks[:1] // the refuted cd claim
	nit := "nit: " + rcEscapeIssue
	a.Checks = append(a.Checks, rcIssueCheck{Issue: nit, Verdict: "supported", Reason: "The comment is wrong.", Finding: "f", Evidence: []rcCheckEvidence{{Source: 2, LineStart: 1, LineEnd: 1}}})
	res, _, err := rcValidateChallenge(rcTestAnswer(t, a), []string{rcFalseCDIssue, nit}, nil, rcCDSources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Review, nit) || !strings.Contains(res.Review, "MERGE_READY: 5") {
		t.Errorf("want the nit published and MERGE_READY 5:\n%s", res.Review)
	}
}

// Lines tools read are not comments: a point about one is not a nit.
func TestDirectivesAreNotComments(t *testing.T) {
	for _, src := range []string{"#!/bin/sh", "#!/usr/bin/env python3", "//go:build linux", "// +build linux", "//nolint:errcheck", "# type: ignore", "# noqa: E501", "#include <stdio.h>", "/* eslint-disable */", "// @ts-expect-error"} {
		if rcIsCommentLine(src) {
			t.Errorf("%q was taken for a plain comment", src)
		}
	}
	for _, src := range []string{"// Excerpt returns at most limit characters", "# Retries three times", " * the max argument", "-- counts rows"} {
		if !rcIsCommentLine(src) {
			t.Errorf("%q was not taken for a comment", src)
		}
	}
}

// The batched path re-caps readiness in rcFinalize; a confirmed nit must not
// cap it there either (Kai's review of kai-cli#169 found the guard missing).
func TestFinalizeDoesNotCapOnANit(t *testing.T) {
	res := &rcChallengeResult{
		Allegations: []rcAllegationResult{
			{ID: 1, Issue: "nit: a.go:3 — the comment names `max`", Status: rcStatusSupported, Finding: "f"},
			{ID: 2, Issue: "a.go:9 — `n` is unchecked", Status: rcStatusRefuted, Reason: "it is checked"},
		},
		match:    finding.MatchVerified,
		proposed: finding.Readiness(5),
	}
	rcFinalize(res)
	if !strings.Contains(res.Review, "MERGE_READY: 5") {
		t.Errorf("a confirmed nit capped the batched review:\n%s", res.Review)
	}
	res.Allegations = append(res.Allegations, rcAllegationResult{ID: 3, Issue: "a.go:12 — `x` is nil", Status: rcStatusSupported, Finding: "g"})
	rcFinalize(res)
	if !strings.Contains(res.Review, "MERGE_READY: 3") {
		t.Errorf("a confirmed defect did not cap the batched review:\n%s", res.Review)
	}
}
