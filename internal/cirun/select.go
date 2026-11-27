package cirun

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FindJob resolves a target: "workflow.yml:job", "workflow:job" or "job".
// A bare job ID that exists in more than one workflow is an error that names
// the candidates.
func FindJob(wfs []*Workflow, target string) (*Job, error) {
	wfName, jobID, hasWF := strings.Cut(target, ":")
	if !hasWF {
		jobID, wfName = wfName, ""
	}
	var matches []*Job
	for _, wf := range wfs {
		if wfName != "" && wf.File != wfName && strings.TrimSuffix(strings.TrimSuffix(wf.File, ".yml"), ".yaml") != wfName {
			continue
		}
		if j := wf.Job(jobID); j != nil {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		var all []string
		for _, wf := range wfs {
			for _, j := range wf.Jobs {
				all = append(all, j.Ref())
			}
		}
		sort.Strings(all)
		if len(all) == 0 {
			return nil, fmt.Errorf("no job %q: no workflows found under %s", target, strings.Join(WorkflowDirs, " or "))
		}
		return nil, fmt.Errorf("no job %q. Jobs here: %s", target, strings.Join(all, ", "))
	default:
		var refs []string
		for _, j := range matches {
			refs = append(refs, j.Ref())
		}
		return nil, fmt.Errorf("job %q is in more than one workflow; pick one of: %s", target, strings.Join(refs, ", "))
	}
}

// EventContext is the synthetic event a workflow's triggers are matched
// against.
type EventContext struct {
	Event      string // "push" or "pull_request"
	Branch     string // current branch ("" when HEAD is detached)
	BaseBranch string // the default branch a pull request would target
	// Changed are the files the change touches. ChangedKnown is false when
	// there was no base to diff against; path filters then match, because Kai
	// can't prove the workflow is unaffected.
	Changed      []string
	ChangedKnown bool
}

// Triggered reports whether the event would start the workflow, and if not,
// why not, in words fit to print.
func Triggered(wf *Workflow, ev EventContext) (bool, string) {
	f, ok := wf.On[ev.Event]
	if !ok {
		return false, fmt.Sprintf("not triggered by %s", ev.Event)
	}
	switch ev.Event {
	case "push":
		// A branch push only matches a tags-only trigger if there are no
		// branch filters at all.
		if len(f.Branches) == 0 && len(f.BranchesIgnore) == 0 && (len(f.Tags) > 0 || len(f.TagsIgnore) > 0) {
			return false, "push trigger is for tags only"
		}
		if ok, why := branchMatch(f, ev.Branch, "push to"); !ok {
			return false, why
		}
	case "pull_request", "pull_request_target":
		if ok, why := branchMatch(f, ev.BaseBranch, "pull request into"); !ok {
			return false, why
		}
	}
	if !ev.ChangedKnown {
		return true, ""
	}
	if len(f.Paths) > 0 {
		for _, p := range ev.Changed {
			if matchPatterns(f.Paths, p) {
				return true, ""
			}
		}
		return false, "no changed file matches its paths"
	}
	if len(f.PathsIgnore) > 0 {
		for _, p := range ev.Changed {
			if !matchPatterns(f.PathsIgnore, p) {
				return true, ""
			}
		}
		return false, "every changed file matches its paths-ignore"
	}
	return true, ""
}

func branchMatch(f *EventFilter, branch, verb string) (bool, string) {
	if len(f.Branches) > 0 {
		if branch == "" || !matchPatterns(f.Branches, branch) {
			return false, fmt.Sprintf("%s %s doesn't match branches [%s]", verb, orDetached(branch), strings.Join(f.Branches, ", "))
		}
	}
	if len(f.BranchesIgnore) > 0 && branch != "" && matchPatterns(f.BranchesIgnore, branch) {
		return false, fmt.Sprintf("%s %s matches branches-ignore", verb, branch)
	}
	return true, ""
}

func orDetached(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}

// matchPatterns applies GitHub's filter rules: patterns are tried in order
// and the last one that matches decides, a leading ! making it a "no".
func matchPatterns(patterns []string, name string) bool {
	matched := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if re := filterRegexp(p); re != nil && re.MatchString(name) {
			matched = !neg
		}
	}
	return matched
}

// filterRegexp translates GitHub's filter pattern syntax: * stops at a
// slash, ** doesn't, ? and + apply to the character before them, and [] is a
// character class.
func filterRegexp(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?', '+':
			b.WriteByte(c)
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(p[i : i+end+1])
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
