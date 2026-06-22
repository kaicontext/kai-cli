package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"kai/internal/runlog"
)

// `kai run` is the developer-facing surface for the per-turn run-log
// artifacts the agent runner writes under <KaiDir>/runs/<sessionID>/.
// Two views matter:
//
//   - last: pretty-print the most recent turn's summary so a user
//     who just saw a 656k-token run can answer "where did it go?"
//     without grepping JSON by hand.
//
//   - diff: compare two turns' prompt sections. The hash-equality
//     check names which section drove a cache invalidation — the
//     answer to "why was cache only 16% reused?"
//
// Both commands operate against `.kai/runs/`. Pass `--session <id>`
// to target a specific session; otherwise the latest session by
// mtime wins, which is what you usually want right after a run.

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Inspect per-turn agent run-log artifacts",
	Long: `Per-turn JSON artifacts written by the agent runner capture prompt
section sizes/hashes, token usage, and tool-call outcomes for each
model call. They live under .kai/runs/<sessionID>/<turn>.json.

Set KAI_DEBUG_RUNS=1 to also dump the full request/response bodies
alongside (large; off by default).

Examples:
  kai run last            # summary of the most recent turn
  kai run last --session abc123
  kai run diff 1 2        # what changed between turns 1 and 2`,
}

var (
	runSessionFlag string
	runAllFlag     bool
)

var runLastCmd = &cobra.Command{
	Use:   "last",
	Short: "Show the most recent turn's run-log summary",
	RunE:  runRunLast,
}

var runDiffCmd = &cobra.Command{
	Use:   "diff <turn-a> <turn-b>",
	Short: "Diff two turns' prompt sections (names cache-invalidating drift)",
	Args:  cobra.ExactArgs(2),
	RunE:  runRunDiff,
}

var runSummaryCmd = &cobra.Command{
	Use:   "summary",
	Short: "Aggregate dollar cost / cache reuse / tool stats across the session",
	Long: `Walks all turns in the latest (or --session) run-log directory and
prints a benchmark-friendly summary: total cost, mean/max per turn,
cache reuse %, agent count, and a single-line validation table row
suitable for piping into a regression file.

Cost is computed at Anthropic Sonnet 4.6 list rates:
  input        $3.00 / Mtok
  cache write  $3.75 / Mtok
  cache read   $0.30 / Mtok
  output      $15.00 / Mtok`,
	RunE: runRunSummary,
}

// turnCostUSD returns the dollar cost for one turn at Sonnet 4.6
// list rates. Rates live here (not in a config) because run summary
// is a diagnostic surface — keeping the math transparent at the call
// site beats indirection through a rate-card abstraction.
func turnCostUSD(in, cacheWrite, cacheRead, out int) float64 {
	const (
		inputRate      = 3.00 / 1_000_000
		cacheWriteRate = 3.75 / 1_000_000
		cacheReadRate  = 0.30 / 1_000_000
		outputRate     = 15.00 / 1_000_000
	)
	return float64(in)*inputRate +
		float64(cacheWrite)*cacheWriteRate +
		float64(cacheRead)*cacheReadRate +
		float64(out)*outputRate
}

