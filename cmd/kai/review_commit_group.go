package main

// review_commit_group.go — one root cause, one finding, by asking.
//
// A review often states one defect twice: the reviewer and the sweep each
// propose it, reworded, on neighbouring lines, and the challenge confirms
// both. Over 14 days of reviews 164 of 210 pairs of confirmed findings in one
// file within three lines were repeats (2026-10-13), and since kai-server
// scores out of 10, each repeat costs the author points: kai-api#4 read
// "fix 6 findings, −10" for three bugs. A rule asking the challenge to refute
// repeats sits in a long prompt and GLM-5.2 did not follow it; a text rule
// (kai-server's backstop) catches about two thirds and cannot see a repeat
// that shares no code names ("`billingTransport` mints a new charge id" /
// "`RoundTrip` assigns a new charge id").
//
// So after the challenge, one short call does only this: here are the
// confirmed findings, numbered; which ones repeat an earlier one? The answer
// folds each repeat into the finding it repeats, as an "(also: path:line)".
// Any failure — the call, the reply, a bad number — leaves the review as it
// was, and says so in the log.
//
// The fact-check model reasons before it answers, with no off switch (GLM):
// on kai-server#467 a 400-token cap left no room for the answer, and two
// repeats went out unfolded with nothing in the log. So the call caps the
// reasoning, leaves room past it, and logs what came back every time.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

const rcGroupSystem = `You are given the confirmed findings of a code review, numbered. Some may be the same defect said twice: the same root cause — the same wrong expression, the same missing check, the same mistaken assumption — reworded, or cited on a neighbouring line. Two different problems are NOT the same, even on the same line or in the same function: a SQL injection through one variable and another through a different variable are two findings.

Reply with one line per repeat, "N = M", meaning finding N repeats the earlier finding M (M < N). Reply NONE when no finding repeats another. Nothing else.`

// rcGroupMaxIssues bounds the call; a review with more is left as it is.
const rcGroupMaxIssues = 40

// rcGroupMaxTokens leaves room for the model's reasoning as well as the
// answer, which is a few short lines.
const rcGroupMaxTokens = 4096

// rcGroupLineRe is one "N = M" line, also as a list item ("- 5 = 2") or with
// trailing punctuation ("5 = 2.").
var rcGroupLineRe = regexp.MustCompile(`^\s*(?:[-*]\s*)?#?(\d+)\s*=\s*#?(\d+)\s*[.,;]?\s*$`)

// rcGroupDuplicateIssues asks the model which confirmed ISSUES repeat an
// earlier one and folds them. It returns the review and how many it folded.
// refs are the tags taken off the draft (rcTakeEarlierTags); a folded
// repeat's tags move to the finding it is folded into.
func rcGroupDuplicateIssues(ctx context.Context, prov provider.Provider, model, raw string, refs map[string]string) (string, int) {
	return rcGroupDuplicateIssuesWith(raw, refs, func(user string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		resp, err := prov.Send(cctx, provider.Request{
			Model:           model,
			System:          rcGroupSystem,
			MaxTokens:       rcGroupMaxTokens,
			ReasoningEffort: "low",
			Messages:        []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: user}}}},
		})
		if err != nil {
			return "", err
		}
		var text strings.Builder
		for _, part := range resp.Parts {
			if t, ok := part.(message.TextContent); ok {
				text.WriteString(t.Text)
			}
		}
		if strings.TrimSpace(text.String()) == "" {
			return "", fmt.Errorf("no answer (finish %s, %d output tokens)", resp.FinishReason, resp.OutputTokens)
		}
		return text.String(), nil
	})
}

