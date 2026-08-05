package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"kai/internal/config"

	"github.com/kaicontext/kai-engine/agent"
	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/reviewanalyze"
)

// review-commit is the agentic, graph-grounded PR reviewer: the same read-only
// agent `kai verify` uses (via kit), but tuned to REVIEW a commit for defects and
// emit a finding.Finding — the shape the findings inbox stores. This is the
// headless-kit reviewer the CI review workflow runs (kai-review.yml), replacing
// the one-shot reviewanalyze judge that can't see cross-file / concurrency bugs.
const maxReviewCommitDiffBytes = 120 * 1024

// rcInferIntentSystem reconstructs a commit's goal from its message + diff (one
// focused call), mirroring kit verify-commit's intent reconstruction.
const rcInferIntentSystem = "You reconstruct the INTENT of a merged pull request from its commit message and its diff. " +
	"Write the intent as a short specification of the desired end-state — what the change is supposed to ACHIEVE, not a " +
	"summary of which lines changed. 2 to 6 sentences. Be FAITHFUL to the author, not stricter than they were. Treat the " +
	"commit message as the source of truth for the goal; the diff is only evidence. Output only the intent prose: no " +
	"preamble, no markdown headers, no fences."

// rcReviewSystem drives the agentic reviewer: hunt for concrete defects, confirm
// each against the graph, and emit a strict parseable block.
const rcReviewSystem = "You are a rigorous, READ-ONLY code reviewer. You are given the author's CONTEXT, the reconstructed " +
	"INTENT, and a code DIFF. Your job is to find real DEFECTS the change introduces — not style nits. You cannot edit, " +
	"write, or run commands, and you MUST NOT ask the user anything. " +
	"Go deeper than the textual diff using the graph tools: call kai_callers and kai_dependents on changed symbols to check " +
	"the change is complete and consistent across the codebase (a changed signature whose callers were not updated is a " +
	"defect), and kai_context to understand a symbol before flagging it. Use the graph to CONFIRM a suspected defect is real " +
	"and reachable before reporting it — do not speculate. " +
	"Hunt specifically for: concurrency bugs (data races, state mutated outside its lock, unsynchronized map access), " +
	"security issues, resource leaks (goroutines/tickers/files/connections never stopped or closed), correctness bugs " +
	"(off-by-one, wrong refill/limit math, nil dereference, truncation), and error handling that swallows or misroutes " +
	"failures. For SECURITY, examine EVERY comparison that involves a secret or credential — an API key, token, password, " +
	"session id, HMAC or signature: a plain `==`/`!=` (or a map/string compare) is a timing side-channel and MUST use a " +
	"constant-time compare (subtle.ConstantTimeCompare / hmac.Equal); flag each one. Scrutinize authentication, " +
	"authorization, bypass, allowlist, and admin/override paths specifically, and check for missing input validation and " +
	"injection. Systematically walk the changed code for each category above before concluding — do not stop at the first " +
	"bug you find. " +
	"Also judge whether the change matches its stated INTENT: 'verified' (matches), 'partial' (mostly, with gaps), or " +
	"'diverges' (does something materially different or broken). Judge against the author's ACTUAL goal, not a stricter one. " +
	"Finish with EXACTLY this block and nothing after it. One finding per line under FINDINGS; omit the line if there are none:\n" +
	"FINDINGS:\n" +
	"- [category] path:line — one-sentence defect and why it is wrong\n" +
	"INTENT_MATCH: verified|partial|diverges\n" +
	"NOTE: <one sentence overall assessment>"

const rcMaxAuthorContextBytes = 8 * 1024

var (
	reviewCommitFormat string
	reviewCommitBase   string
)

