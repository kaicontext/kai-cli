// Package cirun runs a repo's GitHub Actions workflows locally by driving a
// pinned nektos/act binary. Kai reads the workflow files itself only to pick
// jobs and to say, before anything runs, what can't run faithfully here; act
// owns execution (the needs graph, matrix expansion, expressions, actions).
package cirun

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkflowDirs are read in this order. .kailab/workflows uses the same syntax.
var WorkflowDirs = []string{".github/workflows", ".kailab/workflows"}

// Workflow is one workflow file, reduced to what job picking and the fidelity
// check need.
type Workflow struct {
	// File names the workflow in job refs. It is the base name ("ci.yml")
	// for .github/workflows and the repo-relative path for any other
	// directory, so two directories with a ci.yml don't share a ref.
	File string
	Dir  string // repo-relative directory, slash-separated
	Path string // absolute path
	// siblings are all workflows loaded with this one, for resolving a
	// local reusable workflow call.
	siblings []*Workflow
	Name     string
	On       map[string]*EventFilter // event name → filter (never nil)
	Jobs     []*Job                  // in file order
}

// EventFilter holds the branch/tag/path filters of one trigger.
type EventFilter struct {
	Branches       []string
	BranchesIgnore []string
	Tags           []string
	TagsIgnore     []string
	Paths          []string
	PathsIgnore    []string
}

// Job is one job in a workflow.
type Job struct {
	Workflow *Workflow
	ID       string
	Name     string

	RunsOn     []string // literal labels, lowercased
	RunsOnExpr string   // set when runs-on is a ${{ }} expression
	// MatrixValues are the scalar values of each matrix key, including those
	// that only appear under include:.
	MatrixValues map[string][]string

	Needs        []string
	Uses         string // job-level reusable workflow reference
	Environment  string
	IDTokenWrite bool
	HasServices  bool
	StepUses     []string // every step-level uses:, in order
	Secrets      []string // secrets.X names referenced, sorted
}

// Ref is the job's display and selection name: "ci.yml:test".
func (j *Job) Ref() string { return j.Workflow.File + ":" + j.ID }

// LoadWorkflows reads every workflow under repoDir. A missing directory is not
// an error; a workflow that doesn't parse is.
func LoadWorkflows(repoDir string) ([]*Workflow, error) {
	var out []*Workflow
	for _, dir := range WorkflowDirs {
		abs := filepath.Join(repoDir, filepath.FromSlash(dir))
		entries, err := os.ReadDir(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if e.IsDir() || (ext != ".yml" && ext != ".yaml") {
				continue
			}
			p := filepath.Join(abs, e.Name())
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			wf, err := ParseWorkflow(e.Name(), data)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", dir, e.Name(), err)
			}
			wf.Path, wf.Dir = p, dir
			if dir != WorkflowDirs[0] {
				wf.File = dir + "/" + e.Name()
			}
			out = append(out, wf)
		}
	}
	for _, wf := range out {
		wf.siblings = out
	}
	return out, nil
}

// RelPath is the workflow's repo-relative path, slash-separated.
func (w *Workflow) RelPath() string {
	dir := w.Dir
	if dir == "" {
		dir = WorkflowDirs[0]
	}
	name := w.File
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return dir + "/" + name
}

// called returns the workflow a local `uses: ./path/to/workflow.yml` refers
// to, or nil when it isn't among the loaded ones.
func (w *Workflow) called(uses string) *Workflow {
	rel, _, _ := strings.Cut(strings.TrimPrefix(uses, "./"), "@")
	for _, s := range w.siblings {
		if s.RelPath() == rel {
			return s
		}
	}
	return nil
}

// ParseWorkflow parses one workflow file.
func ParseWorkflow(file string, data []byte) (*Workflow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a workflow: top level is not a mapping")
	}
	wf := &Workflow{File: file, On: map[string]*EventFilter{}}
	if n := mapGet(root, "name"); n != nil {
		wf.Name = n.Value
	}
	parseOn(mapGet(root, "on"), wf.On)

	wfIDToken, _ := idTokenWrite(mapGet(root, "permissions"))
	wfEnv := nodeText(mapGet(root, "env"))

	jobs := mapGet(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return wf, nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		id, n := jobs.Content[i].Value, resolveAlias(jobs.Content[i+1])
		if n.Kind != yaml.MappingNode {
			continue
		}
		j := &Job{Workflow: wf, ID: id, MatrixValues: map[string][]string{}}
		if v := mapGet(n, "name"); v != nil {
			j.Name = v.Value
		}
		parseRunsOn(mapGet(n, "runs-on"), j)
		j.Needs = scalarList(mapGet(n, "needs"))
		if v := mapGet(n, "uses"); v != nil {
			j.Uses = v.Value
		}
		if v := mapGet(n, "environment"); v != nil {
			if v.Kind == yaml.MappingNode {
				if name := mapGet(v, "name"); name != nil {
					j.Environment = name.Value
				}
			} else {
				j.Environment = v.Value
			}
		}
		// A job-level permissions block replaces the workflow-level one.
		if write, set := idTokenWrite(mapGet(n, "permissions")); set {
			j.IDTokenWrite = write
		} else {
			j.IDTokenWrite = wfIDToken
		}
		if v := mapGet(n, "services"); v != nil && len(v.Content) > 0 {
			j.HasServices = true
		}
		if strat := mapGet(n, "strategy"); strat != nil {
			parseMatrix(mapGet(strat, "matrix"), j.MatrixValues)
		}
		if steps := mapGet(n, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
			for _, s := range steps.Content {
				if u := mapGet(resolveAlias(s), "uses"); u != nil && u.Value != "" {
					j.StepUses = append(j.StepUses, u.Value)
				}
			}
		}
		j.Secrets = secretRefs(wfEnv + "\n" + nodeText(n))
		wf.Jobs = append(wf.Jobs, j)
	}
	return wf, nil
}