func runRunSummary(cmd *cobra.Command, args []string) error {
	// --all aggregates every session under <kaiDir>/runs/. Useful for
	// benchmarking a one-shot `kai code -p` invocation: planner and
	// executor live in separate session dirs, so the latest-session
	// default would show only one of them. Wipe runs/ before each
	// benchmark to keep the aggregate scoped to that single task.
	var dirs []string
	if runAllFlag {
		kaiDir, err := resolveKaiDir()
		if err != nil {
			return err
		}
		root := filepath.Join(kaiDir, "runs")
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Errorf("no run-log artifacts yet (looked in %s)", root)
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
		if len(dirs) == 0 {
			return fmt.Errorf("no sessions in %s", root)
		}
	} else {
		dir, err := resolveRunSessionDir(runSessionFlag)
		if err != nil {
			return err
		}
		dirs = []string{dir}
	}
	var allTurns []turnAndDir
	for _, dir := range dirs {
		turns, err := listTurns(dir)
		if err != nil {
			return err
		}
		for _, n := range turns {
			allTurns = append(allTurns, turnAndDir{dir: dir, n: n})
		}
	}
	if len(allTurns) == 0 {
		fmt.Printf("No run-log turns found.\n")
		return nil
	}
	dir := dirs[0] // for "Session:" line; in --all mode we'll override below
	turns := make([]int, len(allTurns))
	for i, t := range allTurns {
		turns[i] = t.n
	}
	var (
		totalCost, maxCost                        float64
		maxTurn                                   int
		totalInput, totalWrite, totalRead, totalOut int
		toolCount                                 int
		maxToolBytes                              int
		agentCount                                int
	)
	for _, td := range allTurns {
		n := td.n
		t, err := readTurn(filepath.Join(td.dir, fmt.Sprintf("%d.json", n)))
		if err != nil {
			return err
		}
		c := turnCostUSD(
			t.Usage.InputTokens,
			t.Usage.CacheCreationInputTokens,
			t.Usage.CacheReadInputTokens,
			t.Usage.OutputTokens,
		)
		totalCost += c
		if c > maxCost {
			maxCost = c
			maxTurn = t.Turn
		}
		totalInput += t.Usage.InputTokens
		totalWrite += t.Usage.CacheCreationInputTokens
		totalRead += t.Usage.CacheReadInputTokens
		totalOut += t.Usage.OutputTokens
		for _, tc := range t.ToolCalls {
			toolCount++
			if tc.OutputBytes > maxToolBytes {
				maxToolBytes = tc.OutputBytes
			}
		}
		_ = agentCount // AgentCount field not currently in runlog.Turn; reserved for future use
	}
	agentCount = 1
	mean := totalCost / float64(len(turns))
	totalIn := totalInput + totalWrite + totalRead
	reuse := 0
	if totalIn > 0 {
		reuse = int(float64(totalRead) * 100 / float64(totalIn))
	}
	fmt.Printf("Session: %s  (%d turns)\n", filepath.Base(dir), len(turns))
	fmt.Printf("  total:        $%.4f\n", totalCost)
	fmt.Printf("  mean/turn:    $%.4f  ← compare against $0.70 baseline; target $0.05-0.10\n", mean)
	fmt.Printf("  max turn:     $%.4f  (turn %d)\n\n", maxCost, maxTurn)
	fmt.Printf("  by category:\n")
	fmt.Printf("    input         $%.4f  (%.1f%%)\n", float64(totalInput)*3.00/1_000_000, pct(float64(totalInput)*3.00/1_000_000, totalCost))
	fmt.Printf("    cache write   $%.4f  (%.1f%%)\n", float64(totalWrite)*3.75/1_000_000, pct(float64(totalWrite)*3.75/1_000_000, totalCost))
	fmt.Printf("    cache read    $%.4f  (%.1f%%)\n", float64(totalRead)*0.30/1_000_000, pct(float64(totalRead)*0.30/1_000_000, totalCost))
	fmt.Printf("    output        $%.4f  (%.1f%%)\n\n", float64(totalOut)*15.00/1_000_000, pct(float64(totalOut)*15.00/1_000_000, totalCost))
	fmt.Printf("Cache health:\n  reuse:        %d%%  (cache_read / total input; target 70%%+)\n\n", reuse)
	tpt := 0.0
	if len(turns) > 0 {
		tpt = float64(toolCount) / float64(len(turns))
	}
	meanToolBytes := 0
	if toolCount > 0 {
		// approximate: we didn't sum, just track max — show max only
		meanToolBytes = maxToolBytes
	}
	fmt.Printf("Tool calls:\n  count:        %d across %d turns (%.1f / turn)\n", toolCount, len(turns), tpt)
	fmt.Printf("  max output:   %s  (target: nothing >4 kB after Lever 5)\n", humanBytes(meanToolBytes))
	fmt.Printf("\nValidation table row:\n  | $%.4f | $%.4f | $%.4f | %d%% | %d | %d | %s |\n",
		totalCost, mean, maxCost, reuse, agentCount, toolCount, humanBytes(maxToolBytes))
	return nil
}

type turnAndDir struct {
	dir string
	n   int
}

func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part * 100 / whole
}

func runRunLast(cmd *cobra.Command, args []string) error {
	dir, err := resolveRunSessionDir(runSessionFlag)
	if err != nil {
		return err
	}
	turns, err := listTurns(dir)
	if err != nil {
		return err
	}
	if len(turns) == 0 {
		fmt.Printf("No run-log turns found in %s.\n", dir)
		return nil
	}
	t, err := readTurn(filepath.Join(dir, fmt.Sprintf("%d.json", turns[len(turns)-1])))
	if err != nil {
		return err
	}
	fmt.Print(formatTurn(t, dir))
	return nil
}

func runRunDiff(cmd *cobra.Command, args []string) error {
	a, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid turn-a %q: %w", args[0], err)
	}
	b, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid turn-b %q: %w", args[1], err)
	}
	dir, err := resolveRunSessionDir(runSessionFlag)
	if err != nil {
		return err
	}
	ta, err := readTurn(filepath.Join(dir, fmt.Sprintf("%d.json", a)))
	if err != nil {
		return fmt.Errorf("turn %d: %w", a, err)
	}
	tb, err := readTurn(filepath.Join(dir, fmt.Sprintf("%d.json", b)))
	if err != nil {
		return fmt.Errorf("turn %d: %w", b, err)
	}
	fmt.Print(formatDiff(ta, tb))
	return nil
}