var reviewCommitCmd = &cobra.Command{
	Use:   "review-commit <commit>",
	Short: "Agentic, graph-grounded code review of a commit — emits a finding (headless)",
	Long: "Reviews a commit (typically a squash-merged PR) with a read-only, graph-grounded AGENT: it reconstructs the\n" +
		"author's intent, then hunts for concrete defects (concurrency, security, resource leaks, correctness, error\n" +
		"handling), using kai_callers / kai_dependents / kai_context to confirm each is real and reachable across the\n" +
		"codebase. Emits a finding.Finding (verdict + intent + risks + diff) — the same JSON the findings inbox stores.\n\n" +
		"This is what the CI review workflow runs. Requires a captured graph (`kai capture`).",
	Args: cobra.ExactArgs(1),
	RunE: runReviewCommit,
}

func init() {
	reviewCommitCmd.Flags().StringVar(&reviewCommitFormat, "format", "text", "output format: text|json")
	reviewCommitCmd.Flags().StringVar(&reviewCommitBase, "base", "", "review the aggregate diff of <base>...<commit> (PR range) instead of a single commit")
	rootCmd.AddCommand(reviewCommitCmd)
}

func runReviewCommit(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ref := args[0]
	cwd, _ := os.Getwd()

	set, outcome := projects.Discover(cwd)
	if outcome != projects.OutcomeRootsFound {
		return fmt.Errorf("not a kai project here — run `kai capture` first")
	}
	if err := set.Open(); err != nil {
		return fmt.Errorf("opening projects: %w", err)
	}
	defer set.Close()

	hash, subject, body, err := rcCommitMeta(ref)
	if err != nil {
		return err
	}
	diff := rcCommitDiff(reviewCommitBase, ref, maxReviewCommitDiffBytes)
	if strings.TrimSpace(diff) == "" {
		return fmt.Errorf("commit %s has an empty diff (merge commit? try a child, or pass --base)", rcShort(hash))
	}

	prov, model := rcReviewProvider()
	if prov == nil {
		return fmt.Errorf("no LLM provider available (run `kai login` or set ANTHROPIC_API_KEY)")
	}

	fmt.Fprintf(os.Stderr, "kai review-commit %s · %s\n", rcShort(hash), subject)
	fmt.Fprintf(os.Stderr, "  reconstructing intent (model %s)…\n", model)
	intent, err := rcInferIntent(ctx, prov, model, subject, body, diff)
	if err != nil {
		return fmt.Errorf("infer intent: %w", err)
	}

	authorContext := subject
	if strings.TrimSpace(body) != "" {
		authorContext += "\n\n" + body
	}

	fmt.Fprintf(os.Stderr, "  reviewing against the graph…\n\n")
	raw, err := rcRunReviewAgent(ctx, set, prov, model, authorContext, intent, diff)
	if err != nil {
		return err
	}

	risks, match, note := rcParseReviewOutput(raw)
	if match == finding.MatchUnknown {
		// The review itself ran — this is the closing block, not a failed review.
		// Say so on stderr with the raw tail, because the finding records only
		// "unknown" and the console renders that as "no contract resolved for this
		// change". Without this line the failure is invisible: the job exits 0, the
		// log shows a clean run, and the amber badge is the only symptom. The tail
		// is what distinguishes a reviewer that stated no verdict from one whose
		// verdict this parser could not read, and it is the signal for how often
		// the closing block drifts.
		fmt.Fprintf(os.Stderr,
			"  ⚠ no INTENT_MATCH verdict could be read from the reviewer's closing block — recording intent=unknown\n"+
				"    reviewer's closing text: %s\n", rcOneLine(rcTail(raw, 400), 400))
	}
	added, removed, files := rcCommitDiffStat(reviewCommitBase, ref)

	// Blast radius: walk the captured graph outward from the changed files so the
	// finding shows what the change reaches (callers/importers), not just the diff.
	// headHex="" walks the freshly-captured (single-snapshot) graph unscoped.
	// Non-fatal — blast is a panel, not the review itself.
	changedPaths := make([]string, 0, len(files))
	for _, df := range files {
		changedPaths = append(changedPaths, df.Path)
	}
	blast, berr := reviewanalyze.BlastFor(ctx, set.Primary().DB, changedPaths, "")
	if berr != nil {
		fmt.Fprintf(os.Stderr, "  blast radius unavailable: %v\n", berr)
	}

	from := rcShort(rcParentHash(hash))
	if reviewCommitBase != "" {
		if b, e := exec.Command("git", "rev-parse", reviewCommitBase).Output(); e == nil {
			from = rcShort(strings.TrimSpace(string(b)))
		}
	}

	// Emit each defect as a risk-tagged grounded Claim so the findings inbox
	// denormalizes RiskCount/ClaimsCount correctly (storeFinding counts claims
	// whose tag == "risk"); Intent.Risks keeps the same list for the intent panel.
	claims := make([]finding.Claim, 0, len(risks))
	for _, r := range risks {
		claims = append(claims, finding.Claim{Statement: r, Tag: finding.TagRisk, Resolved: true, Verified: true})
	}

	f := finding.Finding{
		ID:      rcFindingID(hash),
		Title:   subject,
		Author:  rcCommitAuthor(hash),
		From:    from,
		To:      rcShort(hash),
		Added:   added,
		Removed: removed,
		Files:   len(files),
		Verdict: finding.VerdictAwaiting,
		Intent: finding.Intent{
			Stated: subject,
			Match:  match,
			Note:   note,
			Risks:  risks,
		},
		Claims: claims,
		Diff:   finding.Diff{Files: files},
		Blast:  blast,
	}

	if reviewCommitFormat == "json" {
		out, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling finding: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}

	fmt.Println("──────── review ────────")
	fmt.Printf("intent match: %s\n", f.Intent.Match)
	if note != "" {
		fmt.Printf("note: %s\n", note)
	}
	if len(risks) == 0 {
		fmt.Println("findings: none")
	} else {
		fmt.Printf("findings (%d):\n", len(risks))
		for _, r := range risks {
			fmt.Printf("  • %s\n", r)
		}
	}
	fmt.Println("─────────────────────────")
	return nil
}