// Job returns the job with the given ID, or nil.
func (w *Workflow) Job(id string) *Job {
	for _, j := range w.Jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func parseOn(n *yaml.Node, out map[string]*EventFilter) {
	n = resolveAlias(n)
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		out[n.Value] = &EventFilter{}
	case yaml.SequenceNode:
		for _, e := range n.Content {
			out[e.Value] = &EventFilter{}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			f := &EventFilter{}
			if v := resolveAlias(n.Content[i+1]); v != nil && v.Kind == yaml.MappingNode {
				f.Branches = scalarList(mapGet(v, "branches"))
				f.BranchesIgnore = scalarList(mapGet(v, "branches-ignore"))
				f.Tags = scalarList(mapGet(v, "tags"))
				f.TagsIgnore = scalarList(mapGet(v, "tags-ignore"))
				f.Paths = scalarList(mapGet(v, "paths"))
				f.PathsIgnore = scalarList(mapGet(v, "paths-ignore"))
			}
			out[n.Content[i].Value] = f
		}
	}
}

func parseRunsOn(n *yaml.Node, j *Job) {
	n = resolveAlias(n)
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode { // {group: ..., labels: ...}
		if mapGet(n, "group") != nil {
			// A runner group is always an org's own runners.
			j.RunsOn = append(j.RunsOn, "self-hosted")
		}
		n = resolveAlias(mapGet(n, "labels"))
		if n == nil {
			return
		}
	}
	for _, v := range scalarList(n) {
		if strings.Contains(v, "${{") {
			j.RunsOnExpr = v
			continue
		}
		j.RunsOn = append(j.RunsOn, strings.ToLower(strings.TrimSpace(v)))
	}
}

// matrixMaxCombos bounds the matrix expansion done to apply exclude:. GitHub
// caps a matrix at 256 jobs; anything larger than this is left unexpanded.
const matrixMaxCombos = 4096

// parseMatrix collects the values each matrix key can take in a job GitHub
// would schedule: the listed values, minus those every combination of which
// is removed by exclude:, plus those that only appear under include:.
func parseMatrix(n *yaml.Node, out map[string][]string) {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	add := func(m map[string][]string, k, v string) {
		for _, have := range m[k] {
			if have == v {
				return
			}
		}
		m[k] = append(m[k], v)
	}
	entries := func(val *yaml.Node) []map[string]string {
		var list []map[string]string
		if val.Kind != yaml.SequenceNode {
			return nil
		}
		for _, entry := range val.Content {
			entry = resolveAlias(entry)
			if entry.Kind != yaml.MappingNode {
				continue
			}
			m := map[string]string{}
			for k := 0; k+1 < len(entry.Content); k += 2 {
				if e := resolveAlias(entry.Content[k+1]); e.Kind == yaml.ScalarNode {
					m[entry.Content[k].Value] = e.Value
				}
			}
			list = append(list, m)
		}
		return list
	}

	base := map[string][]string{}
	var keys []string
	var includes, excludes []map[string]string
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, resolveAlias(n.Content[i+1])
		switch {
		case key == "include":
			includes = entries(val)
		case key == "exclude":
			excludes = entries(val)
		case val.Kind == yaml.SequenceNode:
			for _, e := range val.Content {
				if e = resolveAlias(e); e.Kind == yaml.ScalarNode {
					add(base, key, e.Value)
				}
			}
			if len(base[key]) > 0 {
				keys = append(keys, key)
			}
		}
	}

	combos := 1
	for _, k := range keys {
		if combos *= len(base[k]); combos > matrixMaxCombos {
			break
		}
	}
	if len(excludes) == 0 || combos > matrixMaxCombos {
		for _, k := range keys {
			out[k] = append(out[k], base[k]...)
		}
	} else {
		combo := map[string]string{}
		var walk func(int)
		walk = func(i int) {
			if i == len(keys) {
				for _, ex := range excludes {
					if len(ex) == 0 {
						continue
					}
					hit := true
					for k, v := range ex {
						if combo[k] != v {
							hit = false
							break
						}
					}
					if hit {
						return
					}
				}
				for k, v := range combo {
					add(out, k, v)
				}
				return
			}
			for _, v := range base[keys[i]] {
				combo[keys[i]] = v
				walk(i + 1)
			}
		}
		walk(0)
	}
	for _, inc := range includes {
		ks := make([]string, 0, len(inc))
		for k := range inc {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			add(out, k, inc[k])
		}
	}
}

// idTokenWrite reports whether a permissions node grants id-token: write, and
// whether the node was present at all.
func idTokenWrite(n *yaml.Node) (write, set bool) {
	n = resolveAlias(n)
	if n == nil {
		return false, false
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value == "write-all", true
	}
	if v := mapGet(n, "id-token"); v != nil {
		return v.Value == "write", true
	}
	return false, true
}

var secretRefRE = regexp.MustCompile(`\bsecrets\.([A-Za-z_][A-Za-z0-9_]*)`)

func secretRefs(text string) []string {
	seen := map[string]bool{}
	for _, m := range secretRefRE.FindAllStringSubmatch(text, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return resolveAlias(n.Content[i+1])
		}
	}
	return nil
}

func scalarList(n *yaml.Node) []string {
	n = resolveAlias(n)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "" {
			return nil
		}
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, e := range n.Content {
			if e = resolveAlias(e); e.Kind == yaml.ScalarNode {
				out = append(out, e.Value)
			}
		}
		return out
	}
	return nil
}

func nodeText(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return string(b)
}
