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
		if wfName != "" && !wfNameMatches(wf, wfName) {
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

// wfNameMatches accepts a workflow's ref name, its repo-relative path, its
// base name, and the base name without the extension.
func wfNameMatches(wf *Workflow, name string) bool {
	base := wf.File
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
	return name == wf.File || name == wf.RelPath() || name == base || name == stem
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

// ValidateFilters reports a branch, tag or path filter of the event's trigger
// that isn't a valid pattern. GitHub rejects such a workflow; matching it as
// "no match" would skip the workflow, or drop an ignore rule, without a word.
func ValidateFilters(wf *Workflow, event string) error {
	f, ok := wf.On[event]
	if !ok {
		return nil
	}
	for _, list := range [][]string{f.Branches, f.BranchesIgnore, f.Tags, f.TagsIgnore, f.Paths, f.PathsIgnore} {
		for _, p := range list {
			if _, err := filterRegexp(strings.TrimPrefix(p, "!")); err != nil {
				return fmt.Errorf("invalid filter pattern %q in its %s trigger", p, event)
			}
		}
	}
	return nil
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
		if re, err := filterRegexp(p); err == nil && re.MatchString(name) {
			matched = !neg
		}
	}
	return matched
}

// filterRegexp translates GitHub's filter pattern syntax: * stops at a
// slash, ** doesn't, ? and + apply to the character before them, and [] is a
// character class.
func filterRegexp(p string) (*regexp.Regexp, error) {
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
	return regexp.Compile(b.String())
}