// rcReviewProvider builds the LLM provider (kailab creds → OpenRouter, or an
// ANTHROPIC_API_KEY fallback), reusing the gate/planner plumbing.
func rcReviewProvider() (provider.Provider, string) {
	cfg, err := config.Load(kaiDir)
	if err != nil {
		return nil, ""
	}
	prov, reviewModel, _, err := buildGateProvider(cfg)
	if err != nil {
		return nil, ""
	}
	return prov, reviewModel
}

func rcInferIntent(ctx context.Context, prov provider.Provider, model, subject, body, diff string) (string, error) {
	var in strings.Builder
	in.WriteString("COMMIT MESSAGE:\n")
	in.WriteString(strings.TrimSpace(subject))
	if strings.TrimSpace(body) != "" {
		in.WriteString("\n\n")
		in.WriteString(strings.TrimSpace(body))
	}
	in.WriteString("\n\nDIFF:\n")
	in.WriteString(diff)

	resp, err := prov.Send(ctx, provider.Request{
		Model:     model,
		System:    rcInferIntentSystem,
		MaxTokens: 600,
		Messages:  []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: in.String()}}}},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return out.String(), nil
}

func rcRunReviewAgent(ctx context.Context, set *projects.Set, prov provider.Provider, model, sourceContext, intent, diff string) (string, error) {
	primary := set.Primary()

	var user strings.Builder
	if sc := strings.TrimSpace(sourceContext); sc != "" {
		if len(sc) > rcMaxAuthorContextBytes {
			sc = sc[:rcMaxAuthorContextBytes] + "\n... (context truncated)"
		}
		user.WriteString("AUTHOR CONTEXT (the change author's own description):\n")
		user.WriteString(sc)
		user.WriteString("\n\n")
	}
	user.WriteString("INTENT:\n")
	user.WriteString(strings.TrimSpace(intent))
	user.WriteString("\n\nDIFF:\n")
	if strings.TrimSpace(diff) == "" {
		user.WriteString("(no changes)\n")
	} else {
		user.WriteString(diff)
		user.WriteString("\n")
	}
	prompt := "System: " + rcReviewSystem + "\n\n" + user.String()

	res, err := agent.Run(ctx, agent.Options{
		Projects:   set,
		Workspace:  primary.Path,
		Provider:   prov,
		Model:      model,
		Graph:      asGraphDB(primary.DB),
		ReadOnly:   true,
		EnableBash: false,
		MaxTurns:   20,
		Prompt:     prompt,
		Hooks: agent.Hooks{
			OnToolCall: func(name, inputJSON string) {
				fmt.Fprintf(os.Stderr, "  → %s %s\n", name, rcOneLine(inputJSON, 90))
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("review run: %w", err)
	}
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(res.FinalText), nil
}

// rcVerdictSynonyms is the closed set of verdict spellings this parser accepts.
// Every entry appears VERBATIM in the prompt this command sends (rcReviewSystem)
// or in the finding.Match enum it maps onto — nothing is here on the theory that
// a model "might" say it.
//
// That rule excludes two spellings kai-engine's judgeIntent happens to accept,
// "match" and "diverge": neither appears in any prompt in this codebase, so they
// are that parser's own defensive guesses, and inheriting them would launder a
// guess into a justification. The two parsers never read the same text anyway —
// judgeIntent prompts for JSON "matches|partial|diverges", this one prompts for
// "INTENT_MATCH: verified|partial|diverges" — so mirroring it was never required
// for correctness.
//
// Growing this list is allowed, but only from evidence: review-commit logs the
// reviewer's raw closing text whenever the verdict is unreadable (see
// verdictRead). Add a spelling when a real run produces it, not before.
var rcVerdictSynonyms = map[string]finding.Match{
	// finding.MatchVerified + rcReviewSystem: "INTENT_MATCH: verified|partial|diverges"
	"verified": finding.MatchVerified,
	// rcReviewSystem glosses the same verdict as "'verified' (matches)", so the
	// model is shown both words for one meaning — this is the spelling that was
	// observed being dropped.
	"matches": finding.MatchVerified,
	// finding.MatchPartial + rcReviewSystem's literal block.
	"partial": finding.MatchPartial,
	// finding.MatchDiverges + rcReviewSystem's literal block.
	"diverges": finding.MatchDiverges,
}

// rcStripDecoration removes the markdown a model wraps around a block it was told
// to emit verbatim — "**INTENT_MATCH:** verified" and "`INTENT_MATCH:` verified"
// both have to reach the same parse as the bare form. Only * and ` are removed:
// stripping _ would break the INTENT_MATCH key itself and mangle snake_case
// identifiers in the note.
func rcStripDecoration(s string) string {
	return strings.TrimSpace(strings.NewReplacer("*", "", "`", "").Replace(s))
}

// rcOpener returns the run of markdown a line opens with ("**", "`"), or "" for
// an undecorated line.
func rcOpener(s string) string {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] == '*' || t[i] == '`') {
		i++
	}
	return t[:i]
}

// rcUnwrap removes the wrapper the model put around a line, and NOTHING else.
// Only the exact token the line opened with is removed, and only from the ends:
// "**NOTE:** the fix matches `handleFoo`" must yield a note that still ends in a
// backtick, while "**NOTE: mostly good**" must not keep its closing "**".
//
// rcStripDecoration is right for the verdict, which is matched word by word, but
// wrong for prose. Blanket-stripping turns a note's `a*b` into "ab" and deletes
// the backticks around `os.Getenv`; trimming any trailing run eats the closing
// backtick of a note that legitimately ends in `code`. Both rewrite the
// reviewer's assessment on its way to the console, which is why this matches the
// opener instead of guessing.
func rcUnwrap(s, opener string) string {
	v := strings.TrimSpace(s)
	if opener == "" {
		return v
	}
	v = strings.TrimSpace(strings.TrimPrefix(v, opener))
	return strings.TrimSpace(strings.TrimSuffix(v, opener))
}

// rcVerdictWords returns the lowercased alphabetic words of s, so punctuation the
// model appends ("verified.", "verified — no gaps") can't hide the verdict.
func rcVerdictWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(rcStripDecoration(s)), func(r rune) bool {
		return !unicode.IsLetter(r)
	})
}

