package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// The diff sweep: a line-by-line read of every changed hunk, run beside the
// grounded review and merged into its draft before the publication gate.
//
// Why it exists. On the 2026-09-27 benchmark rerun Kai missed 119 of 173
// known bugs, and the misses were traced one by one against the production
// logs: 67 were in files the reviewer had opened and it never raised them,
// and 95 of the 119 sat in the diff itself, where a careful read of the hunk
// would catch them. The grounded reviewer investigates: it picks one or two
// themes, follows them across the graph, and stops. Keycloak #36880 (+866
// lines, three High bugs) got one finding; Grafana #103633 spent nine searches
// on one cache question and walked past two plain bugs in the same hunk;
// every test, docstring and typo bug on the benchmark was missed. The sweep
// does the part the investigation skips — read every changed line, in every
// changed file, test files included — and hands what it finds to the same
// gate that checks the reviewer's own claims, so it adds findings without
// lowering the bar a finding must clear.
//
// It reads the diff with wide context, in chunks, in parallel, and never
// blocks the review: a chunk that fails or runs out of time contributes
// nothing, and the review proceeds with what the reviewer found.

const rcSweepSystem = `You are sweeping a code change for defects, line by line. You get part of the change as a unified diff with wide context. Read EVERY added and changed line in EVERY file shown — test files, fixtures, docs, config, styles and translations included — and list every concrete defect the change introduces or leaves in the lines it touches.

A defect is something wrong as written, that you can point at on one line and explain in one sentence. Check each changed line for:
- WRONG VALUE OR VARIABLE: a copy-paste slip (the wrong variable, field, metric tag, map key, flag or constant), a value that disagrees with the data or comment beside it, an off-by-one, a wrong unit.
- WRONG CONDITION: inverted or incomplete logic, a guard that tests the wrong thing, "==" where identity or order matters, a branch that can never run.
- MISSING NULL OR ERROR HANDLING: a dereference, index or key access on a value the new code can produce as null/None/nil/undefined/empty; an error or exception that escapes where the surrounding code handles it; a failed call whose result is used anyway.
- NORMALIZATION MISMATCH: two sides of a comparison, lookup or uniqueness check normalized differently (case, whitespace, trailing slash, string vs symbol, id kind), or data written without the normalization its readers apply.
- CONTRACT MISMATCH VISIBLE HERE: a return shape, argument order or identifier kind that the lines shown use inconsistently; an abstract method a new subclass does not implement; a function whose result its callers use in a way it does not support.
- SECURITY: unescaped or unsanitized output of user-controlled content, a permission check that grants more than it names, a missing authorization or ownership check on a new path, a request to a user-supplied URL, a secret compared with ==.
- CONCURRENCY: a read-check-write on shared state (a counter, a one-time code, a status) with nothing serializing it.
- TEST BUGS: a test that cannot fail, asserts the wrong value, or tests something other than its name says.
- DOCS, COMMENTS AND TEXT: a docstring or comment that now contradicts the code below it, a typo in an identifier, key, message or user-facing string, a broken template tag.
- STYLE THAT BREAKS SOMETHING: an unused or dead new code path, a magic number repeated where the constant exists, an inconsistency with the sibling code that changes behaviour.

Rules:
- Only defects in or directly caused by the changed lines. Name the exact file path and the line number in the NEW file (the "+" side; use the hunk header to count).
- Be concrete: say what is wrong and what it should be. No hedging ("might", "could potentially", "consider"), no general advice, no "add a test" without a specific wrong behaviour it would catch, no praise.
- Do not report a problem that depends on a future code change.
- One defect per line of output. If the same mistake repeats, write it once and add "(also: path:line, …)".
- A line can carry more than one defect. After you find one, keep checking the same lines for a DIFFERENT one — a second wrong value, a missing check beside the wrong one, a test that also asserts the wrong thing — and list each on its own bullet. Stopping at the first plausible problem is how real defects are missed.
- It is fine to report nothing for a chunk that is correct. Do not pad.
- Never write a bullet for a line you checked and found correct. Think it through before writing; a bullet is a defect, not a note.

Output ONLY this, nothing before or after:
ISSUES:
- <path>:<line> — <the defect, one sentence>
or, when there is nothing:
ISSUES:
- (none)`

