package main

// review_commit_bigpr.go — a large change is reviewed on its riskiest files.
//
// A review's cost grows with the files it reads: the turn budget, the
// coverage gate, both sweeps and the diff in every prompt are all sized to
// the change. For a review someone pays for that is the right trade. For one
// given away it means one very large pull request can cost as much as fifty
// ordinary ones.
//
// So a job may set a file limit: KAI_REVIEW_RISKIEST_FILES=N keeps the N
// riskiest reviewable files of a change that has more than
// KAI_REVIEW_RISKIEST_OVER of them (left out, more than N). The turn budget,
// the coverage gate, the sweeps and the diff in the prompt are sized to those
// files. The other files are named in the prompt, with their size, and not
// shown. The bundle says which files were kept and out of how many
// (run.file_cut), so whoever shows the review can say so.
//
// Neither setting present, nothing is cut. The fast pass is one call over the
// diff and is not cut either.

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/kaicontext/kai-engine/finding"
)

// rcFileCut is the cut a large change was reviewed on: the files kept, in
// rank order, out of Of reviewable files.
type rcFileCut struct {
	Kept []string `json:"kept"`
	Of   int      `json:"of"`
}

// rcRiskiestLimits reads the job's file limit: keep at most keep files of a
// change with more than over reviewable ones. keep is 0 when no limit is set.
func rcRiskiestLimits() (keep, over int) {
	read := func(name string) int {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "  ignoring unparseable %s=%q\n", name, v)
			return 0
		}
		return n
	}
	keep = read("KAI_REVIEW_RISKIEST_FILES")
	if keep == 0 {
		return 0, 0
	}
	over = read("KAI_REVIEW_RISKIEST_OVER")
	if over < keep {
		over = keep
	}
	return keep, over
}

// rcFileRiskScore ranks one changed file by how likely a defect in it is to
// matter, from its path and size alone, with rcPreReviewRisk's own words:
//
//   - code that ships outranks tests and docs, whatever their size;
//   - a path that names auth, billing or migrations, or a .sql file, outranks
//     other code;
//   - then deploy config, dependency manifests and CI;
//   - then size, up to 500 changed lines: past that a file is more often
//     generated or moved than written.
func rcFileRiskScore(f finding.DiffFile) int {
	churn := f.Added + f.Removed
	if churn > 500 {
		churn = 500
	}
	score := churn / 10
	if rcRiskTestOrDoc(f.Path) {
		return score
	}
	score += 100
	l := strings.ToLower(f.Path)
	base := path.Base(l)
	area, config := strings.HasSuffix(base, ".sql"), false
	for _, w := range strings.FieldsFunc(l, func(r rune) bool { return r == '/' || r == '_' || r == '-' || r == '.' }) {
		if _, ok := rcRiskAreas[w]; ok {
			area = true
		}
		if rcRiskDeployWords[w] {
			config = true
		}
	}
	if rcRiskManifests[base] || strings.HasPrefix(l, ".github/workflows/") || base == ".gitlab-ci.yml" || base == "dockerfile" {
		config = true
	}
	if area {
		score += 100
	}
	if config {
		score += 30
	}
	return score
}

// rcRiskiestCut is the cut for a change whose review covers scope (every
// changed file, or a re-review's focus), or nil when nothing is cut: no limit
// set, or no more than over reviewable files in scope. files is the whole
// change, which is where a file's size comes from and what decides whether
// vendor/ is a dependency bump.
func rcRiskiestCut(files []finding.DiffFile, scope []string, keep, over int) *rcFileCut {
	if keep < 1 {
		return nil
	}
	reviewable := rcReviewablePaths(scope, rcPathsOf(files))
	if len(reviewable) <= over || len(reviewable) <= keep {
		return nil
	}
	byPath := make(map[string]finding.DiffFile, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	ranked := make([]finding.DiffFile, 0, len(reviewable))
	for _, p := range reviewable {
		f, ok := byPath[p]
		if !ok {
			f = finding.DiffFile{Path: p}
		}
		ranked = append(ranked, f)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		si, sj := rcFileRiskScore(ranked[i]), rcFileRiskScore(ranked[j])
		if si != sj {
			return si > sj
		}
		ci, cj := ranked[i].Added+ranked[i].Removed, ranked[j].Added+ranked[j].Removed
		if ci != cj {
			return ci > cj
		}
		return ranked[i].Path < ranked[j].Path
	})
	cut := &rcFileCut{Of: len(reviewable)}
	for _, f := range ranked[:keep] {
		cut.Kept = append(cut.Kept, f.Path)
	}
	return cut
}

// rcCutDiff is the diff the reviewer is given under a cut: the kept files'
// sections whole, every other file reduced to its header and one line saying
// how much changed, then cut at maxBytes like any diff. raw is the whole diff.
func rcCutDiff(raw string, kept []string, maxBytes int) string {
	keep := make(map[string]bool, len(kept))
	for _, p := range kept {
		keep[p] = true
	}
	var b strings.Builder
	var secs [][]string
	for _, l := range strings.SplitAfter(raw, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			secs = append(secs, []string{l})
		case len(secs) == 0:
			b.WriteString(l)
		default:
			secs[len(secs)-1] = append(secs[len(secs)-1], l)
		}
	}
	for _, sec := range secs {
		if p := rcSectionPath(sec); p == "" || keep[p] {
			rcWriteLines(&b, sec)
			continue
		}
		rcWriteLines(&b, sec[:1])
		fmt.Fprintf(&b, "(not part of this review: %d changed line(s), not shown)\n", rcChangedLines(sec[1:]))
	}
	d := b.String()
	if len(d) > maxBytes {
		return d[:maxBytes] + "\n... (diff truncated)\n"
	}
	return d
}

// rcFileCutBlock tells the reviewer what it is reviewing and what it is not,
// so it spends its turns on the kept files and does not report on code it
// was not shown. "" with no cut.
func rcFileCutBlock(cut *rcFileCut) string {
	if cut == nil {
		return ""
	}
	return fmt.Sprintf("LARGE CHANGE: this change has %d reviewable files, and this review covers the %d ranked riskiest: %s. "+
		"Review those files. The other changed files appear in the diff as a header and a line count only: "+
		"they are not part of this review, so do not open them to review them and do not report findings in them. "+
		"You may still open any file, changed or not, to check how a reviewed file is called or what it calls.\n\n",
		cut.Of, len(cut.Kept), strings.Join(cut.Kept, ", "))
}