// rcVerdictFromValue maps the text following "INTENT_MATCH:" onto a Match, using
// the FIRST word so a trailing justification still resolves ("verified — no
// material gaps"). Only the first word: it is the one the prompt asks for, and
// reading the rest turns ordinary justification prose into a verdict —
// "diverges — the code no longer matches the stated intent" names two verdicts
// and means one. Reading only the first word is also what makes "not verified"
// unreadable rather than verified, since the negation is the first word and no
// synonym is.
func rcVerdictFromValue(v string) (finding.Match, bool) {
	for _, w := range rcVerdictWords(v) {
		m, ok := rcVerdictSynonyms[w]
		return m, ok
	}
	return "", false
}

// rcParseReviewOutput extracts the FINDINGS lines, INTENT_MATCH and NOTE from the
// reviewer's final text. match is MatchUnknown when no verdict could be read —
// whether because the reviewer stated none, because what it stated was
// unreadable, or because it stated two that disagree. The caller cannot tell
// those apart from match alone and does not need to: it logs the raw closing
// text, which shows which happened.
func rcParseReviewOutput(raw string) (risks []string, match finding.Match, note string) {
	match = finding.MatchUnknown
	var stated finding.Match
	conflict := false
	inFindings := false
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		opener := rcOpener(t)
		bare := rcUnwrap(t, opener)
		key := strings.ToUpper(rcStripDecoration(t))
		switch {
		case key == "FINDINGS:":
			// A second header starts the list over. The prompt says to finish with
			// this block and nothing after it, so the last one is the real one —
			// and a reviewer that quotes an EXAMPLE block earlier in its reasoning
			// (this command's own tests are full of them) would otherwise have the
			// quoted bullets stored as defects it never reported. Observed: a real
			// run recorded 3 findings, 2 of them quoted test fixtures.
			inFindings = true
			risks = nil
		case strings.HasPrefix(key, "INTENT_MATCH:"):
			inFindings = false
			if i := strings.Index(t, ":"); i >= 0 {
				if m, ok := rcVerdictFromValue(t[i+1:]); ok {
					// Two verdict lines that disagree are not a verdict. The model
					// quoting the block back ("`INTENT_MATCH: diverges`" as an
					// example) or correcting a draft it already emitted would
					// otherwise have whichever line came last recorded as fact,
					// making the result depend on emission order alone.
					if stated != "" && m != stated {
						conflict = true
					}
					stated = m
				}
			}
		case strings.HasPrefix(key, "NOTE:"):
			inFindings = false
			if i := strings.Index(t, ":"); i >= 0 {
				note = rcUnwrap(t[i+1:], opener)
			}
		// The bullet is tested AFTER unwrapping, like every other key on this
		// switch: a model that wraps a finding in bold ("**- [security] …**") is
		// doing the same thing it does to INTENT_MATCH, and matching the raw line
		// dropped that finding on the floor — a clean review with a real defect
		// silently missing from it.
		case inFindings && strings.HasPrefix(bare, "-"):
			if item := strings.TrimSpace(strings.TrimPrefix(bare, "-")); item != "" {
				risks = append(risks, item)
			}
		default:
			// The list ends at the first line that is not a bullet. Without this,
			// a closing block the model spelled differently leaves the parser
			// inside FINDINGS and its trailing prose is stored as a defect.
			inFindings = false
		}
	}
	if !conflict {
		match = stated
	}
	if match == "" {
		match = finding.MatchUnknown
	}
	return risks, match, note
}

