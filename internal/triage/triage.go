// Package triage classifies an incoming user request into one of four
// handling tracks before any planning happens. It runs a single LLM
// call on a strong model: the routing decision is small but high
// leverage — a mis-route either sends a big change down a path with no
// review, or buries a one-line fix under a full plan-confirm cycle.
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Track is the handling path triage selects for a request.
type Track string

const (
	// TrackAnswer: a pure question, no code change. Result.Answer
	// holds the reply; nothing is executed.
	TrackAnswer Track = "answer"
	// TrackQuick: a trivial, reversible change. Runs as a single
	// agent with no plan-confirm step.
	TrackQuick Track = "quick"
	// TrackPlan: a non-trivial, multi-step, or risky change. Handed
	// to the full planner -> plan-confirm -> orchestrate flow. Also
	// the fallback for any triage failure.
	TrackPlan Track = "plan"
	// TrackClarify: too ambiguous to route. Result.Question holds
	// what to ask the user.
	TrackClarify Track = "clarify"
	// TrackHost: the request is a host-shell task (install something
	// onto PATH, sudo, brew install, register an MCP server, edit a
	// shell rc, log in). Cannot be done from inside a CoW spawn
	// workspace — writes to the user's real filesystem fail or land
	// in the wrong place. Result.Answer carries a human-readable
	// explanation plus the literal command the user should run on
	// their own shell. Nothing is spawned or planned.
	TrackHost Track = "host"
)

// Result is triage's decision. The JSON tags match the object the
// triage model is asked to emit, so a response parses straight in.
type Result struct {
	Track    Track  `json:"track"`
	Reason   string `json:"reason"`
	Question string `json:"question,omitempty"`
	Answer   string `json:"answer,omitempty"`
	// HostCommand is the literal shell command kai should run on
	// the user's host (after they approve) when Track is TrackHost
	// AND the classifier narrowed down a specific command. Empty
	// when we know it's a host task but don't know what to run —
	// the TUI then shows Answer as prose and asks the user to run
	// their own equivalent. Set by the heuristic for the kai-
	// specific cases (install -> `cd kai-cli && make install`).
	HostCommand string `json:"host_command,omitempty"`
}

// Request is the input to Classify.
type Request struct {
	// UserRequest is the raw text the user submitted.
	UserRequest string
	// ForcedMode is a /slash mode override ("plan", "chat", "debug")
	// or "" when none. A forced /plan short-circuits triage entirely.
	ForcedMode string
	// Projects is a thin repo signal — project names (optionally with
	// languages) — never file contents. Keeps the prompt lean.
	Projects []string
	// RecentTurns is the last turn or two of conversation, oldest
	// first, for context on follow-up requests.
	RecentTurns []string
	// ProjectHints is a list of one-line manifest summaries from the
	// workspace root — e.g.:
	//   "package.json: scripts dev, build, start"
	//   "Cargo.toml: bin kai-cli"
	//   "go.mod: module kai"
	//   "Makefile: targets install, test, build"
	// Used by the triage prompt to disambiguate intent verbs like
	// "run it" / "start it" / "build it" / "test it" — without these
	// hints the LLM has no way to know "run it" in a kai-desktop
	// directory means `npm run dev` rather than asking what to do.
	// Producers (TUI runPlan) scan known manifest files in the cwd
	// and pass at most a few; the prompt is kept lean.
	ProjectHints []string
}

// Sender is the minimal LLM interface Classify needs. Defined here
// rather than imported so the package stays dependency-free and
// trivially stubbable in tests — the same cycle-avoidance pattern
// tools.Sender uses. An adapter at the call site bridges to the real
// provider.
type Sender interface {
	Send(ctx context.Context, req SenderRequest) (SenderResponse, error)
}

// SenderRequest / SenderResponse mirror the slim text-in/text-out
// subset of provider.Request / provider.Response.
type SenderRequest struct {
	Model     string
	System    string
	UserText  string
	MaxTokens int
}

type SenderResponse struct {
	Text string
}

// triageMaxTokens caps the triage completion. The reply is a small
// JSON object (plus, for the answer track, a concise answer), so this
// is generous headroom, not a target.
const triageMaxTokens = 700

// Classify routes req into a Track via at most one LLM call on model.
//
// A forced /plan short-circuits without a call. Any failure — a
// transport error or an unparseable response — resolves to TrackPlan:
// the conservative choice, since plan is the only track that still
// puts a review step in front of the user. On a transport error the
// error is also returned so the caller can log it; the Result is
// still safe to use.
func Classify(ctx context.Context, s Sender, model string, req Request) (Result, error) {
	if strings.EqualFold(req.ForcedMode, "plan") {
		return Result{Track: TrackPlan, Reason: "forced by /plan"}, nil
	}
	// All routing — including TrackHost — flows through the LLM.
	// We previously had a regex pre-filter for the obvious install /
	// PATH / sudo / MCP cases; it was brittle in the same way the
	// rename gate (deleted in 0.30.27) was: every phrasing we didn't
	// anticipate slipped through, and the maintenance burden of
	// growing the regex table only added more brittle patterns. The
	// triage LLM, primed with the host-track examples in its
	// system prompt, classifies more reliably and produces the
	// literal host_command directly. Cost is one cheap classification
	// call per user request; transport errors fall back to TrackPlan,
	// the safe default that still hands the user a confirmation step.
	resp, err := s.Send(ctx, SenderRequest{
		Model:     model,
		System:    triageSystemPrompt,
		UserText:  buildTriageUserText(req),
		MaxTokens: triageMaxTokens,
	})
	if err != nil {
		return Result{Track: TrackPlan, Reason: "triage call failed: " + err.Error()}, err
	}
	res, ok := parseResult(resp.Text)
	if !ok {
		return Result{Track: TrackPlan, Reason: "triage response unparseable; defaulting to plan"}, nil
	}
	return res, nil
}

