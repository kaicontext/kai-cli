// Package boundary keeps kai-cli's dependency on the engine modules
// (github.com/kaicontext/kai-core and github.com/kaicontext/kai-engine)
// clean: kai-cli imports them as versioned modules, it does not carry their
// source. Two things break that boundary, and this guard rejects both:
//
//   - a copy of engine source committed here, which would silently diverge
//     from the real module; and
//   - a `replace` directive in go.mod, which resolves only against a local
//     checkout and breaks the build for everyone else.
//
// A small sentinel denylist additionally flags engine-owned content (e.g. the
// agent system prompt) that belongs in the engine module, not duplicated here.
//
// The checks are pure functions over an injectable root dir and go.mod text,
// so they unit-test with fixtures (boundary_test.go). TestBoundaryClean runs
// them against the real repo; CI runs them on every PR to main.
//
// This package imports only the standard library, by design: the CI job can
// then run it without fetching the engine modules (and thus without the read
// token), which also lets the check run on pull requests from forks. Keep it
// stdlib-only.
package boundary

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Engine module paths. kai-cli must REQUIRE both and REPLACE neither.
const (
	moduleCore   = "github.com/kaicontext/kai-core"
	moduleEngine = "github.com/kaicontext/kai-engine"
	// modulePrefix matches any current or future kaicontext engine module,
	// so a `replace` of any of them is caught even if a third is added later.
	modulePrefix = "github.com/kaicontext/"
)

// Directory names a vendored copy of an engine module would carry (e.g. an
// accidentally committed side-by-side checkout).
const (
	engineDirCore   = "kai-core"
	engineDirEngine = "kai-engine"
)

// guardDirRel is this package's own path. Only its two denylist-bearing files
// (boundary.go, boundary_test.go) are exempt from the sentinel scan — see
// denylistFiles — because they hold the sentinel fragments as literals and
// would otherwise self-trip. Engine source hidden in any OTHER file here is
// still caught. Keep all sentinel literals in boundary.go.
var guardDirRel = filepath.Join("internal", "boundary")

// denylistFiles are the only files exempt from the sentinel scan: they define
// and test the denylist, so the sentinel strings appear in them by necessity.
// Code review covers these two; all sentinel literals must live in boundary.go.
var denylistFiles = map[string]bool{
	filepath.Join(guardDirRel, "boundary.go"):      true,
	filepath.Join(guardDirRel, "boundary_test.go"): true,
}

// Finding is a single boundary violation. Message is human-actionable: it
// names exactly what tripped (a path, a go.mod line, or a sentinel) and how
// to resolve it.
type Finding struct {
	Kind    string // "engine-source" | "replace" | "missing-require" | "sentinel"
	Message string
}

// Sentinel is a distinctive fragment of engine-owned content (the agent
// system prompt, gate logic, etc.) whose canonical home is the engine module.
// Finding it copied into kai-cli source means the boundary slipped. This is
// defense-in-depth behind the directory and go.mod checks; extend the list as
// new engine-owned assets appear.
type Sentinel struct {
	Text   string // distinctive substring to search for
	Origin string // where it belongs, surfaced in the message
}

// sentinels are stable, distinctive fragments. Prompt fragments are unlikely
// to recur by accident; gate fragments pin the exact engine signatures (e.g.
// tasksmd.Task) so a future, unrelated public helper that merely shares a
// name (RunGate) does not false-positive.
var sentinels = []Sentinel{
	{
		Text:   "EDIT BUDGET: You have ~10 read-tool calls",
		Origin: "agent system prompt (kai-engine/agentprompt)",
	},
	{
		Text:   "SANDBOX: You are running inside an isolated workspace",
		Origin: "agent system prompt (kai-engine/agentprompt)",
	},
	{
		Text:   `EXEMPLAR-FIRST for "add a thing like an existing thing"`,
		Origin: "agent system prompt (kai-engine/agentprompt)",
	},
	{
		Text:   "func ExtractGateCommand(acceptance string)",
		Origin: "drive gate logic (kai-engine)",
	},
	{
		Text:   "func RunGate(workDir string, t tasksmd.Task",
		Origin: "drive gate logic (kai-engine)",
	},
}

// Check runs every boundary check against the repo rooted at root, reading
// its go.mod. It returns all findings (empty slice == clean). An error is
// returned only for I/O failures that prevent the guard from running.
func Check(root string) ([]Finding, error) {
	findings, err := CheckTree(root)
	if err != nil {
		return nil, err
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("boundary: reading go.mod: %w", err)
	}
	return append(findings, CheckGoMod(string(gomod))...), nil
}

