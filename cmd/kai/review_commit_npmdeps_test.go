package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNpmPackageName(t *testing.T) {
	for spec, want := range map[string]string{
		"@falkordb/ui":          "@falkordb/ui",
		"@falkordb/ui/base.css": "@falkordb/ui",
		"react":                 "react",
		"lodash/merge":          "lodash",
		"./drawer":              "",
		"../lib/cn":             "",
		"@/components/ui":       "",
		"~/lib":                 "",
		"node:fs":               "",
		"@scope":                "",
	} {
		if got := rcNpmPackageName(spec); got != want {
			t.Errorf("%q -> %q, want %q", spec, got, want)
		}
	}
}

// falkordb-browser#2108's shape: wrappers moved onto @falkordb/ui.
const rcNpmTestDiff = `diff --git a/components/ui/drawer.tsx b/components/ui/drawer.tsx
--- a/components/ui/drawer.tsx
+++ b/components/ui/drawer.tsx
@@ -1,3 +1,4 @@
-import * as DrawerPrimitive from "vaul"
+import { DrawerContent as UIDrawerContent, type DrawerContentProps } from "@falkordb/ui"
+import { cn } from "@/lib/utils"
 import React from "react"
diff --git a/components/ui/dropdown-menu.tsx b/components/ui/dropdown-menu.tsx
--- a/components/ui/dropdown-menu.tsx
+++ b/components/ui/dropdown-menu.tsx
@@ -1,2 +1,3 @@
+import { DropdownMenuContent } from '@falkordb/ui'
+const x = require("clsx")
 import "./local.css"
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1,2 @@
+import { Nope } from "not-code"
`

func TestChangedNpmDepsReadsOnlyAddedImportsInCode(t *testing.T) {
	deps := rcChangedNpmDeps(rcNpmTestDiff)
	byPkg := map[string]rcNpmDep{}
	for _, d := range deps {
		byPkg[d.Pkg] = d
	}
	ui, ok := byPkg["@falkordb/ui"]
	if !ok {
		t.Fatalf("deps = %+v, want @falkordb/ui", deps)
	}
	if len(ui.Files) != 2 {
		t.Errorf("@falkordb/ui files = %v, want both wrappers", ui.Files)
	}
	for _, n := range []string{"DrawerContent", "UIDrawerContent", "DrawerContentProps", "DropdownMenuContent"} {
		if !rcContains(ui.Names, n) {
			t.Errorf("@falkordb/ui names %v lack %s", ui.Names, n)
		}
	}
	if deps[0].Pkg != "@falkordb/ui" {
		t.Errorf("first dep = %s, want the package most changed files import", deps[0].Pkg)
	}
	if _, ok := byPkg["clsx"]; !ok {
		t.Error("require() was not read")
	}
	for _, not := range []string{"vaul", "react", "not-code"} {
		if _, ok := byPkg[not]; ok {
			t.Errorf("%s was picked up (removed line, context line, or not a code file)", not)
		}
	}
}

