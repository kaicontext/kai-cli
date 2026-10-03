package main

import (
	"strings"
	"testing"
)

// The rules a review applies live in the review skill (reviewskill/), and a
// rule dropped in an edit fails silently: the reviewer simply stops looking.
// Pin each one by the behaviour it demands, so rewording is free and deletion
// is not.
func TestReviewSystemPrompt_KeepsTheHardWonRules(t *testing.T) {
	rules := []struct {
		name  string
		needs []string
	}{
		{name: "a test must fail without the fix", needs: []string{"fails with the fix removed", "the fix is unverified"}},
		{name: "mechanism configured is not behaviour verified", needs: []string{"asserts the mechanism was configured instead of the behaviour"}},
		{name: "a skipped test is not a passing test", needs: []string{"skips for an environmental reason"}},
		{name: "a defect needs a trigger that exists today", needs: []string{"# What counts as a defect", "**trigger**", "**failure mechanism**", "needs someone to change code first is not a defect", "cannot bind an array parameter", "never as an issue"}},
		{name: "wrong as written counts", needs: []string{"Wrong as written counts, even without a current caller"}},
		{name: "concurrency is reachable by default", needs: []string{"Concurrency is reachable by default"}},
		{name: "the change owns what it touches", needs: []string{"\"Pre-existing\" means code whose lines and enclosing function the change does not touch"}},
		{name: "generic advice is not a defect", needs: []string{"## What is not a defect", "Generic advice", "a specific requirement this change must meet that nothing verifies", "a concrete regression", "adds nothing that would fail without the fix", "that one stays a finding"}},
		{name: "every concrete defect is reported", needs: []string{"Keep going after the first finding", "tests and docs included", "A defect you saw and left out is lost"}},
		{name: "one cause, one issue", needs: []string{"## One cause, one issue", "(also: path:line, path:line)"}},
		{name: "decisions, and money has a direction", needs: []string{"## Decisions", "who is debited and who is credited", "never lowers the verdict"}},
		{name: "trace what crosses a call", needs: []string{"## 1. Contracts across a call", "Kind of value", "Shape", "Freshness", "a price in cents passed where dollars are expected", "Name both ends"}},
		{name: "two requests at once", needs: []string{"## 2. Shared state and concurrency", "run it twice at the same time", "loses one of two withdrawals", "an atomic update", "Name the two requests and the interleaving"}},
		{name: "sibling paths share guards", needs: []string{"Sibling paths share guards"}},
		{name: "secrets compare in constant time", needs: []string{"constant-time comparison"}},
		{name: "environment assumptions get named", needs: []string{"## 5. Resources and the environment", "The finding is the failure mode"}},
		{name: "config read from the wrong place", needs: []string{"Configuration read from the wrong place", "gitconfig"}},
		{name: "a deadline alone does not bound an exec", needs: []string{"Subprocesses that never return", "WaitDelay"}},
		{name: "hot-path cost", needs: []string{"Cost on a hot path"}},
		{name: "an optimisation must not be fatal", needs: []string{"An optional step that can abort the whole operation"}},
		{name: "unrequested writes get named", needs: []string{"Writes nobody asked for", "opt-out"}},
		{name: "an all-clear needs a boundary", needs: []string{"within this repository, the only caller is X"}},
		{name: "external facts get checked", needs: []string{"Repeating the author's premise back in your own voice is not review"}},
		{name: "graph tools are used", needs: []string{"kai_callers", "kai_dependents", "kai_context", "kai_web_search"}},
	}
	for _, r := range rules {
		for _, need := range r.needs {
			if !strings.Contains(rcReviewSystem, need) {
				t.Errorf("the reviewer stopped enforcing %q: %q is gone from the prompt", r.name, need)
			}
		}
	}
}

// A rule lands better with an example than with a warning: the categories
// where judgement is hardest each show a finding next to something that is
// not one.
func TestReviewCatalogShowsFindingsNextToNonFindings(t *testing.T) {
	catalog := rcSkillFile("catalog.md")
	for _, section := range []string{"## 1. Contracts across a call", "## 2. Shared state and concurrency", "## 3. Errors and absent values", "## 4. Inputs, security and access", "## 6. Tests"} {
		i := strings.Index(catalog, section)
		if i < 0 {
			t.Errorf("catalog lost %q", section)
			continue
		}
		rest := catalog[i+len(section):]
		if k := strings.Index(rest, "\n## "); k >= 0 {
			rest = rest[:k]
		}
		if !strings.Contains(rest, "> Finding:") || !strings.Contains(rest, "> Not a finding:") {
			t.Errorf("%s has no finding / not-a-finding example", section)
		}
	}
}

// The prompts must not carry Martian's Code Review Bench: its repositories,
// its pull requests or the defects its golden comments describe. Rules written
// around benchmark cases (kai-cli #142, 2026-09-25) taught the reviewer the
// answers to the test it is scored on; a rule needs an example from code the
// benchmark does not contain.
func TestPromptsCarryNoBenchmarkCases(t *testing.T) {
	fingerprints := []string{
		"Cal.com", "cal.com", "Keycloak", "keycloak", "Sentry", "Grafana", "Discourse", "discourse",
		"#14943", "#11059", "#10600", "#36880", "#37038", "#37429", "#8087", "#103633",
		"benchmark", "Prisma", "HubSpot", "Zoho", "jsforce", "refreshOAuthTokens", "retryCount",
		"backup code", "getClientId", "instance_url", "MANAGE_CLIENTS", "translation in the wrong language",
		"predicate method", "sleeps after patching",
	}
	prompts := map[string]string{
		"review": rcReviewSystem, "fast": rcFastReviewSystem, "sweep": rcSweepSystem,
		"challenge head": rcChallengeSystemHead, "challenge tail": rcChallengeSystemTail,
	}
	for name, prompt := range prompts {
		for _, f := range fingerprints {
			if strings.Contains(prompt, f) {
				t.Errorf("the %s prompt carries a benchmark case: %q", name, f)
			}
		}
	}
}
