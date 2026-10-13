package main

// review_commit_stakes.go — a decision names what is at stake.
//
// DECISIONS exists for the change that is correct and still needs a human's
// yes because it reaches real people: it charges them, limits them, sends to
// them, deletes their data, changes their access, or changes a contract they
// build on (kai-server#126: a 5% surcharge green-checked as "consistent").
// The model stretched it to anything a tool does — on kai-cli#171 every
// review asked for a yes on how the reviewer itself behaves — and a customer
// (2026-10-13) read Kai as "focused on making sure you are making the right
// decisions" rather than on whether the change can merge.
//
// The prompts now ask each decision to open with its stake: [money] [limit]
// [sends] [deletes] [access] [contract]. A decision with no tag is kept only
// if its own words name one of those stakes; otherwise it is dropped before
// the challenge, so it neither reaches the author nor holds the score at 4.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// rcStakeTagRe reads a decision's leading stake tag.
// Any case, and through the markdown a model wraps a label in (**[money]**,
// `[access]`).
var rcStakeTagRe = regexp.MustCompile(`(?i)^[\s*_`+"`"+`]*\[(money|limit|sends|deletes|access|contract)\]`)

// rcStakeWordsRe finds a stake in an untagged decision's own words.
var rcStakeWordsRe = regexp.MustCompile(`(?i)\b(charg(e|es|ed|ing)|billing|billed|pric(e|es|ing)|credits?|debit\w*|refund\w*|invoice\w*|payments?|subscriptions?|surcharge\w*|balance|quota\w*|rate[- ]limit\w*|spend(ing)? (cap|limit)|emails?|notifications?|webhooks?|sms|push notification\w*|delet(e|es|ed|ion)|purg(e|es|ed)|retention|erase\w*|permissions?|roles?|authenticat\w*|authoriz\w*|oauth|sso|api keys?|access tokens?|secrets?|public api|breaking change|schema|migrations?)\b`)

// rcHasStake reports whether a decision names what is at stake.
func rcHasStake(decision string) bool {
	return rcStakeTagRe.MatchString(decision) || rcStakeWordsRe.MatchString(decision)
}

// rcDropStakelessDecisions removes DECISIONS bullets that name no stake from
// a draft's coda, and says how many it removed.
func rcDropStakelessDecisions(raw string) (string, int) {
	i := strings.Index(raw, rcReviewDataMarker)
	if i < 0 {
		return raw, 0
	}
	head, coda := raw[:i+len(rcReviewDataMarker)], raw[i+len(rcReviewDataMarker):]
	lines := strings.Split(coda, "\n")
	out := lines[:0]
	section, dropped := "", 0
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		bullet := rcUnwrapMachineBullet(t)
		if !strings.HasPrefix(bullet, "-") {
			// Only the coda's own headers move between lists, exactly as
			// rcParseReviewOutput reads them: a stray "Note that X:" line is
			// not a header.
			if key, _, labelled := rcMachineLine(t); labelled {
				switch key {
				case "issues", "findings", "decisions", "intent_match", "merge_ready", "summary", "note":
					section = key
				}
			}
			out = append(out, ln)
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(bullet, "-"))
		if section == "decisions" && item != "" && !rcIsEmptyListItem(item) && !rcHasStake(item) {
			dropped++
			continue
		}
		out = append(out, ln)
	}
	return head + strings.Join(out, "\n"), dropped
}

// rcStakeDecisions is the same rule over parsed decisions, for a path that
// reaches the finding without the challenge.
func rcStakeDecisions(decisions []string) []string {
	var keep []string
	for _, d := range decisions {
		if rcHasStake(d) {
			keep = append(keep, d)
		}
	}
	if n := len(decisions) - len(keep); n > 0 {
		fmt.Fprintf(os.Stderr, "  decisions: %d dropped — no stake named (money, limit, sends, deletes, access, contract)\n", n)
	}
	return keep
}