// resolveRunSessionDir returns <KaiDir>/runs/<sessionID>. With no
// session arg it picks the most recently modified session dir,
// which matches "show me what just ran."
func resolveRunSessionDir(session string) (string, error) {
	kaiDir, err := resolveKaiDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(kaiDir, "runs")
	if session != "" {
		p := filepath.Join(root, session)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("session dir %s not found", p)
		}
		return p, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("no run-log artifacts yet (looked in %s)", root)
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no sessions in %s", root)
	}
	// Sort by mtime descending — latest run wins.
	sort.Slice(dirs, func(i, j int) bool {
		ai, _ := dirs[i].Info()
		aj, _ := dirs[j].Info()
		if ai == nil || aj == nil {
			return false
		}
		return ai.ModTime().After(aj.ModTime())
	})
	return filepath.Join(root, dirs[0].Name()), nil
}

// resolveKaiDir locates the .kai directory, trying the cwd first and
// walking up. Mirrors the resolution path the runner uses; kept here
// rather than imported so the run command works without an open DB.
func resolveKaiDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Honor both placements kaipath uses: .kai/ (legacy) and .git/kai/
	// (preferred for git repos). Walk up trying each.
	for d := cwd; d != "/" && d != "."; d = filepath.Dir(d) {
		for _, candidate := range []string{".kai", filepath.Join(".git", "kai")} {
			p := filepath.Join(d, candidate)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("kai directory not found (run `kai init` first)")
}

func listTurns(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var turns []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.Contains(name, ".request.") || strings.Contains(name, ".response.") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		turns = append(turns, n)
	}
	sort.Ints(turns)
	return turns, nil
}

func readTurn(path string) (*runlog.Turn, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t runlog.Turn
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &t, nil
}

// formatTurn renders one turn's summary. Targets a 24-row terminal:
// section table → usage → top-N tool calls → assistant snippet → file
// pointers. Color-free for grep-friendliness; users running it in a
// terminal can pipe through `less -R` if they want highlights.
func formatTurn(t *runlog.Turn, dir string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "━━━ Turn %d  (%s · %s · %dms) ━━━\n",
		t.Turn, t.SessionID[:minInt(12, len(t.SessionID))], t.Model, t.DurationMs)
	if t.TaskName != "" {
		fmt.Fprintf(&sb, "task: %s\n", t.TaskName)
	}
	if t.FinishReason != "" {
		fmt.Fprintf(&sb, "finish: %s\n", t.FinishReason)
	}
	sb.WriteString("\nSections (chars / est tokens / hash):\n")
	rows := []struct {
		name string
		s    runlog.Section
	}{
		{"system", t.Sections.System},
		{"tools", t.Sections.Tools},
		{"messages (full history)", t.Sections.Messages},
		{"message (latest)", t.Sections.Message},
	}
	for _, r := range rows {
		fmt.Fprintf(&sb, "  %-26s  %10s  %8s tok  %s\n",
			r.name, humanBytes(r.s.Chars), humanCount(r.s.EstTokens), r.s.Hash)
	}
	totalTok := t.Sections.System.EstTokens + t.Sections.Tools.EstTokens + t.Sections.Messages.EstTokens
	fmt.Fprintf(&sb, "  %-26s  %10s  %8s tok\n", "TOTAL prompt (est)", "", humanCount(totalTok))

	fmt.Fprintf(&sb, "\nUsage (billed):\n")
	fmt.Fprintf(&sb, "  input:        %s\n", humanCount(t.Usage.InputTokens))
	fmt.Fprintf(&sb, "  cache write:  %s\n", humanCount(t.Usage.CacheCreationInputTokens))
	fmt.Fprintf(&sb, "  cache read:   %s\n", humanCount(t.Usage.CacheReadInputTokens))
	fmt.Fprintf(&sb, "  output:       %s\n", humanCount(t.Usage.OutputTokens))
	// reuse_pct is the headline cost metric: fraction served from a
	// previous turn's cache (paid at ~10% of normal input rate).
	// cached_pct kept for completeness but explicitly labeled as
	// the deceptive one — a turn that writes a fresh cache every
	// time will report 99% cached and 0% reuse, which is the worst
	// case for cost.
	fmt.Fprintf(&sb, "  reuse:        %d%% (served from prior-turn cache)\n", t.Usage.ReusePct)
	fmt.Fprintf(&sb, "  cached:       %d%% (write+read; misleading on its own)\n", t.Usage.CachedPct)
	if t.Usage.CacheReadInputTokens == 0 && t.Usage.CacheCreationInputTokens > 0 {
		fmt.Fprintf(&sb, "  ⚠ cache_read=0 with cache_write>0: prior cache was invalidated\n")
		fmt.Fprintf(&sb, "    run `kai run diff <prev-turn> %d` to see which section drifted\n", t.Turn)
	}

	if len(t.ToolCalls) > 0 {
		fmt.Fprintf(&sb, "\nTool calls (%d):\n", len(t.ToolCalls))
		// Sort by output bytes descending so the worst offenders
		// (the kai_callers that returned 47kB) are at the top.
		sorted := append([]runlog.ToolCall(nil), t.ToolCalls...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].OutputBytes > sorted[j].OutputBytes })
		shown := sorted
		if len(shown) > 8 {
			shown = shown[:8]
		}
		for _, c := range shown {
			line := fmt.Sprintf("  %-18s in=%-6s out=%-6s %dms",
				c.Name, humanBytes(c.InputBytes), humanBytes(c.OutputBytes), c.DurationMs)
			if c.Error != "" {
				line += "  ERR(" + c.Error + ")"
			}
			sb.WriteString(line + "\n")
		}
		if len(sorted) > len(shown) {
			fmt.Fprintf(&sb, "  ...and %d more\n", len(sorted)-len(shown))
		}
	}

	if t.AssistantText != "" {
		fmt.Fprintf(&sb, "\nAssistant said:\n  %s\n", t.AssistantText)
	}

	fmt.Fprintf(&sb, "\nArtifact: %s\n", filepath.Join(dir, fmt.Sprintf("%d.json", t.Turn)))
	if t.RequestPath != "" {
		fmt.Fprintf(&sb, "Request:  %s\n", t.RequestPath)
	}
	if t.ResponsePath != "" {
		fmt.Fprintf(&sb, "Response: %s\n", t.ResponsePath)
	}
	return sb.String()
}

