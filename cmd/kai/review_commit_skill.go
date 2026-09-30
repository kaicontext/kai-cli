package main

// The review skill: every prompt a review sends is composed from the markdown
// files in reviewskill/, compiled into the binary.
//
//	procedure.md  how a grounded review proceeds, step by step
//	defect.md     what counts as a defect, what does not, one cause per issue,
//	              decisions: one definition shared by the reviewer, the fast
//	              pass and the fact-check, so they cannot drift apart
//	catalog.md    the categories of defect to check, each with an example of a
//	              finding and of something that is not one
//	report.md     how the grounded review writes its prose and its coda
//	fast.md       the fast pass's role and its own coda
//	sweep.md      the line-by-line sweep's role and output
//	challenge.md  the fact-check's role and method
//	packs/        checks for one language or one risk area, added only when the
//	              change touches it (rcPacksFor)
//
// A stage file splits at "<!-- shared sections -->": its role goes before the
// shared sections, its output contract after. Before this, each stage carried
// its own copy of the definitions in a Go string, and the copies had drifted.
//
// Examples in these files come from code outside any benchmark. A rule that
// quotes a benchmark case teaches the reviewer the test it is scored on
// (TestPromptsCarryNoBenchmarkCases).

import (
	"embed"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed reviewskill/*.md reviewskill/packs/*.md
var rcSkillFS embed.FS

const rcSharedMarker = "<!-- shared sections -->"

// rcSkillFile returns one skill file. They are embedded, so a missing one is a
// build mistake, and TestReviewSkillFilesCompose catches it before release.
func rcSkillFile(name string) string {
	b, err := rcSkillFS.ReadFile("reviewskill/" + name)
	if err != nil {
		panic("review skill: " + err.Error())
	}
	return strings.TrimSpace(string(b))
}

// rcStageFile splits a stage file into the part before the shared sections
// and the part after.
func rcStageFile(name string) (head, tail string) {
	h, t, ok := strings.Cut(rcSkillFile(name), rcSharedMarker)
	if !ok {
		return strings.TrimSpace(h), ""
	}
	return strings.TrimSpace(h), strings.TrimSpace(t)
}

func rcJoinSections(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// The stage prompts without packs. The grounded review and the sweep add the
// packs for their change (rcReviewSystemFor, rcSweepSystemFor).
var (
	rcReviewSystem        = rcReviewSystemFor(nil)
	rcFastReviewSystem    = rcComposeStage("fast.md", rcSkillFile("defect.md"), rcSkillFile("catalog.md"))
	rcSweepSystem         = rcSweepSystemFor(nil)
	rcChallengeSystemHead = rcComposeStage("challenge.md", rcSkillFile("defect.md"))
)

func rcComposeStage(stage string, shared ...string) string {
	head, tail := rcStageFile(stage)
	return rcJoinSections(append(append([]string{head}, shared...), tail)...)
}

// rcReviewSystemFor is the grounded reviewer's system prompt with the packs
// for its change between the catalog and the report format.
func rcReviewSystemFor(packs []string) string {
	return rcJoinSections(rcSkillFile("procedure.md"), rcSkillFile("defect.md"), rcSkillFile("catalog.md"),
		rcPackText(packs), rcSkillFile("report.md"))
}

// rcSweepSystemFor is the sweep's system prompt. It takes the catalog and the
// packs but not the definition of a defect: the sweep proposes candidates, and
// the fact-check that follows applies the definition to each of them.
func rcSweepSystemFor(packs []string) string {
	return rcComposeStage("sweep.md", rcSkillFile("catalog.md"), rcPackText(packs))
}

func rcPackText(packs []string) string {
	if len(packs) == 0 {
		return ""
	}
	parts := []string{"# Checks for what this change touches\n\nThese extend the catalog for the languages and risk areas this change touches."}
	for _, p := range packs {
		parts = append(parts, rcSkillFile("packs/"+p+".md"))
	}
	return rcJoinSections(parts...)
}

// Language packs, by the extensions of the files a change touches.
var rcLangPackExts = map[string][]string{
	"go":         {".go"},
	"java":       {".java", ".kt", ".kts"},
	"python":     {".py"},
	"typescript": {".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue", ".svelte"},
	"ruby":       {".rb", ".erb", ".rake"},
}

// Risk packs, by what the change's paths and added lines mention. Words are
// bounded by non-letters rather than \b, so has_permission, isAuthenticated
// and refund_amount count: \b never fires between an underscore and a letter.
var rcRiskPacks = []struct {
	name string
	re   *regexp.Regexp
}{
	{"auth", rcWords(`auth[a-z]*|authori[sz][a-z]*|permissions?|roles?|scopes?|sessions?|login|logout|passwords?|acl|polic(y|ies)|admin[a-z]*|oauth|jwt|csrf`)},
	{"money", rcWords(`prices?|amounts?|balances?|credits?|charge[sd]?|invoices?|billing|payments?|refund(s|ed)?|currenc(y|ies)|cents|stripe|payouts?|subscriptions?`)},
	{"data", regexp.MustCompile(`(?i)(migrat[a-z]*|alter table|create (table|index)|add_column|schema\.prisma|\.sql\b|migrations?/)`)},
	{"external", regexp.MustCompile(`(?i)(https?://|\bfetch\(|axios|requests\.(get|post|put|delete)\b|\bhttp\.(get|post|newrequest|client)\b|webhooks?|httpclient|resttemplate|faraday|net::http|(^|[^a-z])retr(y|ies)([^a-z]|$))`)},
}

// rcWords matches any of the alternatives as a whole word, where a word is
// delimited by anything that is not a letter.
func rcWords(alternatives string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^a-z])(` + alternatives + `)([^a-z]|$)`)
}

const (
	rcMaxLangPacks = 2
	rcMaxRiskPacks = 3
)

// rcPacksFor picks the packs for a unified diff: up to two language packs, for
// the languages with the most changed lines, and up to three risk packs, for
// the risk areas its paths and added lines mention most. Returned in a stable
// order, so the prompt for a change is the same on every run and caches.
func rcPacksFor(diff string) []string {
	langLines := map[string]int{}
	riskHits := map[string]int{}
	lang := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			lang = rcLangOf(p)
			for _, r := range rcRiskPacks {
				if r.re.MatchString(p) {
					riskHits[r.name]++
				}
			}
		case strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-"):
			if strings.HasPrefix(line, "---") {
				continue
			}
			if lang != "" {
				langLines[lang]++
			}
			if strings.HasPrefix(line, "+") {
				for _, r := range rcRiskPacks {
					if r.re.MatchString(line) {
						riskHits[r.name]++
					}
				}
			}
		}
	}
	top := func(counts map[string]int, max int) []string {
		var names []string
		for n, c := range counts {
			if c > 0 {
				names = append(names, n)
			}
		}
		sort.Slice(names, func(i, j int) bool {
			if counts[names[i]] != counts[names[j]] {
				return counts[names[i]] > counts[names[j]]
			}
			return names[i] < names[j]
		})
		if len(names) > max {
			names = names[:max]
		}
		sort.Strings(names)
		return names
	}
	return append(top(langLines, rcMaxLangPacks), top(riskHits, rcMaxRiskPacks)...)
}

func rcLangOf(p string) string {
	ext := strings.ToLower(path.Ext(p))
	for lang, exts := range rcLangPackExts {
		for _, e := range exts {
			if ext == e {
				return lang
			}
		}
	}
	return ""
}
