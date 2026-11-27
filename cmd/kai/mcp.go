package main

// mcp.go — `kai mcp serve`: Kai's local review and graph queries as MCP
// tools, for a coding agent (Claude Code, Cursor, Codex) to call before its
// user pushes.
//
// The MCP connector this replaces was removed in June 2026; it carried live
// sync, file locks and a watcher along with a dozen graph tools, and left it
// to the agent to decide when any of them was worth calling. This one is
// built around a single question the agent's user is about to ask anyway —
// "is this change ready to push?" — and answers it with Kai's own review:
//
//   - kai_review runs `kai review-commit --local` on the working tree: the
//     review pipeline, on the user's own Anthropic API key (review_local.go).
//     The agent gets the findings back and can fix them before the pull
//     request exists.
//   - The graph tools (kai_callers, kai_impact, kai_tests, …) are the engine's
//     own, registered as they are, so the agent can dig into a finding. They
//     need a captured graph and are simply absent without one.
//
// Nothing here calls a model on Kai's account or sends the code or the review
// to Kai's server.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Kai as an MCP server for coding agents",
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve Kai's local review and graph queries over MCP (stdio)",
	Long: "Runs an MCP server on stdin/stdout for a coding agent to call.\n\n" +
		"  claude mcp add kai -- kai mcp serve\n\n" +
		"Tools:\n" +
		"  kai_review   Kai's review of your unpushed and uncommitted changes, run on your own\n" +
		"               Anthropic API key (set ANTHROPIC_API_KEY). Fast by default; a deep review\n" +
		"               returns its estimated cost first and runs only once that is confirmed.\n" +
		"  kai_callers, kai_callees, kai_dependents, kai_dependencies, kai_context, kai_impact,\n" +
		"  kai_tests    Queries on the captured graph (run `kai capture` first).\n\n" +
		"The review of record is still the one Kai runs on the pull request.",
	Args: cobra.NoArgs,
	RunE: runMCPServe,
}

func init() {
	mcpCmd.AddCommand(mcpServeCmd)
	rootCmd.AddCommand(mcpCmd)
}

// mcpGraphTools are the engine tools served: read-only queries on the graph.
// The engine's other kai_* tools act on a session, a workspace or the user's
// accounts, and belong to Kai's own agent.
var mcpGraphTools = map[string]bool{
	"kai_callers": true, "kai_callees": true, "kai_dependents": true, "kai_dependencies": true,
	"kai_context": true, "kai_impact": true, "kai_tests": true,
}

const mcpInstructions = "Kai reviews code changes. Before the user pushes or opens a pull request, call kai_review to run " +
	"Kai's review on their unpushed and uncommitted changes and fix what it finds. The review runs on the user's own " +
	"Anthropic API key and costs them money: the fast review is a few cents; a deep review can be a few dollars, so " +
	"kai_review returns an estimate for it first and you must show the user that figure and get their yes before " +
	"confirming. Kai's review on the pull request is still the check of record; this is the run before it. Use the " +
	"graph tools (kai_callers, kai_impact, kai_tests and the rest) to check a finding against the code that calls or " +
	"depends on the change."

func runMCPServe(cmd *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	srv := server.NewMCPServer("kai", Version,
		server.WithToolCapabilities(false),
		server.WithInstructions(mcpInstructions))

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the kai binary: %w", err)
	}
	srv.AddTool(mcpReviewTool(), mcpReviewHandler(mcpExecReview(self, cwd)))

	// stdout is the protocol; anything for a person goes to stderr.
	set, outcome := projects.Discover(cwd)
	if outcome == projects.OutcomeRootsFound {
		if err := set.Open(); err != nil {
			fmt.Fprintf(os.Stderr, "kai mcp: graph tools unavailable: %v\n", err)
		} else {
			defer set.Close()
			n := mcpAddGraphTools(srv, (&tools.KaiTools{Set: set}).All())
			fmt.Fprintf(os.Stderr, "kai mcp: serving kai_review and %d graph tool(s)\n", n)
		}
	} else {
		fmt.Fprintln(os.Stderr, "kai mcp: serving kai_review; no captured graph here, so no graph tools (run `kai capture`)")
	}
	return server.ServeStdio(srv)
}

// mcpAddGraphTools registers each served engine tool under its own name,
// description and parameter schema, and returns how many there were.
func mcpAddGraphTools(srv *server.MCPServer, all []tools.BaseTool) int {
	n := 0
	for _, t := range all {
		info := t.Info()
		if !mcpGraphTools[info.Name] {
			continue
		}
		schema, err := json.Marshal(map[string]any{
			"type":       "object",
			"properties": mcpProperties(info.Parameters),
			"required":   mcpRequired(info.Required),
		})
		if err != nil {
			continue
		}
		tool := mcp.NewToolWithRawSchema(info.Name, info.Description, schema)
		srv.AddTool(tool, mcpEngineHandler(t))
		n++
	}
	return n
}

