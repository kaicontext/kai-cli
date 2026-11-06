package main

// Risk routing: a low-risk change is reviewed on a cheaper review profile.
//
// The review job can name two profiles: KAI_REVIEW_PROFILE for every change and
// KAI_REVIEW_PROFILE_LOW_RISK for a change rated at one of the levels in
// KAI_REVIEW_LOW_RISK_LEVELS (default S) before the review starts. The rating has to come before any model runs, so it is the part of
// kai-server's risk label (kailab-control/internal/api/review_size_risk.go,
// changeRisk) that reads only the change: where it lands, what it changes and
// how many files depend on it directly. The server's label also reads the
// review's own decisions ("Decision: [money] ..."), which do not exist yet; a
// change this rates S can still be labelled M or L on the published review.
//
// Benchmarked 2026-11-05 on the Martian bench (50 PRs, saved judge verdicts):
// 20 of 50 PRs rate S; reviewing those on the hybrid profile (run50's Sonnet
// finder and Opus rank, the fact-check, second finder and second sweep on
// Doubleword) and the rest on run50 scored Opus-judge F1 53.6 against 56.4 for
// run50 everywhere, at about $2.22 a review against $2.90.
//
// 2026-11-06: deploy config (Helm, k8s, Terraform, deploy/) rates M instead of
// L, and "session" no longer counts as auth. Re-rated, the first 81 production
// reviews go from 43 S / 11 M / 27 L to 43 S / 27 M / 11 L. With the job routing
// M to the low-risk profile too (KAI_REVIEW_LOW_RISK_LEVELS=S,M), that is about
// $1.58 a review against $2.24; the bench scored routing S and M that way at
// Opus-judge F1 52.2.

import (
	"fmt"
	"path"
	"strings"

	"github.com/kaicontext/kai-engine/finding"
)

// rcRiskMediumDirect mirrors the server's riskMediumDirect: more files than
// this depending on a changed file directly makes the change M.
const rcRiskMediumDirect = 30

// rcRiskMediumReaches mirrors riskMediumReachesFallback, used when the blast
// radius has no node list.
const rcRiskMediumReaches = 100

var rcRiskLockfiles = map[string]bool{
	"go.sum": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"cargo.lock": true, "poetry.lock": true, "gemfile.lock": true, "composer.lock": true, "uv.lock": true,
}

var rcRiskManifests = map[string]bool{
	"go.mod": true, "package.json": true, "cargo.toml": true, "pyproject.toml": true, "gemfile": true,
	"pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "composer.json": true, "setup.py": true,
}

// rcRiskAreas are the server's sensitiveAreas: a changed file whose path names
// one makes the change L. Matched against whole path words.
//
// 2026-11-06: "session" and "sessions" are no longer auth words. In the first
// 81 production reviews they matched UI packages and a session-titles backfill
// script, never authentication.
var rcRiskAreas = map[string]string{
	"auth": "auth", "authn": "auth", "authz": "auth", "oauth": "auth", "login": "auth",
	"permission": "auth", "permissions": "auth", "rbac": "auth", "acl": "auth", "sso": "auth",
	"billing": "billing", "payment": "billing", "payments": "billing", "stripe": "billing", "invoice": "billing",
	"invoices": "billing", "pricing": "billing", "credits": "billing", "subscription": "billing", "subscriptions": "billing",
	"migration": "migrations", "migrations": "migrations", "schema": "migrations",
}

// rcRiskDeployWords name deploy configuration: a changed file whose path names
// one makes the change M, like a CI change. They made a change L until
// 2026-11-06; in the first 81 production reviews Helm charts alone put 13 of
// 27 L reviews there, routine chart edits that run50's review did not need.
var rcRiskDeployWords = map[string]bool{
	"terraform": true, "k8s": true, "kubernetes": true, "helm": true, "deploy": true,
}

// rcRisk is a pre-review risk level, S, M or L, with what set it. Level is ""
// when the change has no file list to read.
type rcRisk struct {
	Level   string
	Reasons []string
}

func (r rcRisk) String() string {
	if len(r.Reasons) == 0 {
		return r.Level
	}
	return r.Level + " (" + strings.Join(r.Reasons, ", ") + ")"
}