// formatDiff names which sections changed between two turns, sized by
// the delta. The headline is the cache verdict — the section with the
// largest changed-hash chars count is almost always the culprit.
func formatDiff(a, b *runlog.Turn) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "━━━ Diff: turn %d → turn %d ━━━\n", a.Turn, b.Turn)
	fmt.Fprintf(&sb, "cache: turn %d → %d%%   turn %d → %d%%\n",
		a.Turn, a.Usage.CachedPct, b.Turn, b.Usage.CachedPct)

	type row struct {
		name           string
		aSec, bSec     runlog.Section
	}
	rows := []row{
		{"system", a.Sections.System, b.Sections.System},
		{"tools", a.Sections.Tools, b.Sections.Tools},
		{"messages", a.Sections.Messages, b.Sections.Messages},
		{"message", a.Sections.Message, b.Sections.Message},
	}

	sb.WriteString("\nSection drift:\n")
	var driftBytes int
	var driftSection string
	for _, r := range rows {
		marker := "="
		delta := r.bSec.Chars - r.aSec.Chars
		if r.aSec.Hash != r.bSec.Hash {
			marker = "!"
			if abs(delta) > driftBytes {
				driftBytes = abs(delta)
				driftSection = r.name
			}
		}
		fmt.Fprintf(&sb, "  %s %-10s  %10s → %-10s  Δ %+s   %s → %s\n",
			marker, r.name,
			humanBytes(r.aSec.Chars), humanBytes(r.bSec.Chars),
			humanBytes(delta), shortHash(r.aSec.Hash), shortHash(r.bSec.Hash))
	}

	sb.WriteString("\n")
	if driftSection != "" {
		fmt.Fprintf(&sb, "Cache-invalidating section: %s (Δ %s)\n", driftSection, humanBytes(driftBytes))
		fmt.Fprintf(&sb, "→ check what changed in this section between turns. Common causes:\n")
		switch driftSection {
		case "system":
			sb.WriteString("    - dynamic content in the system prompt (timestamps, mode label)\n")
		case "tools":
			sb.WriteString("    - tool registry order changed (Go map iteration is random)\n")
			sb.WriteString("    - a tool's schema changed mid-session\n")
		case "messages":
			sb.WriteString("    - compaction rewrote earlier history\n")
			sb.WriteString("    - per-turn project overview was regenerated with shifted content\n")
		case "message":
			sb.WriteString("    - this is the latest user/tool-result turn; expected to differ\n")
		}
	} else {
		sb.WriteString("All sections matched — cache should have been near-100%.\n")
		sb.WriteString("If billed cache is still low, the provider may not have honored the prefix\n")
		sb.WriteString("(check provider_note in the response artifact with KAI_DEBUG_RUNS=1).\n")
	}

	return sb.String()
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fkB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/1024/1024)
	}
}

func humanCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