// rcGroupDuplicateIssuesWith is rcGroupDuplicateIssues with the call as a
// seam, for tests.
func rcGroupDuplicateIssuesWith(raw string, refs map[string]string, ask func(user string) (string, error)) (string, int) {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return raw, 0
	}
	head, coda := raw[:i+len(rcReviewDataMarker)], raw[i+len(rcReviewDataMarker):]
	lines := strings.Split(coda, "\n")

	// The plain, located ISSUES bullets, numbered from 1.
	type bullet struct {
		line int
		item string
		loc  string
	}
	var bullets []bullet
	section := ""
	for j, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "- ") {
			if key, _, labelled := rcMachineLine(t); labelled {
				section = key
			}
			continue
		}
		if section != "issues" && section != "findings" {
			continue
		}
		item := strings.TrimPrefix(t, "- ")
		if _, nit := rcSplitNit(item); nit {
			continue
		}
		path, ln, ok := rcIssueLocation(item)
		if !ok {
			continue
		}
		bullets = append(bullets, bullet{line: j, item: item, loc: fmt.Sprintf("%s:%d", path, ln)})
	}
	if len(bullets) < 2 {
		return raw, 0
	}
	if len(bullets) > rcGroupMaxIssues {
		fmt.Fprintf(os.Stderr, "  grouping: %d findings, more than %d; left as they are\n", len(bullets), rcGroupMaxIssues)
		return raw, 0
	}

	var user strings.Builder
	for n, b := range bullets {
		fmt.Fprintf(&user, "%d. %s\n", n+1, b.item)
	}
	reply, err := ask(user.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, "  grouping: the call failed (%v); findings left as they are\n", err)
		return raw, 0
	}

	// N = M, N later than M, both real, M not itself a repeat.
	into := map[int]int{}
	for _, l := range strings.Split(reply, "\n") {
		m := rcGroupLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		t, _ := strconv.Atoi(m[2])
		if t < 1 || n <= t || n > len(bullets) {
			continue
		}
		into[n] = t
	}
	for n, t := range into {
		// A repeat of a repeat folds into the first. Every step points to an
		// earlier finding, so this ends; the bound is there regardless.
		for k := 0; into[t] != 0 && k < len(bullets); k++ {
			t = into[t]
		}
		into[n] = t
	}
	if len(into) == 0 {
		fmt.Fprintf(os.Stderr, "  grouping: %d findings, no repeats (answer %q)\n", len(bullets), rcClip(strings.TrimSpace(reply), 120))
		return raw, 0
	}
	// In order, so the also-lists and the log read the same on every run.
	repeats := make([]int, 0, len(into))
	for n := range into {
		repeats = append(repeats, n)
	}
	sort.Ints(repeats)
	also := map[int][]string{}
	var targets []int
	drop := map[int]bool{}
	for _, n := range repeats {
		t := into[n]
		if _, seen := also[t]; !seen {
			also[t] = nil
			targets = append(targets, t)
		}
		// The repeat brings its own location and any it already listed; a
		// location the kept finding names (its own line, or its also-list)
		// is not added again.
		for _, loc := range append([]string{bullets[n-1].loc}, rcAlsoLocs(bullets[n-1].item)...) {
			if loc != bullets[t-1].loc && !slices.Contains(rcAlsoLocs(bullets[t-1].item), loc) && !slices.Contains(also[t], loc) {
				also[t] = append(also[t], loc)
			}
		}
		drop[bullets[n-1].line] = true
		rcFoldTags(refs, bullets[t-1].item, bullets[n-1].item)
		fmt.Fprintf(os.Stderr, "  grouping: %s repeats %s — folded\n", bullets[n-1].loc, bullets[t-1].loc)
	}
	renamed := map[string]string{}
	for _, t := range targets {
		item := rcWithAlso(bullets[t-1].item, also[t])
		renamed[bullets[t-1].item] = item
		lines[bullets[t-1].line] = "- " + item
	}
	dropped := map[string]bool{}
	for _, n := range repeats {
		dropped[bullets[n-1].item] = true
	}
	kept := lines[:0:0]
	for j, l := range lines {
		if !drop[j] {
			kept = append(kept, l)
		}
	}
	return rcFoldProse(head, dropped, renamed) + strings.Join(kept, "\n"), len(into)
}

// rcFoldProse does to the read-through what the fold did to ISSUES: each
// confirmed finding has a "### <its ISSUES text>" section under "## Findings"
// (rcAssembleReview), and kai-server shows that prose as the full read-
// through. A repeat's section goes; the finding it repeats gets its
// also-list in its heading. Sections are matched on their exact text; one
// that matches nothing is left alone.
func rcFoldProse(head string, dropped map[string]bool, renamed map[string]string) string {
	lines := strings.Split(head, "\n")
	out := lines[:0:0]
	skipping := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "### ") || t == rcReviewDataMarker {
			skipping = false
		}
		if title, ok := strings.CutPrefix(t, "### "); ok {
			title = strings.TrimSpace(title)
			if dropped[title] && renamed[title] == "" {
				skipping = true
				continue
			}
			if r := renamed[title]; r != "" {
				l = "### " + r
			}
		}
		if !skipping {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// rcAlsoLocs is the locations a bullet's "(also: …)" list names.
func rcAlsoLocs(item string) []string {
	var out []string
	for _, m := range rcAlsoListRe.FindAllString(item, -1) {
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(m), "(also:"), ")")
		for _, loc := range strings.Split(inner, ",") {
			if loc = strings.TrimSpace(loc); loc != "" {
				out = append(out, loc)
			}
		}
	}
	return out
}

// rcFoldTags gives the finding kept the tags of a repeat folded into it: the
// repeat's earlier-ref when the kept one has none, so kai-server still sees a
// point already on the pull request, and the higher of the two severities.
func rcFoldTags(refs map[string]string, keptItem, dropItem string) {
	if refs == nil {
		return
	}
	keptKey, _, ok1 := rcEarlierBulletKey("issues", "- "+keptItem)
	dropKey, _, ok2 := rcEarlierBulletKey("issues", "- "+dropItem)
	if !ok1 || !ok2 || refs[dropKey] == "" {
		return
	}
	earlier, sev := rcTagParts(refs[keptKey])
	dEarlier, dSev := rcTagParts(refs[dropKey])
	if earlier == "" {
		earlier = dEarlier
	}
	rank := map[string]int{"(severity: low)": 1, "(severity: medium)": 2, "(severity: high)": 3}
	if rank[dSev] > rank[sev] {
		sev = dSev
	}
	var tags []string
	for _, t := range []string{sev, earlier} {
		if t != "" {
			tags = append(tags, t)
		}
	}
	refs[keptKey] = strings.Join(tags, " ")
}

// rcTagParts splits a remembered tag string into its earlier-ref and its
// severity, each normalised ("(earlier: …)", "(severity: high)").
func rcTagParts(tags string) (earlier, severity string) {
	for _, t := range rcEarlierTagRe.FindAllString(tags, -1) {
		t = strings.ToLower(strings.TrimSpace(t))
		t = "(" + strings.Join(strings.Fields(strings.Replace(strings.Trim(t, "()"), ":", ": ", 1)), " ") + ")"
		if strings.HasPrefix(t, "(severity") {
			severity = t
		} else {
			earlier = t
		}
	}
	return earlier, severity
}