// rcRiskTestOrDoc mirrors the server's isTestOrDoc: a file that cannot change
// what ships.
func rcRiskTestOrDoc(p string) bool {
	l := strings.ToLower(p)
	base := path.Base(l)
	switch {
	case strings.HasSuffix(base, "_test.go"), strings.Contains(base, ".test."), strings.Contains(base, ".spec."),
		strings.HasSuffix(base, "_test.py"), strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, "_spec.rb"), strings.HasSuffix(base, "_test.rb"),
		strings.HasSuffix(base, "_test.exs"), rcRiskCamelTest(path.Base(p)):
		return true
	case strings.HasSuffix(base, ".md"), strings.HasSuffix(base, ".rst"), strings.HasSuffix(base, ".mdx"):
		return true
	}
	for _, seg := range strings.Split(path.Dir(l), "/") {
		switch {
		case seg == "docs" || seg == "doc" || seg == "documentation" || strings.HasPrefix(seg, "docs-") || strings.HasPrefix(seg, "docs_"):
			return true
		case seg == "test" || seg == "tests" || seg == "__tests__" || seg == "testdata":
			return true
		case (seg == "spec" || seg == "specs") && strings.HasSuffix(base, ".rb"):
			return true
		}
	}
	return false
}

// rcRiskCamelTest reports FooTest.java, InvoiceTests.cs, LedgerSpec.scala.
func rcRiskCamelTest(base string) bool {
	ext := path.Ext(base)
	switch strings.ToLower(ext) {
	case ".java", ".kt", ".cs", ".swift", ".php", ".scala":
	default:
		return false
	}
	name := strings.TrimSuffix(base, ext)
	return strings.HasSuffix(name, "Test") || strings.HasSuffix(name, "Tests") || strings.HasSuffix(name, "Spec")
}

// rcPreReviewRisk rates a change from its file list and blast radius alone.
func rcPreReviewRisk(files []finding.DiffFile, blast finding.Blast) rcRisk {
	if len(files) == 0 {
		return rcRisk{}
	}
	allTests := true
	for _, f := range files {
		if !rcRiskTestOrDoc(f.Path) {
			allTests = false
		}
	}
	if allTests {
		return rcRisk{Level: "S", Reasons: []string{"tests and docs only"}}
	}
	var high, medium []string
	seen := map[string]bool{}
	note := func(list *[]string, s string) {
		if !seen[s] {
			seen[s] = true
			*list = append(*list, s)
		}
	}
	for _, f := range files {
		if rcRiskTestOrDoc(f.Path) {
			continue
		}
		l := strings.ToLower(f.Path)
		base := path.Base(l)
		if strings.HasSuffix(base, ".sql") {
			note(&high, "touches migrations")
		}
		for _, w := range strings.FieldsFunc(l, func(r rune) bool { return r == '/' || r == '_' || r == '-' || r == '.' }) {
			if area, ok := rcRiskAreas[w]; ok {
				note(&high, "touches "+area)
			}
			if rcRiskDeployWords[w] {
				note(&medium, "changes deploy config")
			}
		}
		if rcRiskManifests[base] || rcRiskLockfiles[base] || (strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")) {
			note(&medium, "changes dependencies")
		}
		if strings.HasPrefix(l, ".github/workflows/") || base == ".gitlab-ci.yml" || base == "dockerfile" {
			note(&medium, "changes CI or the build")
		}
	}
	if len(blast.Nodes) > 0 {
		direct := 0
		for _, n := range blast.Nodes {
			if n.Hops == 1 {
				direct++
			}
		}
		if direct > rcRiskMediumDirect {
			note(&medium, fmt.Sprintf("%d files depend on it directly", direct))
		}
	} else if blast.Reaches > rcRiskMediumReaches {
		note(&medium, fmt.Sprintf("reached from %d places", blast.Reaches))
	}
	switch {
	case len(high) > 0:
		return rcRisk{Level: "L", Reasons: append(high, medium...)}
	case len(medium) > 0:
		return rcRisk{Level: "M", Reasons: medium}
	}
	return rcRisk{Level: "S"}
}
