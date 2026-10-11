package main

// review_commit_prior.go — re-reviewing, not reviewing again.
//
// Every push used to be reviewed from nothing. On FalkorDB's first week the
// verdict changed 30 times between consecutive reviews of the same pull
// request — #2278 went 3, 4, 4, 3, 4, 4, 3, 4 over eight small pushes — and in
// 26 of them what the review listed had changed too: not the code, the
// reviewer's recall. A person re-reviewing starts from what they said last
// time and checks it against the new commit.
//
// kai-server sends Kai's latest grounded review of the PR as KAI_PRIOR_REVIEW
// (api/review_prior.go). This puts it in front of the reviewer, with the files
// that changed since that commit when the clone has it, and the rule
// (rcReviewSystem, "YOU HAVE REVIEWED THIS BEFORE"): keep what still holds,
// drop what was fixed, and move the score only for a reason you can name.
//
// It also carries what people answered: a line comment of Kai's that someone
// replied to ("intentional", "not an issue"). On kai-server#434 the author
// answered "intentional" to the only open defect; the next review listed no
// findings and still scored 3/5, its prose asking for the guard the author
// had just declined — it knew what it said, not what was said back.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// rcPrior is Kai's previous review of this pull request.
type rcPrior struct {
	Head      string `json:"head"`
	Readiness int    `json:"readiness"`
	Summary   string `json:"summary"`
	Points    []rcPriorPoint `json:"points"`
	Answered []struct {
		Point string `json:"point"`
		Reply string `json:"reply"`
	} `json:"answered"`
}

// rcPriorPoint is one earlier point; Ref is set on defects and is what the
// reviewer tags the point with when it still holds (review_commit_earlier.go).
type rcPriorPoint struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Ref  string `json:"ref"`
}

// rcPriorFromEnv reads KAI_PRIOR_REVIEW; nil when it is unset or unreadable.
func rcPriorFromEnv() *rcPrior {
	raw := strings.TrimSpace(os.Getenv("KAI_PRIOR_REVIEW"))
	if raw == "" {
		return nil
	}
	var p rcPrior
	if json.Unmarshal([]byte(raw), &p) != nil || strings.TrimSpace(p.Head) == "" {
		return nil
	}
	return &p
}

// rcChangedSince lists the files that differ between the previous review's
// commit and HEAD, or nil when the clone does not have that commit (a
// force-push, a shallow clone).
func rcChangedSince(head string) ([]string, bool) {
	if exec.Command("git", "rev-parse", "--verify", "--quiet", head+"^{commit}").Run() != nil {
		return nil, false
	}
	out, err := exec.Command("git", "--no-pager", "diff", "--name-only", head, "HEAD").Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, true
}

// rcPriorBlock renders the previous review for the reviewer's prompt.
// changed is what differs since its commit; known is false when that could not
// be worked out.
func rcPriorBlock(p *rcPrior, changed []string, known bool) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "YOUR PREVIOUS REVIEW OF THIS PULL REQUEST (of commit %s", rcShort(p.Head))
	if p.Readiness > 0 {
		fmt.Fprintf(&b, ", MERGE_READY %d", p.Readiness)
	}
	b.WriteString(") — re-check it; see YOU HAVE REVIEWED THIS BEFORE:\n")
	if s := strings.TrimSpace(p.Summary); s != "" {
		b.WriteString("Summary then: " + s + "\n")
	}
	for _, pt := range p.Points {
		if pt.Ref != "" {
			fmt.Fprintf(&b, "- [%s, ref %s] %s\n", pt.Kind, pt.Ref, strings.TrimSpace(pt.Text))
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s\n", pt.Kind, strings.TrimSpace(pt.Text))
	}
	if len(p.Answered) > 0 {
		b.WriteString("Answered on the pull request since — see ANSWERED POINTS:\n")
		for _, a := range p.Answered {
			fmt.Fprintf(&b, "- you said: %s\n  they replied: %s\n", strings.TrimSpace(a.Point), strings.TrimSpace(a.Reply))
		}
	}
	switch {
	case !known:
		b.WriteString("Files changed since then: unknown (that commit is not in this clone).\n")
	case len(changed) == 0:
		b.WriteString("Files changed since then: none — the code is the same as when you last reviewed it.\n")
	default:
		const show = 40
		list := changed
		more := ""
		if len(list) > show {
			list, more = list[:show], fmt.Sprintf(" … and %d more", len(changed)-show)
		}
		fmt.Fprintf(&b, "Files changed since then (%d): %s%s\n", len(changed), strings.Join(list, ", "), more)
	}
	b.WriteString("\n")
	return b.String()
}
