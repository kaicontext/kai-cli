package main

// review_commit_description.go — the pull request description a person would
// write.
//
// kai-server puts a "Kai context" section at the end of a pull request's
// description, and what belongs there is what the change is about: the
// summary and the list of changes an author writes when they open the PR. Not
// the review's status, coverage or opinions — those live in the review.
//
// It is written from what the author's own description is written from: the
// commits, whatever the author already said, and the diff. It does not need
// the review, so it runs beside it and costs no time; a description that
// fails or is late is left out (the bundle's description is optional and the
// server writes nothing without one).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcDescriptionMaxBytes bounds the description the bundle carries.
const rcDescriptionMaxBytes = 4000

const rcDescriptionSystem = `You write the description for a pull request, the way its author would when opening it. You are given the commit messages, whatever description the author already wrote, and the diff.

Write GitHub markdown in exactly this shape:

<one to three sentences: what this change does and, when the commits or the author say so, why>

#### Changes
- <one bullet per area of the change: what changed there, concretely — name the files, functions, flags or endpoints a reader would look for>

#### Testing
- <only if the diff adds or changes tests or the commits say how it was verified: what is covered. Omit this heading entirely otherwise.>

Rules:
- Describe the change. Do not review it: no opinions, risks, praise, suggestions, scores or "looks good".
- State only what the diff and the messages show. Never invent a motivation, a ticket, a benchmark or a test that is not there.
- Do not repeat the author's description back; the reader sees it directly above yours. Where it already explains the why, say what the code does.
- Plain and specific, like a careful engineer. No preamble, no headings other than the two above, no closing line, at most 12 bullets.
- Never write an @-mention.`

// rcStartDescription asks for the description in the background and returns
// the call that collects it: the description, or "" when it failed or did not
// arrive within wait of being collected.
func rcStartDescription(ctx context.Context, prov provider.Provider, model, authorContext, diff string) func(wait time.Duration) string {
	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- ""
			}
		}()
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		started := time.Now()
		d, err := rcWriteDescription(cctx, prov, model, authorContext, diff)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  description: not written (%s)\n", rcClip(err.Error(), 200))
			done <- ""
			return
		}
		fmt.Fprintf(os.Stderr, "  timing: description=%s\n", time.Since(started).Round(time.Second))
		done <- d
	}()
	return func(wait time.Duration) string {
		select {
		case d := <-done:
			return d
		case <-time.After(wait):
			fmt.Fprintf(os.Stderr, "  description: not ready in time; left out\n")
			return ""
		}
	}
}

// rcWriteDescription is the one model call.
func rcWriteDescription(ctx context.Context, prov provider.Provider, model, authorContext, diff string) (string, error) {
	var in strings.Builder
	in.WriteString("COMMITS AND THE AUTHOR'S DESCRIPTION:\n")
	in.WriteString(strings.TrimSpace(authorContext))
	in.WriteString("\n\nDIFF:\n")
	in.WriteString(diff)
	resp, err := prov.Send(ctx, provider.Request{
		Model:           model,
		System:          rcDescriptionSystem,
		MaxTokens:       rcTokensFor(1200, rcStageEffort(rcStageIntent)),
		ReasoningEffort: rcStageEffort(rcStageIntent),
		Messages:        []message.Message{{Role: message.RoleUser, Parts: []message.ContentPart{message.TextContent{Text: in.String()}}}},
	})
	if err != nil {
		return "", err
	}
	// A description cut off by the token limit ends mid-thought; none is
	// better than half of one.
	if resp.FinishReason == message.FinishReasonMaxTokens {
		return "", fmt.Errorf("cut off at the token limit")
	}
	var out strings.Builder
	for _, p := range resp.Parts {
		if t, ok := p.(message.TextContent); ok {
			out.WriteString(t.Text)
		}
	}
	return rcCleanDescription(out.String()), nil
}

// rcCleanDescription trims what the model wrote to what the bundle carries:
// no surrounding fence, no live @-mention, bounded on a line (or, with no
// line to cut at, on a character).
func rcCleanDescription(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	s = rcDefuseMentions(s)
	if len(s) > rcDescriptionMaxBytes {
		cut := rcDescriptionMaxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
		if i := strings.LastIndex(s, "\n"); i > 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// rcDefuseMentions keeps the description from mentioning anyone on GitHub,
// whatever the diff led the model to write: a zero-width space after every
// "@" outside a code span (a path or decorator in code is shown as written;
// GitHub does not notify from code), after every "@" when a backtick is
// unpaired. kai-server does the same before it writes; this is the bundle's
// own guarantee.
func rcDefuseMentions(s string) string {
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 {
		return strings.ReplaceAll(s, "@", "@\u200b")
	}
	for i := 0; i < len(parts); i += 2 {
		parts[i] = strings.ReplaceAll(parts[i], "@", "@\u200b")
	}
	return strings.Join(parts, "`")
}