func rcCommitMeta(ref string) (hash, subject, body string, err error) {
	h, e := exec.Command("git", "rev-parse", ref).Output()
	if e != nil {
		return "", "", "", fmt.Errorf("resolve commit %q: %w", ref, e)
	}
	hash = strings.TrimSpace(string(h))
	s, _ := exec.Command("git", "log", "-1", "--format=%s", hash).Output()
	b, _ := exec.Command("git", "log", "-1", "--format=%b", hash).Output()
	return hash, strings.TrimSpace(string(s)), strings.TrimSpace(string(b)), nil
}

func rcCommitDiff(base, ref string, maxBytes int) string {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", "--no-color", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--no-color", "--format=", ref).Output()
	}
	if err != nil || len(out) == 0 {
		return ""
	}
	if len(out) > maxBytes {
		return string(out[:maxBytes]) + "\n... (diff truncated)\n"
	}
	return string(out)
}

func rcCommitDiffStat(base, ref string) (added, removed int, files []finding.DiffFile) {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", "--numstat", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--numstat", "--format=", ref).Output()
	}
	if err != nil {
		return 0, 0, nil
	}
	// Also capture the actual unified diff hunks per file so the findings inbox
	// Code tab renders real added/removed lines, not just counts. numstat alone
	// gave us Added/Removed but left DiffFile.Patch empty (blank Code tab).
	patches := rcCommitPatches(base, ref)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		a, _ := strconv.Atoi(fields[0])
		r, _ := strconv.Atoi(fields[1])
		added += a
		removed += r
		path := fields[2]
		files = append(files, finding.DiffFile{Path: path, Action: "modified", Added: a, Removed: r, Patch: patches[path]})
	}
	return added, removed, files
}