// Sweep budgets. The sweep runs beside the grounded review, so its deadline
// is its own; what it has not finished by then is simply not merged.
//
// The output budget is generous on purpose: on a reasoning model the hidden
// chain of thought is drawn from the same max_tokens, and a budget sized for
// the visible answer alone comes back empty.
var (
	rcSweepDeadline     = 10 * time.Minute
	rcSweepChunkBytes   = 48 * 1024
	rcSweepMaxChunks    = 12
	rcSweepParallel     = 6
	rcSweepContextLines = 20
	rcSweepMaxFileBytes = 64 * 1024
	rcSweepMaxTokens    = 16000
)

// rcSweepSkip reports files whose diff is not code anyone reviews line by
// line: lockfiles, vendored and generated output, minified bundles, binaries.
var rcSweepSkip = regexp.MustCompile(`(?i)(^|/)(package-lock\.json|yarn\.lock|pnpm-lock\.yaml|go\.sum|poetry\.lock|Gemfile\.lock|Cargo\.lock|composer\.lock)$|(^|/)(vendor|node_modules|dist|build)/|\.min\.(js|css)$|\.(png|jpe?g|gif|svg|ico|pdf|woff2?|ttf|eot|zip|gz|jar|snap)$|(^|/)__snapshots__/|_pb2\.py$|\.pb\.go$|\.generated\.`)

// rcSweepResult is what the sweep hands the gate: the defects it proposes,
// and the diff chunks they were read from, which become citable sources.
type rcSweepResult struct {
	Runs    []rcSweepRun
	Issues  []string
	Sources []rcSource
	Chunks  int
	Failed  int
}