// CheckTree walks the file tree at root and reports vendored engine-source
// directories and engine-owned sentinels in committed .go files. It ignores
// api/ wrapper files that merely IMPORT the engine — the check is about source
// presence, not imports — because those wrappers contain none of the directory
// names or sentinels this scan looks for.
func CheckTree(root string) ([]Finding, error) {
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			rel := relOrPath(root, path)
			if rel == ".git" {
				return fs.SkipDir
			}
			if name := d.Name(); name == engineDirCore || name == engineDirEngine {
				findings = append(findings, Finding{
					Kind: "engine-source",
					Message: fmt.Sprintf(
						"engine source directory %q present at %s — the engine modules (kai-core/kai-engine) are consumed as versioned dependencies, not vendored here. Remove this directory; this code lives in the engine module.",
						name, rel),
				})
				return fs.SkipDir // one finding per vendored module is enough
			}
			return nil
		}
		rel := relOrPath(root, path)
		// A nested go.mod that declares a kaicontext module is a whole engine
		// module vendored in — catch it under ANY directory name, not just the
		// kai-core/kai-engine names the directory check keys on.
		if d.Name() == "go.mod" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if mod := moduleDecl(string(data)); strings.HasPrefix(mod, modulePrefix) {
				findings = append(findings, Finding{
					Kind: "engine-source",
					Message: fmt.Sprintf(
						"vendored engine module at %s declares `module %s` — engine modules are consumed as versioned dependencies, not vendored here. Remove this copy.",
						rel, mod),
				})
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if denylistFiles[rel] {
			return nil // these legitimately hold the sentinel literals
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range sentinels {
			if bytes.Contains(data, []byte(s.Text)) {
				findings = append(findings, Finding{
					Kind: "sentinel",
					Message: fmt.Sprintf(
						"engine-owned content in %s: contains %q (%s). This belongs in the engine module — import it, don't copy it here.",
						rel, s.Text, s.Origin),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

// CheckGoMod scans go.mod contents (text only — no golang.org/x/mod, which is
// not a kai-cli dependency) and asserts both engine modules are REQUIRED and
// NEITHER is REPLACEd (nor any kaicontext module). This keeps the local-dev
// `replace => ../kai-engine` out of the committed build.
func CheckGoMod(contents string) []Finding {
	var findings []Finding
	lines := strings.Split(contents, "\n")

	// (1) No `replace` of any kaicontext engine module. Every replacement —
	// single-line or inside a `replace ( ... )` block, versioned or not —
	// carries `=>`, so a line with `=>` whose left-hand side names a
	// kaicontext module is rejected. Unrelated replaces are left alone.
	for i, raw := range lines {
		line := stripComment(raw)
		arrow := strings.Index(line, "=>")
		if arrow < 0 {
			continue
		}
		if strings.Contains(line[:arrow], modulePrefix) {
			findings = append(findings, Finding{
				Kind: "replace",
				Message: fmt.Sprintf(
					"replace directive for an engine module at go.mod line %d: %q. A committed replace resolves only against a local checkout and breaks the build for everyone else — use a gitignored go.work for local side-by-side development, and remove this before committing.",
					i+1, strings.TrimSpace(raw)),
			})
		}
	}

	// (2) Both engine modules must be REQUIRED. A dropped require can also
	// mask a vendored copy, so its absence is itself a failure.
	for _, m := range []string{moduleCore, moduleEngine} {
		if !moduleRequired(lines, m) {
			findings = append(findings, Finding{
				Kind: "missing-require",
				Message: fmt.Sprintf(
					"go.mod no longer requires %s. kai-cli depends on this engine module, and a missing require can mask a vendored copy. Restore the `require` entry.",
					m),
			})
		}
	}
	return findings
}

// moduleRequired reports whether module appears on a non-replace line as a
// whole path token (a `require` entry, direct or indirect).
func moduleRequired(lines []string, module string) bool {
	for _, raw := range lines {
		line := stripComment(raw)
		if strings.Contains(line, "=>") {
			continue // a replace line is not a require
		}
		if hasModuleToken(line, module) {
			return true
		}
	}
	return false
}

// hasModuleToken reports whether line contains module as a whole path token
// (at end-of-line or followed by whitespace), so that
// "github.com/kaicontext/kai-core" does not match
// "github.com/kaicontext/kai-core-extra".
func hasModuleToken(line, module string) bool {
	for idx := 0; ; {
		i := strings.Index(line[idx:], module)
		if i < 0 {
			return false
		}
		end := idx + i + len(module)
		if end == len(line) {
			return true
		}
		switch line[end] {
		case ' ', '\t', '\r':
			return true
		}
		idx = end
	}
}

// moduleDecl returns the module path declared by a go.mod's `module` line, or
// "" if there is none.
func moduleDecl(contents string) string {
	for _, raw := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// stripComment drops a `//` line comment so `=>` or a module path hiding in a
// comment is never misread as a directive.
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func relOrPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

// Report renders findings as a single actionable failure message.
func Report(findings []Finding) string {
	var b strings.Builder
	b.WriteString("MODULE BOUNDARY CHECK FAILED — engine source belongs in the engine module, not in kai-cli.\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  • [%s] %s\n", f.Kind, f.Message)
	}
	b.WriteString("\nThe engine ships as versioned modules (kai-core/kai-engine) that kai-cli imports. To fix:\n")
	b.WriteString("  - Engine code lives in the engine module — import it, don't copy it here.\n")
	b.WriteString("  - Use a gitignored go.work (not a go.mod `replace`) for local side-by-side development.\n")
	b.WriteString("  - Keep both engine modules in `require` with no `replace`.\n")
	b.WriteString("See internal/boundary/boundary.go and CONTRIBUTING.md for details.\n")
	return b.String()
}
