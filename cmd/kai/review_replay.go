package main

// kai review-replay: run the selection steps again over a saved review record.
//
// The recall plan measures a change to ranking or the publish gate without
// paying for the finders and the fact-check again: the saved bundle
// (finding JSON, challenge.allegations) already holds every confirmed defect.
// This restores the allegations the selection and the gate had withheld,
// runs rcRankSupported and rcPublishGate on them with the current build, and
// prints which defects would be published. The ranker is a model call;
// nothing else is (the rewording of published defects is skipped: it changes
// the words, never the choice). Hidden: it is a measurement tool, not a
// product feature.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	replayRecord      string
	replayNoCorrobate bool
)

var reviewReplayCmd = &cobra.Command{
	Use:    "review-replay",
	Short:  "Re-run ranking and the publish gate over a saved review (benchmark tool)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runReviewReplay,
}

func init() {
	reviewReplayCmd.Flags().StringVar(&replayRecord, "record", "", "a saved review JSON (the bundle review-commit --format json writes)")
	reviewReplayCmd.Flags().BoolVar(&replayNoCorrobate, "no-corroboration", false, "leave out the single-proposal rule, as when the second sweep did not run")
	rootCmd.AddCommand(reviewReplayCmd)
}

type rcReplayRecord struct {
	Intent struct {
		Stated string `json:"stated"`
	} `json:"intent"`
	Files int `json:"files"`
	Diff  struct {
		Files []struct {
			Path  string `json:"path"`
			Patch string `json:"patch"`
		} `json:"files"`
	} `json:"diff"`
	Challenge *rcChallengeResult `json:"challenge"`
}

// rcReplayOut is what the replay prints: each allegation's id and what became
// of it.
type rcReplayOut struct {
	Ranked      bool                `json:"ranked"`
	Selection   *rcSelectionRecord  `json:"selection,omitempty"`
	Allegations []rcReplayAllegaton `json:"allegations"`
}

type rcReplayAllegaton struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	Rank   string `json:"rank,omitempty"`
	Reason string `json:"reason,omitempty"`
	Votes  int    `json:"votes,omitempty"`
}

// rcReplayRestore undoes what the selection and the gate did to a saved
// record: an allegation they withheld becomes supported again, and one the
// one-claim rewrite reworded gets the check's wording back.
func rcReplayRestore(res *rcChallengeResult) {
	for i := range res.Allegations {
		a := &res.Allegations[i]
		if a.Status == rcStatusRefuted && strings.HasPrefix(a.Reason, rcSelectionSkipPrefix) {
			a.Status, a.Reason = rcStatusSupported, ""
		}
		if a.Status == rcStatusSupported && a.Original != nil {
			a.Issue, a.Finding, a.Remedy = a.Original.Issue, a.Original.Finding, a.Original.Remedy
			a.Original = nil
		}
		a.Rank, a.Votes = "", 0
	}
}

// rcReplayDiff rebuilds a diff and the changed line ranges from the record's
// per-file patches.
func rcReplayDiff(rec *rcReplayRecord) (string, *rcRepo) {
	var b strings.Builder
	repo := &rcRepo{changed: map[string][][2]int{}}
	for _, f := range rec.Diff.Files {
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n%s\n", f.Path, f.Path, f.Patch)
		line, start := 0, 0
		flush := func(end int) {
			if start > 0 {
				repo.changed[f.Path] = append(repo.changed[f.Path], [2]int{start, end})
				start = 0
			}
		}
		for _, l := range strings.Split(f.Patch, "\n") {
			switch {
			case strings.HasPrefix(l, "@@"):
				flush(line - 1)
				var c int
				if _, err := fmt.Sscanf(l[strings.Index(l, "+"):], "+%d", &c); err == nil {
					line = c
				}
			case strings.HasPrefix(l, "+"):
				if start == 0 {
					start = line
				}
				line++
			case strings.HasPrefix(l, "-"):
				flush(line - 1)
			default:
				flush(line - 1)
				line++
			}
		}
		flush(line - 1)
	}
	return b.String(), repo
}

func runReviewReplay(cmd *cobra.Command, _ []string) error {
	raw, err := os.ReadFile(replayRecord)
	if err != nil {
		return err
	}
	var rec rcReplayRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("reading %s: %w", replayRecord, err)
	}
	if rec.Challenge == nil || len(rec.Challenge.Allegations) == 0 {
		return fmt.Errorf("%s has no challenge.allegations to replay", replayRecord)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	prov, _, _ := rcReviewProvider()
	if prov == nil {
		return fmt.Errorf("no model provider: log in with kai first")
	}
	res := rec.Challenge
	rcReplayRestore(res)
	diff, repo := rcReplayDiff(&rec)
	ranked := rcRankSupported(ctx, prov, rec.Intent.Stated, diff, rec.Files, res)
	rcPublishGate(res, repo, ranked && !replayNoCorrobate)
	out := rcReplayOut{Ranked: ranked, Selection: res.Selection}
	for _, a := range res.Allegations {
		out.Allegations = append(out.Allegations, rcReplayAllegaton{ID: a.ID, Status: a.Status, Rank: a.Rank, Reason: a.Reason, Votes: a.Votes})
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