// rcSweepPatches returns each changed file's diff with wide context, keyed by
// path, in diff order.
func rcSweepPatches(base, ref string) ([]string, map[string]string) {
	args := []string{"--no-pager", "diff", "--no-color", fmt.Sprintf("-U%d", rcSweepContextLines)}
	if strings.TrimSpace(base) != "" {
		args = append(args, base+"..."+ref)
	} else {
		args = []string{"--no-pager", "show", "--no-color", fmt.Sprintf("-U%d", rcSweepContextLines), "--format=", ref}
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil || len(out) == 0 {
		return nil, nil
	}
	return rcSplitDiff(string(out))
}

// rcSplitDiff splits a unified diff into per-file patches, keyed by the new
// path (the old one for a deletion), preserving order.
func rcSplitDiff(diff string) ([]string, map[string]string) {
	var order []string
	patches := map[string]string{}
	parts := strings.Split(diff, "\ndiff --git ")
	for i, p := range parts {
		if i == 0 {
			p = strings.TrimPrefix(p, "diff --git ")
		}
		if strings.TrimSpace(p) == "" {
			continue
		}
		path := ""
		for _, ln := range strings.Split(p, "\n") {
			if strings.HasPrefix(ln, "+++ b/") {
				path = strings.TrimPrefix(ln, "+++ b/")
				break
			}
			if strings.HasPrefix(ln, "--- a/") && path == "" {
				path = strings.TrimPrefix(ln, "--- a/")
			}
		}
		if path == "" {
			continue
		}
		if _, seen := patches[path]; !seen {
			order = append(order, path)
		}
		patches[path] = "diff --git " + p
	}
	return order, patches
}

// rcSweepChunks packs the reviewable patches into chunks of at most
// rcSweepChunkBytes, test files last so a cap on the number of chunks drops
// them before it drops production code, and one oversized file truncated
// rather than skipped.
func rcSweepChunks(order []string, patches map[string]string) [][]string {
	var code, tests []string
	for _, p := range order {
		if rcSweepSkip.MatchString(p) {
			continue
		}
		if rcLooksLikeTest(p) {
			tests = append(tests, p)
		} else {
			code = append(code, p)
		}
	}
	var chunks [][]string
	var cur []string
	size := 0
	for _, p := range append(code, tests...) {
		n := len(patches[p])
		if n > rcSweepMaxFileBytes {
			n = rcSweepMaxFileBytes
		}
		if size > 0 && size+n > rcSweepChunkBytes {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	if len(chunks) > rcSweepMaxChunks {
		chunks = chunks[:rcSweepMaxChunks]
	}
	return chunks
}

var rcTestPath = regexp.MustCompile(`(?i)(^|/)(tests?|spec|__tests__|testdata|fixtures?)/|(_test\.go|_spec\.rb|\.test\.[jt]sx?|\.spec\.[jt]sx?|Test\.java|Tests?\.cs|_test\.py|test_[^/]*\.py)$`)

func rcLooksLikeTest(path string) bool { return rcTestPath.MatchString(path) }

// rcRunSweep reads every chunk with the review model and returns the defects
// it proposes, each grounded to a changed path. Chunks run in parallel; a
// chunk that errors or times out is counted and skipped.
func rcRunSweep(ctx context.Context, prov provider.Provider, model, intent string, order []string, patches map[string]string) rcSweepResult {
	chunks := rcSweepChunks(order, patches)
	res := rcSweepResult{Chunks: len(chunks)}
	if len(chunks) == 0 {
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, rcSweepDeadline)
	defer cancel()
	type out struct {
		attempts []rcOutputAttempt
		issues   []string
		source   string
		err      error
	}
	outs := make([]out, len(chunks))
	sem := make(chan struct{}, rcSweepParallel)
	var wg sync.WaitGroup
	for i, files := range chunks {
		wg.Add(1)
		go func(i int, files []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var b strings.Builder
			if strings.TrimSpace(intent) != "" {
				b.WriteString("INFERRED INTENT (model hypothesis, never author approval):\n")
				b.WriteString(rcOneLine(intent, 600))
				b.WriteString("\n\n")
			}
			b.WriteString("FILES IN THIS PART (each finding.file must be one of these paths):\n")
			for _, f := range files {
				fmt.Fprintf(&b, "  %s\n", f)
			}
			b.WriteString("\nDIFF (wide context):\n")
			var src strings.Builder
			for _, f := range files {
				p := patches[f]
				if len(p) > rcSweepMaxFileBytes {
					p = p[:rcSweepMaxFileBytes] + "\n... (file diff truncated)\n"
				}
				src.WriteString(p)
				if !strings.HasSuffix(p, "\n") {
					src.WriteString("\n")
				}
			}
			b.WriteString(src.String())
			changed := map[string]bool{}
			for _, file := range files {
				changed[file] = true
			}
			issues, attempts, err := rcSweepChunk(ctx, prov, provider.Request{
				Model:           model,
				System:          rcSweepSystem + rcAuthorPolicy + rcSweepOutputInstruction,
				MaxTokens:       rcSweepMaxTokens,
				ReasoningEffort: rcStageEffort(rcStageSweep),
				Messages:        []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: b.String()}}}},
			}, changed, i+1)
			outs[i] = out{issues: issues, attempts: attempts, source: src.String(), err: err}
		}(i, files)
	}
	wg.Wait()
	seen := map[string]bool{}
	run := rcSweepRun{Model: model, Chunks: len(chunks)}
	for _, o := range outs {
		run.Attempts = append(run.Attempts, o.attempts...)
		if o.err != nil {
			res.Failed++
			fmt.Fprintf(os.Stderr, "  sweep: a chunk failed (%v) — the review proceeds without it\n", o.err)
			continue
		}
		if o.source != "" {
			res.Sources = append(res.Sources, rcRowSource(o.source))
		}
		for _, is := range o.issues {
			if key := strings.ToLower(is); !seen[key] {
				seen[key] = true
				res.Issues = append(res.Issues, is)
			}
		}
	}
	run.Failed = res.Failed
	res.Runs = append(res.Runs, run)
	return res
}

var rcSweepBullet = regexp.MustCompile(`^\s*[-*]\s+(.+)$`)

// rcSweepRetracted matches a bullet the model wrote and then took back in the
// same sentence. On the first live chunks the reviewer model reasoned in the
// list itself — "…so this line is fine — no defect here. (Correction: no
// defect.)" — and a retraction handed to the gate only spends its budget.
var rcSweepRetracted = regexp.MustCompile(`(?i)\bno defect\b|\(correction|\bnot a defect\b|\bthis (line|code) is (fine|correct)\b|\bwhich is correct\b|\bso (this|it) is fine\b`)

// rcSweepIssues parses the sweep's ISSUES list, keeping only bullets that
// begin with a changed path and a line number — the shape the gate and the
// finding's grounding both require.
func rcSweepIssues(text string, changed map[string]bool) []string {
	var issues []string
	in := false
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if k, _, ok := rcMachineLine(t); ok && (k == "issues" || k == "findings") {
			in = true
			continue
		}
		if !in {
			continue
		}
		m := rcSweepBullet.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		item := strings.TrimSpace(strings.Trim(m[1], "`"))
		if strings.HasPrefix(item, "(none)") {
			continue
		}
		path := item
		if i := strings.Index(item, ":"); i > 0 {
			path = item[:i]
		} else {
			continue
		}
		rest := item[len(path)+1:]
		if len(rest) == 0 || rest[0] < '0' || rest[0] > '9' {
			continue
		}
		if !changed[path] {
			continue
		}
		if rcSweepRetracted.MatchString(item) {
			continue
		}
		issues = append(issues, item)
	}
	return issues
}

