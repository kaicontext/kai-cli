package main

// Repository review rules: a file the maintainers of a repository commit to
// say what Kai's review should pay attention to there — the conventions a
// change must keep, the paths where a mistake costs the most, and (once the
// review can publish them) the kinds of finding beyond defects they want.
//
// The file is read from the BASE branch, as the review profile is
// (review_commit_profile.go): the pull request under review cannot loosen or
// rewrite the rules it is reviewed against, and a pull request that edits the
// file is reviewed against the version it is changing.
//
// What a rule can do in this version:
//   - instructions: maintainers' own words, handed to every finder, the sweep
//     and the fact-check, which sees them as a citable SOURCE, so a finding
//     that rests on a stated repository requirement can be supported rather
//     than refuted as generic advice;
//   - sensitive_paths: globs; the files a change touches under them are named
//     to the finders as needing the most scrutiny;
//   - detail: how much of the review kai-server posts on the pull request —
//     score, short or full. Carried in the finding bundle; kai-server prefers
//     it to the repository's Atlas setting, since this file is versioned with
//     the code;
//   - rules: rule packs from rcRulePacks. They are checked and recorded now
//     but NOT handed to the finders yet: the fact-check and the selection step
//     publish defects only, so a design or test-quality finding would be
//     drafted and then dropped, spending review time for nothing. They take
//     effect with the rule-finding lane that publishes them separately.
//
// A broken file never fails or silently changes the review: it is reported,
// and the review runs as it would without one.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// rcRulesPath is where a repository keeps its review rules: the repository
// root, beside the other tools' dotfiles, and outside .kai/ (Kai's own data
// directory, which repositories ignore).
const rcRulesPath = ".kai-review.yaml"

const (
	rcRulesMaxInstructionBytes = 4 * 1024
	rcRulesMaxSensitivePaths   = 50
	// rcRulesMaxSensitiveListed caps the files a prompt names as sensitive; a
	// change touching more than this is named by count past it.
	rcRulesMaxSensitiveListed = 30
)

// rcRulePacks are the rule packs a repository can turn on, each a kind of
// finding beyond defects. The description is what a maintainer reads in the
// error for an unknown pack and in the review log.
var rcRulePacks = map[string]string{
	"design":        "changes that make code harder to understand or change: functions with several purposes, abstractions that no longer fit, coupling and tangled dependencies",
	"api":           "interfaces that expose implementation details, are larger or more specialized than their callers need, or behave differently from what they suggest",
	"tests":         "tests that do not exercise the behaviour they claim to, miss the changed behaviour, or would pass without the change",
	"observability": "important runtime paths and failures that cannot be told apart in logs, metrics or traces, or that lack the context to diagnose them",
	"docs":          "missing or misleading documentation that a caller or maintainer needs to use or change the code",
}