func TestNpmVersionPrefersTheLockfileAndWalksUp(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"dependencies":{"@falkordb/ui":"^0.3.0","local":"workspace:*"}}`)
	if v := rcNpmVersion(root, "components/ui/drawer.tsx", "@falkordb/ui"); v != "^0.3.0" {
		t.Errorf("range from package.json = %q", v)
	}
	if v := rcNpmVersion(root, "components/ui/drawer.tsx", "local"); v != "" {
		t.Errorf("a workspace: dependency is not fetchable, got %q", v)
	}
	write("package-lock.json", `{"packages":{"node_modules/@falkordb/ui":{"version":"0.3.0"}}}`)
	if v := rcNpmVersion(root, "components/ui/drawer.tsx", "@falkordb/ui"); v != "0.3.0" {
		t.Errorf("exact version from the lockfile = %q", v)
	}
	if v := rcNpmVersion(root, "components/ui/drawer.tsx", "absent"); v != "" {
		t.Errorf("a package the repo does not pin = %q", v)
	}
}

// The fetch, against a CDN that serves @falkordb/ui the way jsDelivr does.
func TestFetchNpmSourcesReadsTheDeclaredTypes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"@falkordb/ui":"0.3.0","big":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	decl := "export declare interface DrawerContentProps {\n    side?: \"left\" | \"right\" | \"bottom\";\n}\n"
	var big strings.Builder
	for i := 0; i < 4000; i++ {
		big.WriteString("export declare const filler" + strings.Repeat("x", 10) + ": number;\n")
	}
	big.WriteString("export declare const Wanted: (p: { keep?: boolean }) => null;\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@falkordb/ui@0.3.0/package.json":
			w.Write([]byte(`{"exports":{".":{"types":"./dist/index.d.ts"}}}`))
		case "/@falkordb/ui@0.3.0/dist/index.d.ts":
			w.Write([]byte(decl))
		case "/big@1.0.0/package.json":
			w.Write([]byte(`{"types":"index.d.ts"}`))
		case "/big@1.0.0/index.d.ts":
			w.Write([]byte(big.String()))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := rcNpmCDN
	rcNpmCDN = srv.URL
	defer func() { rcNpmCDN = old }()

	got := rcFetchNpmSources(context.Background(), root, []rcNpmDep{
		{Pkg: "@falkordb/ui", Files: []string{"components/ui/drawer.tsx"}, Names: []string{"DrawerContent"}},
		{Pkg: "big", Files: []string{"a.ts"}, Names: []string{"Wanted"}},
		{Pkg: "unpinned", Files: []string{"a.ts"}},
	})
	if len(got) != 2 {
		t.Fatalf("read %d packages, want 2 (the unpinned one skipped)", len(got))
	}
	if got[0].Path != "dist/index.d.ts" || !strings.Contains(got[0].Body, `side?: "left" | "right" | "bottom"`) || got[0].Excerpt {
		t.Errorf("@falkordb/ui source = %+v", got[0])
	}
	if !got[1].Excerpt || !strings.Contains(got[1].Body, "Wanted") || len(got[1].Body) > rcNpmMaxBytes {
		t.Errorf("a large declaration file must be cut to the imported names: excerpt=%v len=%d", got[1].Excerpt, len(got[1].Body))
	}

	block := rcNpmSourceBlock(got)
	if !strings.Contains(block, "@falkordb/ui@0.3.0/dist/index.d.ts (imported by components/ui/drawer.tsx)") || !strings.Contains(block, "inherits what it declares") {
		t.Errorf("prompt block:\n%s", block[:min(600, len(block))])
	}
	srcs := rcNpmChallengeSources(got)
	if len(srcs) != 2 || !rcContains(srcs[0].ForPaths, "components/ui/drawer.tsx") || srcs[0].Coord != rcCoordRows {
		t.Errorf("challenge sources = %+v", srcs)
	}
}

// A batch about a wrapper receives the package's declarations; a batch about
// an unrelated file does not.
func TestBatchSourcesRouteDeclarationsToTheFilesThatImportThem(t *testing.T) {
	decl := rcSource{Text: "// @falkordb/ui types", Coord: rcCoordRows, ForPaths: []string{"components/ui/drawer.tsx"}}
	in := rcBatchSources(nil, []rcSource{decl}, []string{"components/ui/drawer.tsx:30 — `DrawerContent` drops the `side` prop"})
	if len(in) != 1 {
		t.Errorf("the wrapper's batch did not get the declarations: %v", in)
	}
	out := rcBatchSources(nil, []rcSource{decl}, []string{"lib/api.ts:4 — the token is logged"})
	if len(out) != 0 {
		t.Errorf("an unrelated batch got the declarations: %v", out)
	}
}

func TestPromptsSayAWrapperIsNotItsDependency(t *testing.T) {
	if !strings.Contains(rcReviewSystem, "A WRAPPER IS NOT ITS DEPENDENCY") {
		t.Error("the reviewer prompt lacks the wrapper rule")
	}
	if !strings.Contains(rcChallengeSystemHead, "A WRAPPER IS NOT ITS DEPENDENCY") {
		t.Error("the challenge prompt lacks the wrapper rule")
	}
}
