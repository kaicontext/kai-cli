package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Every command that opens a telemetry event closes it through
// finishCommand, so the event reports the error the command returned.
// The classifier has its own tests; this one pins the wiring, which
// they cannot see: a command put back on `defer te.Finish()` would
// report ok for every failure again and nothing else would notice.
//
// The package is parsed, not pattern-matched: every telemetry.NewEvent
// call in this package's non-test files, however it is written, must be
// the assignment
// of a string-literal event in a top-level function's body, followed by
// `defer func() { finishCommand(<the event>, err) }()`, in a function
// whose one result is the named `err error` that deferred call reads.
//
// The defer has to be the very next statement, on purpose: anything in
// between could return early, and an event opened but never finished is
// a command that ran and was never counted.
//
// The rule is for commands, which return one error. The TUI's own events
// (gate review, the negativity signal, in internal/tui/views) are opened
// inside a session, set their result at each branch and are not covered.
func TestEveryCommandEventIsFinishedWithItsResult(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			// The package is found by its import path, so an alias
			// (`tm "…/telemetry"`) is seen too; a file that does not
			// import it cannot open an event.
			telemetryName := importName(f, telemetryImportPath)
			if telemetryName == "" {
				continue
			}
			isNewEvent := func(call *ast.CallExpr) bool { return isCallOn(call, telemetryName, "NewEvent") }
			accepted := map[*ast.CallExpr]bool{}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				stmts := fd.Body.List
				for i, st := range stmts {
					as, ok := st.(*ast.AssignStmt)
					if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
						continue
					}
					call, ok := as.Rhs[0].(*ast.CallExpr)
					if !ok || !isNewEvent(call) {
						continue
					}
					accepted[call] = true
					at := fset.Position(as.Pos())
					event, ok := eventLiteral(call)
					switch {
					case len(call.Args) != 1:
						t.Errorf("%s: NewEvent takes the event name and nothing else here, got %d arguments", at, len(call.Args))
					case !ok:
						t.Errorf("%s: the event name must be a string literal", at)
					}
					seen = append(seen, event)
					v, ok := as.Lhs[0].(*ast.Ident)
					if !ok {
						t.Errorf("%s: the event must be assigned to a plain variable", at)
						continue
					}
					if !namedErrResult(fd) {
						t.Errorf("%s: %s must return a named `err error`, the value finishCommand reads", at, fd.Name.Name)
					}
					if i+1 >= len(stmts) || !isFinishDefer(stmts[i+1], v.Name) {
						t.Errorf("%s: the %q event is not closed by `defer func() { finishCommand(%s, err) }()` as the very next statement (nothing may come between: it could return early)", at, event, v.Name)
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isNewEvent(call) && !accepted[call] {
					t.Errorf("%s: a telemetry event must be opened as `v := telemetry.NewEvent(...)` as a top-level statement of the command's body — not inside a block, a closure or a larger expression, where an early return could leave it unfinished", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	// A sanity floor on the scan itself: the ten commands are there.
	if len(seen) < 10 {
		t.Fatalf("expected at least the ten command events, found %d: %v", len(seen), seen)
	}
}

const telemetryImportPath = "github.com/kaicontext/kai-engine/telemetry"

// importName is the name path is imported under in f: its alias, else
// the last element of the path; "" when f does not import it.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return p[strings.LastIndex(p, "/")+1:]
	}
	return ""
}

// isCallOn is `pkg.fn(...)`.
func isCallOn(call *ast.CallExpr, pkg, fn string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}

func eventLiteral(call *ast.CallExpr) (string, bool) {
	if len(call.Args) != 1 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func namedErrResult(fd *ast.FuncDecl) bool {
	rs := fd.Type.Results
	if rs == nil || len(rs.List) != 1 || len(rs.List[0].Names) != 1 || rs.List[0].Names[0].Name != "err" {
		return false
	}
	id, ok := rs.List[0].Type.(*ast.Ident)
	return ok && id.Name == "error"
}

// isFinishDefer is `defer func() { finishCommand(v, err) }()`.
func isFinishDefer(st ast.Stmt, v string) bool {
	ds, ok := st.(*ast.DeferStmt)
	if !ok || len(ds.Call.Args) != 0 {
		return false
	}
	lit, ok := ds.Call.Fun.(*ast.FuncLit)
	if !ok || len(lit.Body.List) != 1 {
		return false
	}
	es, ok := lit.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "finishCommand" {
		return false
	}
	a, ok1 := call.Args[0].(*ast.Ident)
	b, ok2 := call.Args[1].(*ast.Ident)
	return ok1 && ok2 && a.Name == v && b.Name == "err"
}