// rcDraftWithSweep appends the sweep's issues to the draft's ISSUES list. The
// reviewer's own bullets come first and keep their wording; a sweep bullet at
// a location the reviewer already raised is left for rcMergeDuplicateIssues
// to fold. A draft without a coda gets none: there is nothing to merge into,
// and the conclusion path will write one.
// rcSameDefectThere reports whether a sweep bullet restates one of the
// reviewer's bullets at the same location. A location alone used to decide
// that, which dropped a DIFFERENT defect on a line the reviewer had flagged
// for something else — the sweep's second finding on a line never reached the
// gate. Now the wording decides: a bullet that shares most of its substantive
// words with one already there is the same defect; otherwise both go to the
// gate, which folds genuine repeats (rcMergeDuplicateIssues) and checks the
// rest.
func rcSameDefectThere(bullet string, there []string) bool {
	if len(there) == 0 {
		return false
	}
	words := rcDefectWords(bullet)
	for _, other := range there {
		if rcWordOverlap(words, rcDefectWords(other)) >= rcSameDefectOverlap {
			return true
		}
	}
	return false
}

// rcSameDefectOverlap is the share of the smaller bullet's substantive words
// the two must have in common to count as one defect.
const rcSameDefectOverlap = 0.5

var rcDefectWordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// rcDefectWords is a bullet's substantive words: identifiers and words of four
// or more letters, lowercased, after its location.
func rcDefectWords(bullet string) map[string]bool {
	if i := strings.Index(bullet, " — "); i >= 0 {
		bullet = bullet[i+len(" — "):]
	}
	out := map[string]bool{}
	for _, w := range rcDefectWordRe.FindAllString(bullet, -1) {
		out[strings.ToLower(w)] = true
	}
	return out
}

func rcWordOverlap(a, b map[string]bool) float64 {
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	if len(small) == 0 {
		return 0
	}
	n := 0
	for w := range small {
		if large[w] {
			n++
		}
	}
	return float64(n) / float64(len(small))
}

func rcDraftWithSweep(draft string, extra []string) string {
	if strings.HasPrefix(strings.TrimSpace(draft), "{") || strings.HasPrefix(strings.TrimSpace(draft), "```") {
		if output, err := rcDecodeReview(draft); err == nil {
			draft = output.draft()
		}
	}
	if len(extra) == 0 {
		return draft
	}
	i := strings.Index(draft, rcReviewDataMarker)
	if i < 0 {
		return draft
	}
	head, coda := draft[:i+len(rcReviewDataMarker)], draft[i+len(rcReviewDataMarker):]
	lines := strings.Split(coda, "\n")
	have := map[string][]string{} // location → the reviewer's bullets there
	issuesAt, decisionsAt := -1, -1
	for j, ln := range lines {
		key, _, labelled := rcMachineLine(strings.TrimSpace(ln))
		switch {
		case labelled && (key == "issues" || key == "findings"):
			issuesAt = j
		case labelled && key == "decisions":
			decisionsAt = j
		}
		if m := rcSweepBullet.FindStringSubmatch(ln); m != nil {
			loc := rcSweepLocation(m[1])
			have[loc] = append(have[loc], m[1])
		}
	}
	var add []string
	for _, is := range extra {
		if rcSameDefectThere(is, have[rcSweepLocation(is)]) {
			continue
		}
		add = append(add, "- "+is)
	}
	if len(add) == 0 {
		return draft
	}
	var out []string
	switch {
	case issuesAt >= 0:
		// Insert after the last bullet of the ISSUES list, dropping a "(none)".
		end := issuesAt + 1
		for end < len(lines) {
			t := strings.TrimSpace(lines[end])
			if !strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "*") {
				break
			}
			end++
		}
		kept := []string{}
		for _, ln := range lines[issuesAt+1 : end] {
			if !strings.Contains(strings.ToLower(ln), "(none)") {
				kept = append(kept, ln)
			}
		}
		out = append(out, lines[:issuesAt+1]...)
		out = append(out, kept...)
		out = append(out, add...)
		out = append(out, lines[end:]...)
	case decisionsAt >= 0:
		out = append(out, lines[:decisionsAt]...)
		out = append(out, "ISSUES:")
		out = append(out, add...)
		out = append(out, lines[decisionsAt:]...)
	default:
		out = append(out, strings.TrimRight(coda, "\n"), "ISSUES:")
		out = append(out, add...)
		return head + strings.Join(out, "\n") + "\n"
	}
	return head + strings.Join(out, "\n")
}

