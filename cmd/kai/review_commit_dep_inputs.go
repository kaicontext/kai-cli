package main

import (
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rcReviewDeps includes unchanged modules used by changed Go files: a change
// to a call site often needs the dependency contract without changing go.mod.
// Replaced modules and nested modules are deliberately not resolved here.
func rcReviewDeps(root, diff string, changed []string) []rcDepChange {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return rcChangedDeps(diff)
	}
	for _, line := range strings.Split(string(mod), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "replace" {
			return nil
		}
	}
	var imports strings.Builder
	for i, name := range changed {
		if i >= 40 {
			break
		}
		if !strings.HasSuffix(name, ".go") || filepath.IsAbs(name) {
			continue
		}
		clean := filepath.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		nested := false
		for dir := filepath.Dir(clean); dir != "."; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(root, dir, "go.mod")); err == nil {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		f, err := os.Open(filepath.Join(root, clean))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 256<<10))
		f.Close()
		if err != nil {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, imp := range parsed.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			imports.WriteString(strconv.Quote(p) + "\n")
		}
	}
	// Reuse the bounded require/import scanner, using HEAD's pins instead of
	// guessing dependency versions from call sites. No go toolchain is needed.
	synthetic := "diff --git a/go.mod b/go.mod\n+++ b/go.mod\n+" + strings.ReplaceAll(string(mod), "\n", "\n+") + "\ndiff --git a/imports b/imports\n" + imports.String()
	deps := rcChangedDeps(synthetic)
	var used []rcDepChange
	for _, d := range deps {
		if len(d.Pkgs) > 0 {
			used = append(used, d)
		}
	}
	return used
}