// parseResult extracts the JSON object from a triage response. Models
// sometimes wrap the object in prose or a ```json fence, so we take
// the span from the first '{' to the last '}'. An unknown track is
// treated as a parse failure so the caller falls back to plan.
func parseResult(text string) (Result, bool) {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return Result{}, false
	}
	var r Result
	if err := json.Unmarshal([]byte(text[start:end+1]), &r); err != nil {
		return Result{}, false
	}
	switch r.Track {
	case TrackAnswer, TrackQuick, TrackPlan, TrackClarify, TrackHost:
		return r, true
	default:
		return Result{}, false
	}
}

func buildTriageUserText(req Request) string {
	var b strings.Builder
	b.WriteString("USER REQUEST:\n")
	b.WriteString(strings.TrimSpace(req.UserRequest))
	b.WriteString("\n")
	if req.ForcedMode != "" {
		fmt.Fprintf(&b, "\nFORCED MODE: /%s\n", req.ForcedMode)
	}
	if len(req.Projects) > 0 {
		fmt.Fprintf(&b, "\nWORKSPACE PROJECTS: %s\n", strings.Join(req.Projects, ", "))
	}
	if len(req.ProjectHints) > 0 {
		b.WriteString("\nPROJECT HINTS (use these to map 'run it' / 'build it' / 'test it' / 'start it' to a concrete command):\n")
		for _, h := range req.ProjectHints {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(h))
		}
	}
	if len(req.RecentTurns) > 0 {
		b.WriteString("\nRECENT CONVERSATION:\n")
		for _, t := range req.RecentTurns {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(t))
		}
	}
	return b.String()
}

const triageSystemPrompt = `You are the triage step of the kai coding agent. Classify the user's request into exactly ONE handling track. You do NOT do the work, explore code, or write anything — you only decide the route.

Reply with ONLY a JSON object — no prose, no code fences:
{"track":"<track>","reason":"<one short sentence>","question":"<only for clarify>","answer":"<for answer and host>","host_command":"<only for host: the literal shell command for kai to propose; may be empty>"}

The five tracks:

- "answer" — The request is a question that needs no code change (how does X work, what is Y, should I do Z). Put the full reply in "answer"; be concise and direct.

- "quick" — A change that is trivial AND reversible: a one-to-few-line edit with an obvious, unambiguous target — a typo, a string or constant change, a rename in one spot, an obvious small flag. The kind of thing that needs no plan.

- "plan" — Anything non-trivial: multi-file or multi-step changes, new features, refactors, anything risky or hard to reverse, or anything where the right approach is not obvious. This is the default.

- "clarify" — The request is too ambiguous to route confidently: the target is unclear, the intent could mean several materially different things, or essential information is missing. Put the question to ask in "question". Use this sparingly.

- "host" — The request is a host-shell operation, NOT a code change: installing software onto the user's PATH, sudo / privileged commands, package-manager installs (brew/apt/npm -g/pip/cargo install), registering an MCP server, modifying shell rc files (.bashrc/.zshrc), editing /etc, /usr/local/bin, /opt, logging into a service, running kubectl apply or other deploy commands. These cannot be done from inside the agent's CoW workspace and must be run on the user's host shell — kai will execute the command after the user approves. Put a brief explanation in "answer". Put the literal single-line shell command in "host_command" (kai runs it via 'bash -c' from its current directory; chain with && if needed). Anti-pattern: a request to add/fix/test/refactor the install MACHINERY (the install handler, the install script, the bootstrap function) is a code change — that's "plan", not "host".
  Project-aware recipes — when PROJECT HINTS are provided and the user says "run it" / "start it" / "build it" / "test it" / "launch it" / "fire it up" with no other named target, infer the command from the hints:
    package.json with "dev" script        → host_command: "npm run dev"
    package.json with "start" script only → host_command: "npm start"
    package.json with "build" script + "run/start" verb → host_command: "npm run dev" (build is for the build verb)
    Cargo.toml + run verb                 → host_command: "cargo run"
    Cargo.toml + test verb                → host_command: "cargo test"
    go.mod + run verb                     → host_command: "go run ./..."
    go.mod + build verb                   → host_command: "go build ./..."
    go.mod + test verb                    → host_command: "go test ./..."
    Makefile with matching target         → host_command: "make <target>"
  If multiple managers (e.g. package.json + go.mod), pick the one the verb most naturally maps to. If neither hints nor the prompt make it obvious, ask for clarification instead of guessing — route to "clarify" with a tight question, not a full plan.
  Known kai-specific recipes (use these verbatim when they apply):
    install / put-on-PATH / add-to-PATH         → host_command: "cd kai-cli && make install"
    log in / sign in / authenticate kai         → host_command: "kai auth login"
  For other host tasks (sudo, kubectl apply, etc.) propose the best-guess command from the user's text; the user approves before kai runs it, so an imperfect command is recoverable. If you genuinely can't tell what to run, leave host_command empty and explain in "answer".

Rules:
- Bias conservative. When torn between "quick" and "plan", choose "plan". A needless plan costs one extra confirmation; a mis-routed big change skips review entirely.
- "quick" requires BOTH trivial AND a single obvious target. If you would have to guess where the change goes, it is not "quick" — it is "plan".
- Judge on reversibility, ambiguity, and scope — not on how the request is phrased.
- If the input shows FORCED MODE: /chat, the user has been having a conversation. Lean toward "answer" or "clarify" — but if the request is unambiguously a code change ("fix X", "add Y", "make Z do W"), still route it to "quick" or "plan". A mode the user picked earlier must not trap a real work request.
- Output the JSON object and nothing else.`