func mcpProperties(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

func mcpRequired(r []string) []string {
	if r == nil {
		return []string{}
	}
	return r
}

// mcpEngineHandler runs one engine tool with the arguments the agent sent.
func mcpEngineHandler(t tools.BaseTool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		if args == nil {
			args = map[string]any{}
		}
		input, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError("arguments are not valid JSON: " + err.Error()), nil
		}
		resp, err := t.Run(ctx, tools.ToolCall{ID: "mcp", Name: t.Info().Name, Input: string(input)})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if resp.IsError {
			return mcp.NewToolResultError(resp.Content), nil
		}
		return mcp.NewToolResultText(resp.Content), nil
	}
}

func mcpReviewTool() mcp.Tool {
	return mcp.NewTool("kai_review",
		mcp.WithDescription("Run Kai's code review on the user's local changes before they push: this branch's unpushed commits "+
			"plus uncommitted work, compared against the default branch. Returns the review and its findings (file:line, "+
			"what is wrong). Runs on the user's own Anthropic API key (ANTHROPIC_API_KEY) and costs them money. "+
			"depth \"fast\" (default) is one pass over the diff for a few cents. depth \"deep\" is the grounded review "+
			"that reads callers and dependents; it needs `kai capture` to have been run and can cost a few dollars, so the "+
			"first deep call returns only an estimate: show it to the user, and call again with confirmed_cost_usd set "+
			"to that estimate once they agree."),
		mcp.WithString("depth", mcp.Description("\"fast\" (default) or \"deep\"."), mcp.Enum("fast", "deep")),
		mcp.WithString("base", mcp.Description("Branch or commit to compare against. Default: the remote default branch (origin/main).")),
		mcp.WithString("commit", mcp.Description("Review this commit instead of the working tree.")),
		mcp.WithNumber("max_cost_usd", mcp.Description("Stop the review once it has spent this many US dollars. Default 0.50 fast, 5.00 deep.")),
		mcp.WithNumber("confirmed_cost_usd", mcp.Description("For depth \"deep\" only: the estimate the user agreed to, from the previous kai_review call.")),
	)
}

// mcpReviewArgs is a kai_review call.
type mcpReviewArgs struct {
	Deep         bool
	Base, Commit string
	MaxCostUSD   float64
	ConfirmedUSD float64
}

// mcpReviewRunner runs `kai review-commit --local --format json` with extra
// flags and returns its stdout, its stderr and its error.
type mcpReviewRunner func(ctx context.Context, flags []string) (stdout, stderr string, err error)

// mcpExecReview runs the review as a child `kai`, in dir. A child, not a
// function call: a review sets process-wide state (its profile, its run
// record) that a server answering many calls must not carry between them.
func mcpExecReview(self, dir string) mcpReviewRunner {
	return func(ctx context.Context, flags []string) (string, string, error) {
		args := append([]string{"review-commit", "--local", "--format", "json"}, flags...)
		var out, errb bytes.Buffer
		c := mcpChild(ctx, self, args, dir)
		c.Stdout, c.Stderr = &out, &errb
		err := c.Run()
		return out.String(), errb.String(), err
	}
}

// mcpChildWaitDelay is how long a cancelled child's pipes are waited on.
var mcpChildWaitDelay = 5 * time.Second

// mcpChild is the command a tool call runs. After a cancel the child is
// killed, but Run still waits for its output pipes to close, and a grandchild
// can hold them open; WaitDelay bounds that wait.
func mcpChild(ctx context.Context, bin string, args []string, dir string) *exec.Cmd {
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = dir
	c.Stdin = nil
	c.WaitDelay = mcpChildWaitDelay
	return c
}

func mcpReviewFlags(a mcpReviewArgs) []string {
	var f []string
	if a.Deep {
		f = append(f, "--deep")
	}
	if a.Base != "" {
		f = append(f, "--base", a.Base)
	}
	if a.MaxCostUSD > 0 {
		f = append(f, "--max-cost", fmt.Sprintf("%.2f", a.MaxCostUSD))
	}
	return f
}

func mcpReviewHandler(run mcpReviewRunner) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		a := mcpReviewArgs{
			Deep:         strings.EqualFold(strings.TrimSpace(req.GetString("depth", "fast")), "deep"),
			Base:         strings.TrimSpace(req.GetString("base", "")),
			Commit:       strings.TrimSpace(req.GetString("commit", "")),
			MaxCostUSD:   req.GetFloat("max_cost_usd", 0),
			ConfirmedUSD: req.GetFloat("confirmed_cost_usd", 0),
		}
		if d := strings.ToLower(strings.TrimSpace(req.GetString("depth", "fast"))); d != "fast" && d != "deep" {
			return mcp.NewToolResultError(fmt.Sprintf("depth %q is not fast or deep", d)), nil
		}
		// A value that begins with "-" would be read as a flag by the child.
		for _, v := range []string{a.Base, a.Commit} {
			if strings.HasPrefix(v, "-") {
				return mcp.NewToolResultError(fmt.Sprintf("%q is not a branch or commit", v)), nil
			}
		}
		text, isErr := mcpReview(ctx, run, a)
		if isErr {
			return mcp.NewToolResultError(text), nil
		}
		return mcp.NewToolResultText(text), nil
	}
}

