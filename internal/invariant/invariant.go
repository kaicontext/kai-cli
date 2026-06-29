// Package invariant is a deterministic, LLM-free checker for declared
// co-occurrence invariants of the form "any function that calls Trigger
// must also call Require." It exists to make the gate reviewer's
// forward-consequence-trace finding (#2 — a mutation that skips a
// recompute its siblings perform) enforceable without a model: you
// declare the invariant once, and static analysis enforces it forever.
//
// It is the same discipline as the pricing cross-reference: a finding a
// human/LLM discovered once, promoted to a decidable check. The model's
// job moves from "find the bug every time" to "name the rule once."
package invariant

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rule is one declared co-occurrence invariant. Trigger and Require are
// matched against the FINAL identifier of a call expression, so
// `h.db.AddMember(...)` matches Trigger "AddMember" regardless of the
// receiver. Message, when set, is shown instead of the default text.
type Rule struct {
	Trigger string
	Require string
	Message string
}

// Violation is one function that calls a rule's Trigger but not its
// Require (same function body).
type Violation struct {
	File    string
	Func    string
	Line    int
	Trigger string
	Require string
	Message string
}

// suppressMarker in a function's doc comment opts that function out of
// all invariant checks — the escape hatch for a deliberate exception.
const suppressMarker = "kit:invariant-ok"

// skipDirs are never walked.
var skipDirs = map[string]bool{
	".git": true, ".kai": true, "node_modules": true, "vendor": true,
}

// Check walks root's Go source (excluding _test.go) and returns every
// function that violates a declared co-occurrence rule. Deterministic; no
// model. Same-function scope by design: the check asks "does the handler
// that calls Trigger also, right here, call Require?" — cross-function
// reachability is intentionally not chased (it trades false positives for
// false negatives and is a future extension).
func Check(root string, rules []Rule) ([]Violation, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	var out []Violation
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		file, perr := parser.ParseFile(fset, p, src, parser.ParseComments)
		if perr != nil {
			return nil // unparseable file — skip rather than fail the whole run
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if hasSuppress(fn) {
				continue
			}
			called := calledNames(fn.Body)
			for _, r := range rules {
				// A function is a trigger-site only if it CALLS the
				// trigger. We deliberately do NOT skip by function name:
				// a handler can legitimately share the trigger's name
				// (old-kai's HTTP `Handler.AddMember` calls the DB
				// `AddMember` and still must recompute). The trigger's
				// own definition isn't a trigger-site unless it recurses,
				// which is rare and arguably also wants the require call.
				if called[r.Trigger] && !called[r.Require] {
					out = append(out, Violation{
						File:    rel,
						Func:    fn.Name.Name,
						Line:    fset.Position(fn.Pos()).Line,
						Trigger: r.Trigger,
						Require: r.Require,
						Message: r.Message,
					})
				}
			}
		}
		return nil
	})

	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, err
}

// calledNames returns the set of final-identifier names of every call
// expression in a function body (idents and selectors alike).
func calledNames(body *ast.BlockStmt) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			names[f.Name] = true
		case *ast.SelectorExpr:
			names[f.Sel.Name] = true
		}
		return true
	})
	return names
}

// hasSuppress reports whether a function's doc comment opts it out.
func hasSuppress(fn *ast.FuncDecl) bool {
	if fn.Doc == nil {
		return false
	}
	return strings.Contains(fn.Doc.Text(), suppressMarker)
}
