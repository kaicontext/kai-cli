package main

// review_commit_backups.go — a failing model falls back only inside its set.
//
// Each review model set has its own backup models (kai-server's review sets,
// Backups and Summary), sent to the review as KAI_REVIEW_FALLBACK_MODELS and
// KAI_REVIEW_SUMMARY_MODEL. With them, a failing step falls back only to the
// set's backups, in order, and a long conversation is summarised by the set's
// own model: Free stays on Google models, and Light stays cheap.
//
// Without them the review behaves exactly as before: the engine's built-in
// failover chains, its default summarising model, and one plain call per
// one-shot step.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

// rcSetBackups is KAI_REVIEW_FALLBACK_MODELS, a comma list, and whether the
// server sent it. Unset, blank or only separators is not sent: the server
// never sends a list with no model, and treating one as "no backups" would
// turn a workflow's empty placeholder into every failover switched off.
func rcSetBackups() ([]string, bool) {
	v := strings.TrimSpace(os.Getenv("KAI_REVIEW_FALLBACK_MODELS"))
	if v == "" {
		return nil, false
	}
	out := []string{}
	seen := map[string]bool{}
	for _, m := range strings.Split(v, ",") {
		m = strings.TrimSpace(m)
		if m == "" || seen[strings.ToLower(m)] {
			continue
		}
		seen[strings.ToLower(m)] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		// Only separators: no list was sent. An empty list would switch
		// every failover off.
		return nil, false
	}
	return out, true
}

// rcSummaryModel is KAI_REVIEW_SUMMARY_MODEL, "" when unset: the model the
// engine summarises a long conversation with (agent.Options.CompactionModel).
// Unset keeps the engine's default.
func rcSummaryModel() string {
	return strings.TrimSpace(os.Getenv("KAI_REVIEW_SUMMARY_MODEL"))
}

// Reasons a one-shot step or a resumed review moved to a backup, as
// run.failovers reports them beside the engine's own (agent.ModelSwap*).
const (
	rcSwapEmpty     = "empty"     // the model answered nothing, twice
	rcSwapTransient = "transient" // the model's call failed after the provider's retries
	rcSwapNoCoda    = "no_coda"   // a conclusion came back with no verdict block, twice
	rcSwapResume    = "resume"    // the review agent was resumed on a backup
)

type rcNoSendBackupsKey struct{}

// rcWithoutSendBackups marks ctx so rcSend makes one plain call: for a caller
// that walks the backups itself (the conclusion, rcConcludeFromTranscript).
func rcWithoutSendBackups(ctx context.Context) context.Context {
	return context.WithValue(ctx, rcNoSendBackupsKey{}, true)
}

// rcResponseEmpty reports whether a one-shot reply carried no text.
func rcResponseEmpty(resp provider.Response) bool {
	for _, p := range resp.Parts {
		switch t := p.(type) {
		case message.TextContent:
			if strings.TrimSpace(t.Text) != "" {
				return false
			}
		case message.ToolCall:
			return false
		}
	}
	return true
}

// rcSendOnSet is one one-shot step's call. With no set backups sent, or for a
// caller that walks them itself, it is prov.Send. Otherwise an empty reply is
// retried once on the same model, and an empty or transiently failing model
// then hands the same request to each backup in order until one answers. A
// hard failure (auth, a spent budget, a bad request) is returned as it is: no
// other model can fix it. It returns the model that answered, "" when every
// model answered nothing. Each call after the first is counted in the
// stage's retries, as the engine's own re-sends are for an agent stage.
func rcSendOnSet(ctx context.Context, prov provider.Provider, req provider.Request, stage string) (provider.Response, string, error) {
	backups, sent := rcSetBackups()
	if !sent || ctx.Value(rcNoSendBackupsKey{}) != nil {
		resp, err := prov.Send(ctx, req)
		return resp, req.Model, err
	}
	resp, err := prov.Send(ctx, req)
	if err == nil && rcResponseEmpty(resp) && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "  %s: %s answered nothing — asking it once more\n", stage, req.Model)
		rcRun.retried(stage)
		resp, err = prov.Send(ctx, req)
	}
	empty := err == nil && rcResponseEmpty(resp)
	if !empty && !rcFailoverWorthy(ctx, err) {
		return resp, req.Model, err
	}
	reason := rcSwapTransient
	if empty {
		reason = rcSwapEmpty
	}
	for _, b := range backups {
		if ctx.Err() != nil {
			break
		}
		if strings.EqualFold(b, req.Model) {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s: %s failed (%s) — trying the set's backup %s\n", stage, req.Model, reason, b)
		rcRun.swapped(stage, req.Model, b, reason)
		rcRun.retried(stage)
		next := req
		next.Model = b
		r2, e2 := prov.Send(ctx, next)
		if e2 == nil && !rcResponseEmpty(r2) {
			return r2, b, nil
		}
		if provider.IsCapExceeded(e2) {
			// The budget is spent for every model, so no further backup
			// can answer, and this error, not the first, says why. Other
			// hard errors can be one model's own (not routable, say), so
			// the next backup is still worth asking.
			return r2, b, e2
		}
	}
	// Every backup failed too. The first model's answer or error is
	// returned: it says why the step failed.
	//
	// An empty answer stays an empty answer, not an error, on purpose: every
	// caller already treats an empty reply as its own step failing, the way
	// it does with no backups sent. Intent carries on blank and notes
	// intent_empty, a sweep chunk counts as lost (sweep_failed), and rank,
	// humanize and grouping note their failure. An error instead would end
	// the whole review at the intent step with no bundle at all. The run
	// report still records the step as failed: rcSend turns the "" into
	// errRcNoAnswer for the recorder.
	if err == nil {
		return resp, "", nil
	}
	return resp, req.Model, err
}
