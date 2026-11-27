package cirun

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Bucket is where the fidelity check puts a job before anything runs.
type Bucket string

const (
	BucketRuns    Bucket = "runs"
	BucketCaveats Bucket = "runs_with_caveats"
	BucketCannot  Bucket = "cannot_run"
)

// Fidelity is the verdict for one job.
type Fidelity struct {
	Bucket  Bucket
	Reason  string   // why it can't run here (BucketCannot only)
	Caveats []string // how a local run differs from GitHub's
	// Partial means some matrix entries can't run here, so a pass covers only
	// part of the job. --strict treats that as not run.
	Partial bool
}

// ImageSet names which runner images back the ubuntu labels.
const (
	ImageMedium = "medium"
	ImageFull   = "full"
)

// ImageMap returns runs-on label → container image. set is "medium", "full",
// or a custom image reference used for every ubuntu label.
func ImageMap(set string) map[string]string {
	labels := []string{"ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04"}
	m := map[string]string{}
	for _, l := range labels {
		tag := strings.TrimPrefix(l, "ubuntu-")
		switch set {
		case "", ImageMedium:
			m[l] = "catthehacker/ubuntu:act-" + tag
		case ImageFull:
			m[l] = "catthehacker/ubuntu:full-" + tag
		default:
			m[l] = set
		}
	}
	return m
}

// ClassifyOptions is what the fidelity check needs to know about this machine.
type ClassifyOptions struct {
	Images      map[string]string // supported runs-on labels
	HaveSecrets map[string]bool   // secrets with a local value
}

var (
	pinnedRefRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	exprKeyRE   = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z_][\w-]*)\s*\}\}$`)
)

// Classify sorts one job into a bucket. It looks at the job alone; Resolve
// adds what the jobs it needs imply.
func Classify(j *Job, o ClassifyOptions) Fidelity {
	return classify(j, o, 0)
}

// reusableMaxDepth is how far nested reusable workflow calls are followed;
// GitHub allows four levels.
const reusableMaxDepth = 4

func classify(j *Job, o ClassifyOptions, depth int) Fidelity {
	var f Fidelity
	cannot := func(format string, a ...any) Fidelity {
		return Fidelity{Bucket: BucketCannot, Reason: fmt.Sprintf(format, a...)}
	}

	// Where it runs.
	labels := j.RunsOn
	if j.RunsOnExpr != "" {
		m := exprKeyRE.FindStringSubmatch(strings.TrimSpace(j.RunsOnExpr))
		if m == nil || len(j.MatrixValues[m[1]]) == 0 {
			return cannot("runs-on %s can't be resolved before the run", j.RunsOnExpr)
		}
		var ok, bad []string
		for _, v := range j.MatrixValues[m[1]] {
			if _, have := o.Images[strings.ToLower(v)]; have {
				ok = append(ok, v)
			} else {
				bad = append(bad, v)
			}
		}
		if len(ok) == 0 {
			return cannot("runs-on %s", strings.Join(bad, ", "))
		}
		if len(bad) > 0 {
			f.Partial = true
			f.Caveats = append(f.Caveats, fmt.Sprintf("matrix entries on %s are not run here", strings.Join(bad, ", ")))
		}
	} else if j.Uses == "" {
		switch {
		case len(labels) == 0:
			return cannot("no runs-on")
		case len(labels) > 1:
			return cannot("runs-on [%s] is not a GitHub-hosted Ubuntu runner", strings.Join(labels, ", "))
		}
		if _, have := o.Images[labels[0]]; !have {
			return cannot("runs-on %s", labels[0])
		}
	}

	// What GitHub gives the job that this machine can't.
	if j.IDTokenWrite {
		return cannot("needs an OIDC token (id-token: write)")
	}
	if j.Environment != "" {
		return cannot("uses environment %q, whose protection rules and secrets don't exist here", j.Environment)
	}

	// A local reusable workflow is only as runnable as the jobs it holds.
	if strings.HasPrefix(j.Uses, "./") {
		target := j.Workflow.called(j.Uses)
		switch {
		case target == nil:
			f.Caveats = append(f.Caveats, fmt.Sprintf("calls %s, which Kai couldn't read to check", j.Uses))
		case depth >= reusableMaxDepth:
			f.Caveats = append(f.Caveats, fmt.Sprintf("calls %s, nested too deep to check", j.Uses))
		default:
			for _, tj := range target.Jobs {
				tf := classify(tj, o, depth+1)
				if tf.Bucket == BucketCannot {
					return cannot("calls %s, whose job %s can't run here (%s)", j.Uses, tj.ID, tf.Reason)
				}
				f.Partial = f.Partial || tf.Partial
				for _, c := range tf.Caveats {
					f.Caveats = append(f.Caveats, tj.ID+": "+c)
				}
			}
		}
	}

	// What will run, but differently.
	if j.Uses != "" && !strings.HasPrefix(j.Uses, "./") {
		f.Caveats = append(f.Caveats, fmt.Sprintf("calls a reusable workflow in another repo (%s)", j.Uses))
	}
	if j.HasServices {
		f.Caveats = append(f.Caveats, "services: run as local containers")
	}
	seenCache := false
	var unpinned []string
	for _, u := range j.StepUses {
		if strings.HasPrefix(u, "./") || strings.HasPrefix(u, "docker://") {
			continue
		}
		name, ref, _ := strings.Cut(u, "@")
		if strings.HasPrefix(strings.ToLower(name), "actions/cache") && !seenCache {
			seenCache = true
			f.Caveats = append(f.Caveats, "actions/cache uses a local cache, not GitHub's")
		}
		if !pinnedRefRE.MatchString(ref) {
			unpinned = append(unpinned, u)
		}
	}
	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		unpinned = dedupe(unpinned)
		f.Caveats = append(f.Caveats, "unpinned actions run on this machine: "+strings.Join(unpinned, ", "))
	}
	var missing []string
	for _, s := range j.Secrets {
		if !o.HaveSecrets[s] {
			missing = append(missing, "secrets."+s)
		}
	}
	if len(missing) > 0 {
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		f.Caveats = append(f.Caveats, fmt.Sprintf("%s %s empty here", strings.Join(missing, ", "), verb))
	}

	f.Bucket = BucketRuns
	if len(f.Caveats) > 0 {
		f.Bucket = BucketCaveats
	}
	return f
}

// Resolve is Classify plus the needs graph: a job that needs one that can't
// run here can't run either. act would skip the needed job and carry on, and
// the dependent job would pass without what it depends on.
func Resolve(j *Job, o ClassifyOptions) Fidelity {
	f := Classify(j, o)
	if f.Bucket == BucketCannot {
		return f
	}
	for _, dep := range NeedsClosure(j) {
		if df := Classify(dep, o); df.Bucket == BucketCannot {
			return Fidelity{Bucket: BucketCannot, Reason: fmt.Sprintf("needs %s, which can't run here (%s)", dep.ID, df.Reason)}
		}
	}
	return f
}

// NeedsClosure returns every job j depends on, directly or not, in a stable
// order. Unknown job IDs are ignored; act reports those.
func NeedsClosure(j *Job) []*Job {
	seen := map[string]bool{j.ID: true}
	var out []*Job
	var walk func(*Job)
	walk = func(cur *Job) {
		for _, id := range cur.Needs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if dep := cur.Workflow.Job(id); dep != nil {
				out = append(out, dep)
				walk(dep)
			}
		}
	}
	walk(j)
	return out
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