// mcpDeepTimeout bounds a deep review; the fast pass has its own budget.
const mcpDeepTimeout = 20 * time.Minute

// mcpReview runs one kai_review call and returns what the agent reads.
func mcpReview(ctx context.Context, run mcpReviewRunner, a mcpReviewArgs) (text string, isErr bool) {
	flags := mcpReviewFlags(a)
	tail := []string{}
	if a.Commit != "" {
		tail = []string{"--", a.Commit}
	}
	if a.Deep {
		// The estimate first, every time: the figure the user agrees to is
		// the one for the change as it is now.
		out, errOut, err := run(ctx, append(append(append([]string{}, flags...), "--estimate"), tail...))
		if err != nil {
			return "Kai could not estimate the deep review.\n" + mcpLastLines(errOut, 6), true
		}
		var est rcLocalEstimateReport
		if err := json.Unmarshal([]byte(out), &est); err != nil {
			return "Kai could not read its own estimate: " + err.Error(), true
		}
		if a.ConfirmedUSD <= 0 || a.ConfirmedUSD+0.005 < est.EstimateUSD {
			return fmt.Sprintf("A deep Kai review of this change (%d file(s)) is estimated at about $%.2f on the user's Anthropic API key, "+
				"on %s, and would stop at $%.2f.\n\nNothing has been run. Show the user this figure. If they agree, call kai_review "+
				"again with depth \"deep\" and confirmed_cost_usd %.2f. The fast review (depth \"fast\") costs a few cents and needs no confirmation.",
				est.ChangedFiles, est.EstimateUSD, strings.Join(est.Models, ", "), est.CapUSD, est.EstimateUSD), false
		}
		flags = append(flags, "--yes")
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpDeepTimeout)
		defer cancel()
	}
	out, errOut, err := run(ctx, append(flags, tail...))
	if strings.TrimSpace(out) == "" {
		msg := "Kai's local review did not run."
		if err == nil {
			msg = "Kai's local review returned nothing."
		}
		return msg + "\n" + mcpLastLines(errOut, 8), true
	}
	return mcpRenderReview(out, errOut, err != nil)
}

// mcpBundle is the part of the review bundle kai_review reports.
type mcpBundle struct {
	Review     string `json:"review"`
	Depth      string `json:"depth"`
	Incomplete bool   `json:"incomplete"`
	Readiness  int    `json:"readiness"`
	Intent     struct {
		Note  string   `json:"note"`
		Risks []string `json:"risks"`
	} `json:"intent"`
	Local *rcLocalReport `json:"local"`
}

// mcpRenderReview turns a review bundle into the text the agent reads: what
// the review says, each finding on its own line, and what it cost.
func mcpRenderReview(stdout, stderr string, failed bool) (string, bool) {
	var b mcpBundle
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		return "Kai's local review produced output that could not be read: " + err.Error() + "\n" + mcpLastLines(stderr, 6), true
	}
	var w strings.Builder
	depth := b.Depth
	if depth == "" {
		depth = "fast"
	}
	fmt.Fprintf(&w, "Kai local review (%s)", depth)
	if b.Local != nil && b.Local.Base != "" {
		fmt.Fprintf(&w, " against %s", b.Local.Base)
	}
	w.WriteString("\n\n")
	if b.Incomplete {
		w.WriteString("This review did not finish. What follows is how far it got, not a verdict.\n\n")
	}
	if r := strings.TrimSpace(b.Review); r != "" {
		w.WriteString(r)
		w.WriteString("\n\n")
	}
	if len(b.Intent.Risks) == 0 {
		if !b.Incomplete {
			w.WriteString("Findings: none.\n")
			if depth == "fast" {
				w.WriteString("A fast review reads only the diff, so this is not an all-clear: it did not check callers.\n")
			}
		}
	} else {
		fmt.Fprintf(&w, "Findings (%d):\n", len(b.Intent.Risks))
		for i, r := range b.Intent.Risks {
			fmt.Fprintf(&w, "%d. %s\n", i+1, strings.TrimSpace(r))
		}
	}
	if b.Local != nil {
		fmt.Fprintf(&w, "\nSpent about $%.3f of the user's Anthropic credit over %d call(s) on %s (list prices; cap $%.2f).\n",
			b.Local.SpentUSD, b.Local.Calls, strings.Join(b.Local.Models, ", "), b.Local.CapUSD)
	}
	w.WriteString("Kai's review on the pull request is still the check of record.\n")
	return w.String(), failed && b.Incomplete && strings.TrimSpace(b.Review) == ""
}

// mcpLastLines is the end of a child's stderr with the update notice and
// blank lines dropped: the part that says why it stopped.
func mcpLastLines(s string, n int) string {
	var keep []string
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "Update available:") {
			continue
		}
		keep = append(keep, t)
	}
	// cobra prints an error twice ("Error: x" then "x").
	if l := len(keep); l >= 2 && keep[l-2] == "Error: "+keep[l-1] {
		keep = keep[:l-1]
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}