func rcRulePackNames() []string {
	names := make([]string, 0, len(rcRulePacks))
	for n := range rcRulePacks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// rcReviewDetails are the levels of review kai-server can post (its
// repo_review_settings.review_detail): a score line, a short review of the
// top high-severity points, or the full review.
var rcReviewDetails = []string{"score", "short", "full"}

// rcRepoRules is a parsed rules file.
type rcRepoRules struct {
	Source         string
	Packs          []string
	SensitivePaths []string
	Instructions   string
	// Detail is the review detail level, "" when the file does not set one.
	Detail string
}

// rcActiveDetail is the active rules' detail level, "" for none.
func rcActiveDetail() string {
	if rcActiveRules == nil {
		return ""
	}
	return rcActiveRules.Detail
}

// rcActiveRules are the rules this review runs under, nil for none. Set once,
// before the review starts, by rcLoadRulesFor.
var rcActiveRules *rcRepoRules

// rcParseRules parses and checks a rules file. Any unknown key, unknown pack
// or bad glob is an error naming it: a typo that quietly did nothing would
// leave maintainers believing a rule is applied that is not.
func rcParseRules(data []byte, source string) (*rcRepoRules, error) {
	var f struct {
		Rules          []string `yaml:"rules"`
		SensitivePaths []string `yaml:"sensitive_paths"`
		Instructions   string   `yaml:"instructions"`
		Detail         string   `yaml:"detail"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("review rules %s: %w", source, err)
	}
	r := &rcRepoRules{Source: source, Instructions: strings.TrimSpace(f.Instructions)}
	if d := strings.ToLower(strings.TrimSpace(f.Detail)); d != "" {
		ok := false
		for _, v := range rcReviewDetails {
			ok = ok || d == v
		}
		if !ok {
			return nil, fmt.Errorf("review rules %s: detail %q must be one of %s", source, f.Detail, strings.Join(rcReviewDetails, ", "))
		}
		r.Detail = d
	}
	if len(r.Instructions) > rcRulesMaxInstructionBytes {
		return nil, fmt.Errorf("review rules %s: instructions are %d bytes, at most %d", source, len(r.Instructions), rcRulesMaxInstructionBytes)
	}
	seen := map[string]bool{}
	for _, p := range f.Rules {
		p = strings.ToLower(strings.TrimSpace(p))
		if _, ok := rcRulePacks[p]; !ok {
			return nil, fmt.Errorf("review rules %s: unknown rule pack %q (packs: %s)", source, p, strings.Join(rcRulePackNames(), ", "))
		}
		if !seen[p] {
			seen[p] = true
			r.Packs = append(r.Packs, p)
		}
	}
	if len(f.SensitivePaths) > rcRulesMaxSensitivePaths {
		return nil, fmt.Errorf("review rules %s: %d sensitive_paths, at most %d", source, len(f.SensitivePaths), rcRulesMaxSensitivePaths)
	}
	for _, g := range f.SensitivePaths {
		g = strings.TrimPrefix(strings.TrimSpace(g), "/")
		if g == "" || !doublestar.ValidatePattern(g) {
			return nil, fmt.Errorf("review rules %s: sensitive_paths: %q is not a valid glob", source, g)
		}
		r.SensitivePaths = append(r.SensitivePaths, g)
	}
	return r, nil
}

// rcLoadRulesFor finds the rules for a review of base...ref and makes them the
// active ones. KAI_REVIEW_RULES names a local file, for trying rules by hand;
// otherwise the file is read from the base branch. No file, no base, or a
// broken file is no rules; the returned warning says why a file present was
// not used.
func rcLoadRulesFor(base string) (warning string) {
	rcActiveRules = nil
	var data []byte
	var source string
	if path := strings.TrimSpace(os.Getenv("KAI_REVIEW_RULES")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("review rules: %v", err)
		}
		data, source = b, path
	} else {
		if base == "" {
			return ""
		}
		// ls-tree tells an absent file apart from a read that failed, without
		// fetching anything in the review job's blobless clone.
		listed, err := exec.Command("git", "ls-tree", "--name-only", base, "--", rcRulesPath).Output()
		if err != nil || strings.TrimSpace(string(listed)) == "" {
			return ""
		}
		b, err := exec.Command("git", "show", base+":"+rcRulesPath).Output()
		if err != nil {
			return fmt.Sprintf("review rules: reading %s from %s: %v", rcRulesPath, base, err)
		}
		data, source = b, base+":"+rcRulesPath
		if repo := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY_FULLNAME")); repo != "" {
			source = repo + "@" + source
		}
	}
	r, err := rcParseRules(data, source)
	if err != nil {
		return err.Error() + " — reviewing without them"
	}
	rcActiveRules = r
	return ""
}

// rcDescribeRules is the review log's line for the active rules, "" for none.
func rcDescribeRules() string {
	r := rcActiveRules
	if r == nil {
		return ""
	}
	parts := []string{}
	if r.Instructions != "" {
		parts = append(parts, fmt.Sprintf("instructions (%d bytes)", len(r.Instructions)))
	}
	if n := len(r.SensitivePaths); n > 0 {
		parts = append(parts, fmt.Sprintf("%d sensitive path glob(s)", n))
	}
	if len(r.Packs) > 0 {
		parts = append(parts, fmt.Sprintf("rule packs %s recorded, not yet applied", strings.Join(r.Packs, ", ")))
	}
	if r.Detail != "" {
		parts = append(parts, "detail "+r.Detail)
	}
	if len(parts) == 0 {
		parts = append(parts, "empty")
	}
	return fmt.Sprintf("  review rules: %s — %s", r.Source, strings.Join(parts, "; "))
}

// rcSensitiveOf returns the paths that fall under a sensitive glob, in order.
func rcSensitiveOf(paths []string) []string {
	if rcActiveRules == nil || len(rcActiveRules.SensitivePaths) == 0 {
		return nil
	}
	var out []string
	for _, p := range paths {
		for _, g := range rcActiveRules.SensitivePaths {
			if ok, _ := doublestar.Match(g, p); ok {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// rcRulesBlock is the prompt section the finders and the sweep get for the
// files in front of them, "" when the rules say nothing about them. It states
// what the rules are for and what they are not: they direct attention, they
// do not change what counts as a defect or how one is reported.
func rcRulesBlock(paths []string) string {
	r := rcActiveRules
	if r == nil {
		return ""
	}
	sensitive := rcSensitiveOf(paths)
	if r.Instructions == "" && len(sensitive) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("REPOSITORY REVIEW RULES (from " + rcRulesPath + " on the base branch, written by this repository's maintainers; the change under review cannot edit them). ")
	b.WriteString("They say what this repository requires and where mistakes cost most. They do not change what counts as a defect or how to report one: ")
	b.WriteString("a change that breaks a stated requirement is a defect, and the finding names the requirement it breaks.\n")
	if r.Instructions != "" {
		b.WriteString("MAINTAINERS' INSTRUCTIONS:\n")
		b.WriteString(r.Instructions)
		b.WriteString("\n")
	}
	if len(sensitive) > 0 {
		b.WriteString("SENSITIVE FILES IN THIS CHANGE (the maintainers marked these paths as needing the most scrutiny — authorization, data integrity, money, secrets; read every changed line in them):\n")
		for i, p := range sensitive {
			if i == rcRulesMaxSensitiveListed {
				fmt.Fprintf(&b, "  ... and %d more\n", len(sensitive)-i)
				break
			}
			b.WriteString("  " + p + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// rcRulesChallengeSources hands the maintainers' instructions to the
// fact-check as a SOURCE tied to every changed file, so every batch receives
// it and an allegation resting on a stated requirement can cite it. Nil when
// there are no instructions.
func rcRulesChallengeSources(changed []string) []rcSource {
	if rcActiveRules == nil || rcActiveRules.Instructions == "" {
		return nil
	}
	return []rcSource{{
		Text:     "// " + rcRulesPath + " on the base branch — this repository's maintainers' review instructions\n" + rcActiveRules.Instructions,
		Coord:    rcCoordRows,
		ForPaths: append([]string(nil), changed...),
	}}
}
