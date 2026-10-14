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
// was.

import (
	"context"
	"fmt"
	"os"
	"regexp"
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

var rcGroupLineRe = regexp.MustCompile(`^\s*(\d+)\s*=\s*(\d+)\s*$`)

// rcGroupDuplicateIssues asks the model which confirmed ISSUES repeat an
// earlier one and folds them. It returns the review and how many it folded.
func rcGroupDuplicateIssues(ctx context.Context, prov provider.Provider, model, raw string) (string, int) {
	return rcGroupDuplicateIssuesWith(raw, func(user string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		resp, err := prov.Send(cctx, provider.Request{
			Model:     model,
			System:    rcGroupSystem,
			MaxTokens: 400,
			Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: user}}}},
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
		return text.String(), nil
	})
}

// rcGroupDuplicateIssuesWith is rcGroupDuplicateIssues with the call as a
// seam, for tests.
func rcGroupDuplicateIssuesWith(raw string, ask func(user string) (string, error)) (string, int) {
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
	if len(bullets) < 2 || len(bullets) > rcGroupMaxIssues {
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
		return raw, 0
	}
	also := map[int][]string{}
	drop := map[int]bool{}
	for n, t := range into {
		also[t] = append(also[t], bullets[n-1].loc)
		drop[bullets[n-1].line] = true
		fmt.Fprintf(os.Stderr, "  grouping: %s repeats %s — folded\n", bullets[n-1].loc, bullets[t-1].loc)
	}
	for t, locs := range also {
		lines[bullets[t-1].line] = "- " + rcWithAlso(bullets[t-1].item, locs)
	}
	kept := lines[:0:0]
	for j, l := range lines {
		if !drop[j] {
			kept = append(kept, l)
		}
	}
	return head + strings.Join(kept, "\n"), len(into)
}