// rcCommitPatches returns the unified-diff hunks for each changed file over the
// same base...ref range rcCommitDiffStat counts, keyed by (b-side) path. File
// headers (diff --git, index, ---/+++, mode/rename lines) are stripped; hunk
// headers (@@) and the +/-/context body are kept so the inbox can colorize them.
func rcCommitPatches(base, ref string) map[string]string {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--format=", ref).Output()
	}
	if err != nil {
		return nil
	}
	patches := map[string]string{}
	var curPath string
	var buf strings.Builder
	flush := func() {
		if curPath != "" {
			patches[curPath] = strings.TrimRight(buf.String(), "\n")
		}
		buf.Reset()
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			f := strings.Fields(line)
			curPath = ""
			if len(f) >= 4 {
				curPath = strings.TrimPrefix(f[len(f)-1], "b/")
			}
			continue
		}
		if curPath == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "--- "),
			strings.HasPrefix(line, "+++ "),
			strings.HasPrefix(line, "new file mode "),
			strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "old mode "),
			strings.HasPrefix(line, "new mode "),
			strings.HasPrefix(line, "similarity index "),
			strings.HasPrefix(line, "rename "),
			strings.HasPrefix(line, "Binary files "):
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()
	return patches
}

func rcCommitAuthor(hash string) string {
	out, _ := exec.Command("git", "log", "-1", "--format=%an", hash).Output()
	return strings.TrimSpace(string(out))
}

func rcParentHash(hash string) string {
	out, err := exec.Command("git", "rev-parse", hash+"^").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func rcFindingID(hash string) string {
	if len(hash) >= 16 {
		return "rc-" + hash[:16]
	}
	return "rc-" + hash
}

func rcShort(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// rcTail returns the last n bytes of s, on a rune boundary — the closing block is
// what the verdict parser reads, so it is what a failure log needs to show.
func rcTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return t
}

func rcOneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
