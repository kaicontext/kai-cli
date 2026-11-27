package cirun

import (
	"encoding/json"
	"strings"
	"time"
)

// actLine is one line of `act --json` output. act logs through logrus, so a
// line carries whichever of these fields were in scope when it was written.
type actLine struct {
	Level      string `json:"level"`
	Msg        string `json:"msg"`
	Time       string `json:"time"`
	JobID      string `json:"jobID"`
	Stage      string `json:"stage"`
	Step       string `json:"step"`
	StepResult string `json:"stepResult"`
	JobResult  string `json:"jobResult"`
	RawOutput  bool   `json:"raw_output"`
}

const (
	stepLogLines  = 60 // lines of a step's output kept for the excerpt
	otherLogLines = 30 // lines kept that belong to no job (act's own errors)
)

type stepRec struct {
	name, stage, result string
	log                 []string
	lastErr             string
}

// jobStream is what act said about one job ID across a run. A matrix job
// reports once per entry under the same ID; a failure in any entry sticks.
type jobStream struct {
	result      string // "", "success" or "failure"
	steps       []*stepRec
	first, last time.Time
	failedStep  string
	failedLog   []string
}

type streamParser struct {
	jobs  map[string]*jobStream
	other []string
	// onStep and onRaw feed the live view; either may be nil.
	onStep func(jobID, stage, step, result string)
	onRaw  func(jobID, line string)
}

func newStreamParser() *streamParser {
	return &streamParser{jobs: map[string]*jobStream{}}
}

func (p *streamParser) feed(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	var l actLine
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &l) != nil {
		p.other = appendTail(p.other, line, otherLogLines)
		return
	}
	if l.JobID == "" {
		switch l.Level {
		case "error", "fatal", "panic", "warning":
			p.other = appendTail(p.other, l.Msg, otherLogLines)
		}
		return
	}
	js := p.jobs[l.JobID]
	if js == nil {
		js = &jobStream{}
		p.jobs[l.JobID] = js
	}
	if t, err := time.Parse(time.RFC3339, l.Time); err == nil {
		if js.first.IsZero() {
			js.first = t
		}
		js.last = t
	}

	msg := strings.TrimRight(l.Msg, "\r\n")
	// A step with no name: is named after its whole script.
	if first, _, multi := strings.Cut(strings.TrimSpace(l.Step), "\n"); multi {
		l.Step = first + " …"
	}
	var step *stepRec
	if l.Step != "" {
		step = js.step(l.Stage, l.Step)
		if l.RawOutput {
			step.log = appendTail(step.log, msg, stepLogLines)
			if p.onRaw != nil {
				p.onRaw(l.JobID, msg)
			}
		} else if l.Level == "error" && l.StepResult == "" {
			// act's own complaint about the step, e.g. its exit code. Only
			// the last one is kept: act repeats some of them.
			step.lastErr = msg
		}
	}
	if l.StepResult != "" {
		name := l.Step
		if step != nil {
			step.result = l.StepResult
		} else if _, after, ok := strings.Cut(msg, " - "); ok {
			// "Set up job" and "Complete job" report without a step field.
			name = strings.TrimSpace(after)
		}
		if l.StepResult == "failure" && js.failedStep == "" {
			js.failedStep = name
			if step != nil {
				js.failedLog = append([]string(nil), step.log...)
				if step.lastErr != "" {
					js.failedLog = append(js.failedLog, step.lastErr)
				}
			}
		}
		if p.onStep != nil {
			p.onStep(l.JobID, l.Stage, name, l.StepResult)
		}
	}
	switch l.JobResult {
	case "":
	case "success":
		if js.result == "" {
			js.result = "success"
		}
	default:
		js.result = "failure"
	}
}

func (js *jobStream) step(stage, name string) *stepRec {
	for _, s := range js.steps {
		if s.stage == stage && s.name == name {
			return s
		}
	}
	s := &stepRec{name: name, stage: stage}
	js.steps = append(js.steps, s)
	return s
}

func appendTail(buf []string, line string, max int) []string {
	buf = append(buf, line)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}
