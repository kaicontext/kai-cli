package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/ai"
	"github.com/kaicontext/kai-engine/contract"
	semanticdiff "github.com/kaicontext/kai-engine/diff"
	"github.com/kaicontext/kai-engine/finding"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/review"
	"github.com/kaicontext/kai-engine/safetygate"
	"github.com/kaicontext/kai-engine/shouldtouch"
	"github.com/kaicontext/kai-engine/util"
)

var (
	analyzeFormat   string
	analyzeContract string
)

var reviewAnalyzeCmd = &cobra.Command{
	Use:   "analyze <review-id>",
	Short: "Emit the structured Finding document for a review (headless)",
	Long: `Assemble the review Finding — verdict, intent-vs-code, blast radius, and diff —
as a single JSON document. This is the headless surface the TUI and the server
web UI both render; it serializes data the engine already computed.

The intent-vs-code panel is enriched from a verification contract when one can be
resolved (a single open contract, or --contract <id>). Grounded claims and
should-touch are emitted empty until the graph-grounded-evidence layer lands.

Examples:
  kai review analyze kai-204 --format json
  kai review analyze kai-204 --format json --contract rotate-session-keys`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewAnalyze,
}

func runReviewAnalyze(cmd *cobra.Command, args []string) error {
	if analyzeFormat != "json" {
		return fmt.Errorf("unsupported --format %q (only \"json\" is supported)", analyzeFormat)
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	mgr := review.NewManager(db)
	rev, err := mgr.GetByShortID(args[0])
	if err != nil {
		return err
	}
	if rev.TargetKind != graph.KindChangeSet {
		return fmt.Errorf("review %s targets a %s; only changeset reviews can be analyzed", args[0], rev.TargetKind)
	}

	csNode, err := db.GetNode(rev.TargetID)
	if err != nil {
		return fmt.Errorf("loading changeset: %w", err)
	}
	if csNode == nil {
		return fmt.Errorf("changeset %s not found", util.BytesToHex(rev.TargetID)[:12])
	}

	baseHex, _ := csNode.Payload["base"].(string)
	headHex, _ := csNode.Payload["head"].(string)

	f := finding.Finding{
		ID:        review.IDToHex(rev.ID)[:12],
		Title:     rev.Title,
		Author:    rev.Author,
		From:      shortHex(baseHex),
		To:        shortHex(headHex),
		CreatedAt: rev.CreatedAt,
		Verdict:   verdictFromReviewState(rev.State),
		Claims:    []finding.Claim{},
	}

	// --- Diff panel + line totals -------------------------------------------
	sd, err := semanticdiff.FromChangeSet(db, csNode)
	if err != nil {
		return fmt.Errorf("building semantic diff: %w", err)
	}
	var changedPaths []string
	for _, fd := range sd.Files {
		before := readObjectString(db, fd.BeforeHash)
		after := readObjectString(db, fd.AfterHash)
		added, removed, patch := unifiedDiff(before, after)
		f.Diff.Files = append(f.Diff.Files, finding.DiffFile{
			Path:    fd.Path,
			Action:  string(fd.Action),
			Added:   added,
			Removed: removed,
			Patch:   patch,
		})
		f.Added += added
		f.Removed += removed
		if fd.Action != semanticdiff.ActionRemoved {
			changedPaths = append(changedPaths, fd.Path)
		}
	}
	f.Files = len(sd.Files)

	var headSnapID []byte
	if id, err := util.HexToBytes(headHex); err == nil {
		headSnapID = id
	}
	changedFns := symbolsInFiles(db, headFileNodeIDs(db, headSnapID, changedPaths))

	// --- Grounded claims panel ----------------------------------------------
	f.Claims = buildClaims(db, changedFns)

	// --- Intent-vs-code panel -----------------------------------------------
	f.Intent = buildIntent(csNode, rev)

	// --- Blast radius panel (transitive, hop-annotated) ---------------------
	blast, err := buildBlast(cmd, db, changedPaths, headHex)
	if err != nil {
		return err
	}
	f.Blast = blast
	f.Blast.ShouldTouch = buildShouldTouch(db, headSnapID, changedFns)

	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling finding: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

// buildIntent fills the Intent panel: the stated intent from the changeset,
// enriched with the match verdict / note / risks from a verification contract
// when one resolves.
func buildIntent(csNode *graph.Node, rev *review.Review) finding.Intent {
	stated, _ := csNode.Payload["intent"].(string)
	if strings.TrimSpace(stated) == "" {
		stated = rev.Title
	}
	intent := finding.Intent{Stated: stated, Match: finding.MatchUnknown}

	c := resolveContract()
	if c == nil {
		return intent
	}
	intent.Match = matchFromContract(c.Status)
	intent.Note = c.Semantic.Note
	for _, r := range c.Residue {
		if strings.TrimSpace(r.Prompt) != "" {
			intent.Risks = append(intent.Risks, r.Prompt)
		}
	}
	return intent
}

// resolveContract loads the contract named by --contract, or the single open
// contract when exactly one exists. Returns nil (match=unknown) otherwise —
// Phase 1 has no changeset<->contract link, so we never guess between several.
func resolveContract() *contract.Contract {
	store, err := contract.Open(kaiDir)
	if err != nil {
		return nil
	}
	defer store.Close()

	if analyzeContract != "" {
		c, err := store.Get(analyzeContract)
		if err != nil {
			return nil
		}
		return c
	}
	all, err := store.List()
	if err != nil {
		return nil
	}
	var open []*contract.Contract
	for _, c := range all {
		if !c.Closed {
			open = append(open, c)
		}
	}
	if len(open) == 1 {
		return open[0]
	}
	return nil
}

// blastMaxDepth bounds the transitive walk. The mockup surfaces up to ~3 hops;
// a small ceiling keeps the reach a readable "what the change touches" list
// rather than the whole downstream cone.
const blastMaxDepth = 5

// buildBlast computes the transitive, hop-annotated blast radius by walking
// callers/importers outward from the changed files. Depth-1 nodes are Direct,
// deeper ones Transitive. Should-touch detection arrives in task #6.
func buildBlast(cmd *cobra.Command, db *graph.DB, changedPaths []string, headHex string) (finding.Blast, error) {
	blast := finding.Blast{Nodes: []finding.BlastNode{}, ShouldTouch: []finding.ShouldTouch{}}
	if len(changedPaths) == 0 {
		return blast, nil
	}

	var snapID []byte
	if headID, err := util.HexToBytes(headHex); err == nil {
		snapID = headID
	}

	reached, err := safetygate.Reachback(cmd.Context(), db, changedPaths, snapID, blastMaxDepth)
	if err != nil {
		return blast, fmt.Errorf("computing blast radius: %w", err)
	}

	for _, r := range reached {
		category := finding.CategoryTransitive
		if r.Hops == 1 {
			category = finding.CategoryDirect
		}
		blast.Nodes = append(blast.Nodes, finding.BlastNode{
			Symbol:   r.Symbol,
			File:     r.Path,
			Category: category,
			Hops:     r.Hops,
		})
	}
	blast.Reaches = len(blast.Nodes)
	return blast, nil
}

// buildClaims produces grounded claims: for each changed function/method, a
// callers() lookup resolved against the call graph. Every claim carries the
// literal lookup and its concrete result, so the UI never asserts anything that
// isn't backed by a real query. reaches()/negative-existential claims that
// compare against sibling paths arrive with should-touch (task #6).
func buildClaims(db *graph.DB, changedSymbols []string) []finding.Claim {
	claims := []finding.Claim{}
	for _, sym := range changedSymbols {
		callers := resolveCallers(db, sym)
		var lookup, statement string
		if len(callers) == 0 {
			lookup = fmt.Sprintf("callers(%s) -> ø", sym)
			statement = fmt.Sprintf("%s has no callers in the graph.", sym)
		} else {
			lookup = fmt.Sprintf("callers(%s) -> [%s] (%d edge, resolved)", sym, strings.Join(callers, ", "), len(callers))
			statement = fmt.Sprintf("%s is reached by %s.", sym, countCallers(callers))
		}
		claims = append(claims, finding.Claim{
			Statement: statement,
			Lookup:    lookup,
			Resolved:  true, // the lookup ran; ø is a resolved (empty) result, not a failure
			Tag:       finding.TagInfo,
			Verified:  true,
		})
	}
	return claims
}

// headFileNodeIDs returns the hex File-node ids for the changed paths as they
// exist in the head snapshot (a path has a distinct File node per revision, so
// we scope to the head snapshot's HAS_FILE edges to get the right one).
func headFileNodeIDs(db *graph.DB, headSnapID []byte, changedPaths []string) map[string]bool {
	out := map[string]bool{}
	if len(headSnapID) == 0 || len(changedPaths) == 0 {
		return out
	}
	want := make(map[string]bool, len(changedPaths))
	for _, p := range changedPaths {
		want[p] = true
	}
	edges, err := db.GetEdges(headSnapID, graph.EdgeHasFile)
	if err != nil {
		return out
	}
	for _, e := range edges {
		fn, err := db.GetNode(e.Dst)
		if err != nil || fn == nil {
			continue
		}
		if path, _ := fn.Payload["path"].(string); want[path] {
			out[strings.ToLower(util.BytesToHex(fn.ID))] = true
		}
	}
	return out
}

// symbolsInFiles returns the fqNames of function/method symbols defined in the
// given File nodes (Symbol.fileId == a head File-node id). This is the changed
// files' API surface; callers() of these is the grounded blast the reviewer
// cares about.
func symbolsInFiles(db *graph.DB, fileIDs map[string]bool) []string {
	if len(fileIDs) == 0 {
		return nil
	}
	symbols, err := db.GetNodesByKind(graph.KindSymbol)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range symbols {
		fileID, _ := n.Payload["fileId"].(string)
		if !fileIDs[strings.ToLower(fileID)] {
			continue
		}
		if kind, _ := n.Payload["kind"].(string); kind != "function" && kind != "method" {
			continue
		}
		fq, _ := n.Payload["fqName"].(string)
		if fq == "" || seen[fq] {
			continue
		}
		seen[fq] = true
		out = append(out, fq)
	}
	sort.Strings(out)
	return out
}

// resolveCallers returns the deduplicated "file:line" call sites that call the
// named symbol, read from the call-site nodes behind each CALLS edge.
func resolveCallers(db *graph.DB, calleeName string) []string {
	edges, err := db.GetEdgesByCalleeName(calleeName, graph.EdgeCalls)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		if e.At == nil {
			continue
		}
		node, err := db.GetNode(e.At)
		if err != nil || node == nil {
			continue
		}
		callerFile, _ := node.Payload["callerFile"].(string)
		if callerFile == "" {
			continue
		}
		site := callerFile
		if line, ok := node.Payload["line"].(float64); ok && line > 0 {
			site = fmt.Sprintf("%s:%d", callerFile, int(line))
		}
		if seen[site] {
			continue
		}
		seen[site] = true
		out = append(out, site)
	}
	sort.Strings(out)
	return out
}

func countCallers(callers []string) string {
	if len(callers) == 1 {
		return "exactly one caller, " + callers[0]
	}
	return fmt.Sprintf("%d callers: %s", len(callers), strings.Join(callers, ", "))
}

// buildShouldTouch finds nodes the change pattern implied but didn't modify.
// The graph produces high-recall candidates (peers of a changed function that
// share a common callee it lacks); an LLM judge confirms each is a genuine
// omission and phrases the "why". Without an API key the judge can't run, so we
// emit nothing rather than assert an unverified candidate.
func buildShouldTouch(db *graph.DB, headSnapID []byte, changedFns []string) []finding.ShouldTouch {
	out := []finding.ShouldTouch{}
	cands, err := shouldtouch.Detect(db, headSnapID, changedFns)
	if err != nil || len(cands) == 0 {
		return out
	}
	if !ai.IsConfigured() {
		return out
	}
	client, err := ai.NewClient()
	if err != nil {
		return out
	}
	for _, c := range cands {
		ok, why := judgeShouldTouch(client, c)
		if !ok {
			continue
		}
		out = append(out, finding.ShouldTouch{
			Symbol: c.Callee,
			File:   c.TargetFile,
			Hops:   1,
			Why:    why,
		})
	}
	return out
}

const shouldTouchSystem = `You verify whether an agent's code change is missing a call it almost certainly should have made. You are given a changed function, the peer functions that do the same kind of work, and a callee that every peer invokes but the changed function does not. A wrong "yes" wastes a reviewer's time, so answer "yes" ONLY when the omission is a real, likely-unintended gap. When unsure, answer no.

Respond in exactly one line:
YES: <one sentence explaining what the change fails to do>
or
NO`

func judgeShouldTouch(client *ai.Client, c shouldtouch.Candidate) (bool, string) {
	user := fmt.Sprintf(
		"Changed function: %s\nPeer functions that all call %s: %s\nThe changed function does NOT call %s.\n\nIs this a genuine omission the change should have included?",
		c.ChangedFn, c.Callee, strings.Join(c.Peers, ", "), c.Callee)
	resp, err := client.Complete(shouldTouchSystem, []ai.Message{{Role: "user", Content: user}}, 150)
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(resp)
	if !strings.HasPrefix(strings.ToLower(line), "yes") {
		return false, ""
	}
	why := strings.TrimSpace(strings.TrimPrefix(line[3:], ":"))
	if why == "" {
		why = fmt.Sprintf("Every peer of %s calls %s; this change doesn't.", c.ChangedFn, c.Callee)
	}
	return true, why
}

func verdictFromReviewState(s review.State) finding.Verdict {
	switch s {
	case review.StateApproved, review.StateMerged:
		return finding.VerdictConfirmed
	case review.StateChanges, review.StateAbandoned:
		return finding.VerdictRejected
	default: // draft, open
		return finding.VerdictAwaiting
	}
}

func matchFromContract(v contract.Verdict) finding.Match {
	switch v {
	case contract.VerdictVerified:
		return finding.MatchVerified
	case contract.VerdictBroken, contract.VerdictDrifting:
		return finding.MatchDiverges
	case contract.VerdictCleanUnconfirmed:
		return finding.MatchPartial
	default: // no_intent or anything unrecognized
		return finding.MatchUnknown
	}
}

func readObjectString(db *graph.DB, hash string) string {
	if hash == "" {
		return ""
	}
	if b, err := db.ReadObject(hash); err == nil {
		return string(b)
	}
	return ""
}

func shortHex(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// unifiedDiff returns added/removed line counts and an uncolored unified patch
// between two file versions, using the same line-mode primitive as `kai diff`.
func unifiedDiff(before, after string) (added, removed int, patch string) {
	if before == after {
		return 0, 0, ""
	}
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(before, after)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)
	diffs = dmp.DiffCleanupSemantic(diffs)

	var sb strings.Builder
	for _, d := range diffs {
		body := strings.TrimSuffix(d.Text, "\n")
		if d.Text == "" {
			continue
		}
		segLines := strings.Split(body, "\n")
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			for _, ln := range segLines {
				added++
				sb.WriteString("+" + ln + "\n")
			}
		case diffmatchpatch.DiffDelete:
			for _, ln := range segLines {
				removed++
				sb.WriteString("-" + ln + "\n")
			}
		default: // equal: context
			for _, ln := range segLines {
				sb.WriteString(" " + ln + "\n")
			}
		}
	}
	return added, removed, sb.String()
}