var rcLocationRe = regexp.MustCompile(`^\s*([^\s:]+):(\d+)`)

// rcSweepLocation is an issue's "path:line", or "" when it has none.
func rcSweepLocation(item string) string {
	m := rcLocationRe.FindStringSubmatch(strings.Trim(strings.TrimSpace(item), "`"))
	if m == nil {
		return ""
	}
	return m[1] + ":" + m[2]
}

// rcSweepPaths lists the files a sweep issue set touches, sorted, for logs.
func rcSweepPaths(issues []string) []string {
	set := map[string]bool{}
	for _, is := range issues {
		if loc := rcSweepLocation(is); loc != "" {
			set[loc[:strings.LastIndex(loc, ":")]] = true
		}
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// rcStartSweep starts the diff sweep beside the grounded review and returns
// the channel its result arrives on. KAI_REVIEW_SWEEP=0 turns it off; the
// channel then carries an empty result at once.
func rcStartSweep(ctx context.Context, prov provider.Provider, model, intent, base, ref string) <-chan rcSweepResult {
	ch := make(chan rcSweepResult, 1)
	if os.Getenv("KAI_REVIEW_SWEEP") == "0" {
		ch <- rcSweepResult{}
		return ch
	}
	model = rcSweepModel(model)
	go func() {
		order, patches := rcSweepPatches(base, ref)
		started := time.Now()
		res := rcRunSweep(ctx, prov, model, intent, order, patches)
		fmt.Fprintf(os.Stderr, "  sweep: %d chunk(s) read in %s, %d failed, %d defect(s) proposed\n",
			res.Chunks, time.Since(started).Round(time.Second), res.Failed, len(res.Issues))
		ch <- res
	}()
	return ch
}

// rcSweepModel is the sweep's model: the review profile's, else
// KAI_SWEEP_MODEL, which overrides the model for the sweep alone so the pass
// can be tried on a cheaper or faster model without changing the review's,
// else the review model.
func rcSweepModel(reviewModel string) string {
	model := reviewModel
	if m := strings.TrimSpace(os.Getenv("KAI_SWEEP_MODEL")); m != "" {
		model = m
	}
	return rcStageModel(rcStageSweep, model)
}

// rcAwaitSweep waits for the sweep. It is bounded by the sweep's own
// deadline, so the gate never waits longer than that; a nil channel (no sweep
// was started) yields nothing.
func rcAwaitSweep(ch <-chan rcSweepResult) rcSweepResult {
	if ch == nil {
		return rcSweepResult{}
	}
	return <-ch
}

// rcIssuesOf lists a draft's ISSUES bullets.
func rcIssuesOf(draft string) []string {
	_, issues, _, _, _, _ := rcParseReviewOutput(draft)
	return issues
}

// rcMergeSweeps joins two sweeps over the same hunks: every distinct issue
// from both, and one set of chunk sources (both chunked the same diff).
func rcMergeSweeps(a, b rcSweepResult) rcSweepResult {
	seen := map[string]bool{}
	var issues []string
	for _, is := range append(append([]string(nil), a.Issues...), b.Issues...) {
		k := rcIssueKey(is)
		if seen[k] {
			continue
		}
		seen[k] = true
		issues = append(issues, is)
	}
	out := a
	out.Issues = issues
	out.Chunks = a.Chunks + b.Chunks
	out.Failed = a.Failed + b.Failed
	out.Runs = append(append([]rcSweepRun(nil), a.Runs...), b.Runs...)
	sourceSeen := map[string]bool{}
	out.Sources = nil
	for _, src := range append(append([]rcSource(nil), a.Sources...), b.Sources...) {
		if !sourceSeen[src.Text] {
			out.Sources = append(out.Sources, src)
			sourceSeen[src.Text] = true
		}
	}
	return out
}
