package cirun

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Status is the outcome of one job. A job that did not run is never "passed".
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusNotRun  Status = "not_run" // can't run on this machine
	StatusSkipped Status = "skipped" // the event doesn't trigger it
	StatusError   Status = "error"   // the runner broke before the job had a result
)

// JobResult is one row of the report.
type JobResult struct {
	ID         string   `json:"id"` // "ci.yml:test"
	Workflow   string   `json:"workflow"`
	Job        string   `json:"job"`
	Status     Status   `json:"status"`
	DurationMS int64    `json:"duration_ms"`
	FailedStep string   `json:"failed_step,omitempty"`
	LogExcerpt string   `json:"log_excerpt,omitempty"`
	Reason     string   `json:"reason,omitempty"` // why it was skipped, not run, or errored
	Caveats    []string `json:"caveats,omitempty"`
	Partial    bool     `json:"partial,omitempty"` // some matrix entries couldn't run here
	Why        string   `json:"why"`               // why it was picked
}

// Report is the result of one kai ci run.
type Report struct {
	SHA        string            `json:"sha"`
	Branch     string            `json:"branch,omitempty"`
	Tree       string            `json:"tree"`  // "working" or "head"
	Dirty      bool              `json:"dirty"` // working tree had uncommitted changes
	Event      string            `json:"event"`
	Base       string            `json:"base,omitempty"`
	ActVersion string            `json:"act_version,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Images     map[string]string `json:"images,omitempty"`
	Strict     bool              `json:"strict"`
	Jobs       []*JobResult      `json:"jobs"`
	ExitCode   int               `json:"exit_code"`
}

// exitCode: 0 when everything that ran passed, 1 when a job failed or the
// runner broke. Under strict, a job that couldn't run here (or could only
// partly run) is also 1.
func (r *Report) exitCode() int {
	for _, j := range r.Jobs {
		switch j.Status {
		case StatusFailed, StatusError:
			return 1
		case StatusNotRun:
			if r.Strict {
				return 1
			}
		}
		if r.Strict && j.Partial {
			return 1
		}
	}
	return 0
}

// WriteSummary prints the per-job table, then the caveats and the log excerpt
// of each failure.
func (r *Report) WriteSummary(w io.Writer) {
	width := 0
	for _, j := range r.Jobs {
		if len(j.ID) > width {
			width = len(j.ID)
		}
	}
	for _, j := range r.Jobs {
		detail := j.Reason
		if j.Status == StatusFailed && j.FailedStep != "" {
			detail = fmt.Sprintf("step %q", j.FailedStep)
		}
		dur := ""
		if j.Status == StatusPassed || j.Status == StatusFailed {
			dur = fmtDuration(time.Duration(j.DurationMS) * time.Millisecond)
		}
		line := fmt.Sprintf("%-*s  %-8s %-7s %s", width, j.ID, strings.ReplaceAll(string(j.Status), "_", " "), dur, detail)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
		for _, c := range j.Caveats {
			fmt.Fprintf(w, "%-*s  caveat: %s\n", width, "", c)
		}
	}
	for _, j := range r.Jobs {
		if j.LogExcerpt == "" || (j.Status != StatusFailed && j.Status != StatusError) {
			continue
		}
		fmt.Fprintf(w, "\n%s", j.ID)
		if j.FailedStep != "" {
			fmt.Fprintf(w, ", step %q", j.FailedStep)
		}
		fmt.Fprintln(w, ":")
		for _, l := range strings.Split(j.LogExcerpt, "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	tree := "the working tree"
	if r.Tree == "head" {
		tree = "a clean checkout"
	} else if r.Dirty {
		tree = "the working tree (uncommitted changes included)"
	}
	fmt.Fprintf(w, "\nRan on %s at %s", tree, short(r.SHA))
	if r.Branch != "" {
		fmt.Fprintf(w, " (%s)", r.Branch)
	}
	fmt.Fprintf(w, ", event %s.\n", r.Event)
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
