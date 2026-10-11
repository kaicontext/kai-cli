package main

// review_commit_npmdeps.go — reading the package before judging the wrapper.
//
// review_commit_depsrc.go reads the Go modules a diff moves. JavaScript and
// TypeScript had nothing, and it showed. falkordb-browser#2108 moved its UI
// wrappers onto @falkordb/ui 0.3.0, and the review posted nine line comments
// of the shape "DrawerContent drops the side prop", "DropdownMenuContent no
// longer accepts preventOutsideClose", "ToastClose no longer renders the
// icon". The maintainer answered every one "Not an issue": the package's own
// components carry those props and that behaviour, and the wrappers spread
// their props through. Kai's replies said why it got them wrong — "I don't have
// the @falkordb/ui source in this workspace". The package's type declarations
// were public, 35 KB, and named both props.
//
// So read them. For each package the diff's JS/TS files newly import from,
// resolve the version the repo pins (package-lock.json, else package.json's
// range — jsDelivr resolves ranges) and fetch the file its package.json names
// as "types". The reviewer gets it in the prompt; the challenge gets it as a
// citable source for the files that import it, which is where "the wrapper
// dropped X" is supported or refuted.
//
// WHAT IT WILL NOT DO, as with Go: only packages the added lines import, only
// public npm through jsDelivr, within hard file, byte and time caps. Anything
// it cannot read is simply not there — the reviewer's rule for unverified
// library claims still applies to it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// rcNpmFetchBudget bounds every request together; turn 0 waits on it.
	rcNpmFetchBudget = 10 * time.Second
	// rcNpmMaxPkgs bounds how many packages are read per review.
	rcNpmMaxPkgs = 3
	// rcNpmMaxBytes bounds one package's declarations in the prompt. Larger
	// files are cut to the declarations around the names the diff imports.
	rcNpmMaxBytes = 40 << 10
	// rcNpmExcerptLines is how much of a large declaration file is kept on
	// each side of a line naming an imported identifier.
	rcNpmExcerptLines = 30
)

// rcNpmCDN is the package CDN. A variable so tests can point it at a server.
var rcNpmCDN = "https://cdn.jsdelivr.net/npm"

// rcNpmDep is one package the diff newly imports from.
type rcNpmDep struct {
	Pkg   string   // "@falkordb/ui"
	Files []string // the changed files that import it
	Names []string // the identifiers they import from it, when named
}

// rcNpmSource is one package's declarations, read at the pinned version.
type rcNpmSource struct {
	Dep     rcNpmDep
	Version string // the version or range it was read at
	Path    string // the declaration file within the package
	Body    string
	Excerpt bool // cut to the declarations around the imported names
}

var (
	rcJSFileRe      = regexp.MustCompile(`\.(?:[cm]?[jt]sx?)$`)
	rcImportFromRe  = regexp.MustCompile(`(?:^|\s)(?:import|export)\s+(?:type\s+)?(?:([\w\s{},*$]+?)\s+from\s+)?['"]([^'"]+)['"]`)
	rcRequireRe     = regexp.MustCompile(`(?:require|import)\(\s*['"]([^'"]+)['"]\s*\)`)
	rcImportNamesRe = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
)

// rcNpmPackageName reduces an import specifier to its package: "@scope/name"
// or "name". Relative, absolute, node: builtins and path aliases ("@/…",
// "~/…") are not packages and return "".
func rcNpmPackageName(spec string) string {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") ||
		strings.HasPrefix(spec, "node:") || strings.HasPrefix(spec, "@/") || strings.HasPrefix(spec, "~") {
		return ""
	}
	parts := strings.Split(spec, "/")
	if strings.HasPrefix(spec, "@") {
		if len(parts) < 2 || parts[1] == "" {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// rcChangedNpmDeps lists the packages the diff's added JS/TS lines import
// from, with the files that import each and the names they bring in.
func rcChangedNpmDeps(diff string) []rcNpmDep {
	byPkg := map[string]*rcNpmDep{}
	var order []string
	file := ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if file == "" || !rcJSFileRe.MatchString(file) || !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		code := line[1:]
		var specs [][2]string // {spec, names}
		for _, m := range rcImportFromRe.FindAllStringSubmatch(code, -1) {
			specs = append(specs, [2]string{m[2], m[1]})
		}
		for _, m := range rcRequireRe.FindAllStringSubmatch(code, -1) {
			specs = append(specs, [2]string{m[1], ""})
		}
		for _, s := range specs {
			pkg := rcNpmPackageName(s[0])
			if pkg == "" {
				continue
			}
			d := byPkg[pkg]
			if d == nil {
				d = &rcNpmDep{Pkg: pkg}
				byPkg[pkg] = d
				order = append(order, pkg)
			}
			if !rcContains(d.Files, file) {
				d.Files = append(d.Files, file)
			}
			for _, n := range rcImportNamesRe.FindAllString(s[1], -1) {
				if n != "type" && n != "as" && !rcContains(d.Names, n) {
					d.Names = append(d.Names, n)
				}
			}
		}
	}
	out := make([]rcNpmDep, 0, len(order))
	for _, p := range order {
		out = append(out, *byPkg[p])
	}
	// The package the change is built around first: rcFetchNpmSources reads
	// only a few, and on #2108 the first import seen was an icon set while
	// thirteen files had moved onto @falkordb/ui.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Files) > len(out[j].Files) })
	return out
}

func rcContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// rcNpmVersion finds the version the repo pins for pkg, walking up from the
// importing file's directory to root: package-lock.json's exact version first,
// else the range in package.json's dependencies. "" when it pins none.
func rcNpmVersion(root, file, pkg string) string {
	dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(file)))
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "package-lock.json")); err == nil {
			var lock struct {
				Packages map[string]struct {
					Version string `json:"version"`
				} `json:"packages"`
			}
			if json.Unmarshal(b, &lock) == nil {
				if p, ok := lock.Packages["node_modules/"+pkg]; ok && p.Version != "" {
					return p.Version
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			var pj struct {
				Dependencies, DevDependencies, PeerDependencies map[string]string
			}
			if json.Unmarshal(b, &pj) == nil {
				for _, m := range []map[string]string{pj.Dependencies, pj.DevDependencies, pj.PeerDependencies} {
					if v := strings.TrimSpace(m[pkg]); v != "" && !strings.ContainsAny(v, ":/") {
						return v // a range or version; "workspace:", "file:", git URLs are not fetchable
					}
				}
			}
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return ""
		}
		dir = filepath.Dir(dir)
	}
}

// rcNpmGet fetches one URL within ctx, at most limit bytes.
func rcNpmGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// rcNpmTypesPath reads which declaration file a package names: "types",
// "typings", or exports["."].types.
func rcNpmTypesPath(pkgJSON []byte) string {
	var pj struct {
		Types   string          `json:"types"`
		Typings string          `json:"typings"`
		Exports json.RawMessage `json:"exports"`
	}
	if json.Unmarshal(pkgJSON, &pj) != nil {
		return ""
	}
	if pj.Types != "" {
		return pj.Types
	}
	if pj.Typings != "" {
		return pj.Typings
	}
	var exp map[string]json.RawMessage
	if json.Unmarshal(pj.Exports, &exp) == nil {
		var dot struct {
			Types string `json:"types"`
		}
		if json.Unmarshal(exp["."], &dot) == nil && dot.Types != "" {
			return dot.Types
		}
	}
	return ""
}

// rcNpmExcerpt keeps the declarations around the lines that name any of
// names, so a large file still yields the contracts the diff depends on.
func rcNpmExcerpt(body string, names []string) string {
	lines := strings.Split(body, "\n")
	keep := make([]bool, len(lines))
	any := false
	for i, l := range lines {
		for _, n := range names {
			if strings.Contains(l, n) {
				for j := max(0, i-rcNpmExcerptLines); j < min(len(lines), i+rcNpmExcerptLines+1); j++ {
					keep[j] = true
				}
				any = true
				break
			}
		}
	}
	if !any {
		return ""
	}
	var b strings.Builder
	gap := false
	for i, l := range lines {
		if !keep[i] {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteString("// …\n")
		}
		gap = false
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// rcFetchNpmSources reads the declarations of the packages the diff imports.
func rcFetchNpmSources(ctx context.Context, root string, deps []rcNpmDep) []rcNpmSource {
	ctx, cancel := context.WithTimeout(ctx, rcNpmFetchBudget)
	defer cancel()
	var got []rcNpmSource
	for _, d := range deps {
		if len(got) >= rcNpmMaxPkgs || ctx.Err() != nil {
			break
		}
		ver := rcNpmVersion(root, d.Files[0], d.Pkg)
		if ver == "" {
			continue
		}
		base := fmt.Sprintf("%s/%s@%s", strings.TrimRight(rcNpmCDN, "/"), d.Pkg, ver)
		pj, err := rcNpmGet(ctx, base+"/package.json", 256<<10)
		if err != nil {
			continue
		}
		tp := strings.TrimPrefix(path.Clean("/"+rcNpmTypesPath(pj)), "/")
		if tp == "" || tp == "." {
			continue
		}
		body, err := rcNpmGet(ctx, base+"/"+tp, 4<<20)
		if err != nil || len(body) == 0 {
			continue
		}
		src := rcNpmSource{Dep: d, Version: ver, Path: tp, Body: string(body)}
		if len(src.Body) > rcNpmMaxBytes {
			ex := rcNpmExcerpt(src.Body, d.Names)
			if ex == "" || len(ex) > rcNpmMaxBytes {
				continue // too big to inline and nothing to narrow it by
			}
			src.Body, src.Excerpt = ex, true
		}
		got = append(got, src)
	}
	return got
}

// rcNpmSourceBlock renders the declarations into the reviewer's prompt.
func rcNpmSourceBlock(got []rcNpmSource) string {
	if len(got) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("TYPE DECLARATIONS OF THE PACKAGES THIS DIFF IMPORTS (fetched at the version the repo pins\n")
	b.WriteString("before this review started — treat as already read). A wrapper that delegates to one of\n")
	b.WriteString("these components inherits what it declares: before saying the change drops a prop, a default\n")
	b.WriteString("or a behaviour, check that the imported component does not provide it here.\n\n")
	for _, s := range got {
		cut := ""
		if s.Excerpt {
			cut = ", the declarations around the imported names"
		}
		fmt.Fprintf(&b, "--- %s@%s/%s (imported by %s%s) ---\n", s.Dep.Pkg, s.Version, s.Path, strings.Join(s.Dep.Files, ", "), cut)
		b.WriteString(strings.TrimRight(s.Body, "\n"))
		b.WriteString("\n\n")
	}
	return b.String()
}

// rcNpmChallengeSources hands the declarations to the challenge, each tied to
// the files that import it so a batch about those files receives it.
func rcNpmChallengeSources(got []rcNpmSource) []rcSource {
	out := make([]rcSource, 0, len(got))
	for _, s := range got {
		files := append([]string(nil), s.Dep.Files...)
		sort.Strings(files)
		out = append(out, rcSource{
			Text:     fmt.Sprintf("// %s@%s/%s — type declarations of the package, imported by %s\n%s", s.Dep.Pkg, s.Version, s.Path, strings.Join(files, ", "), s.Body),
			Coord:    rcCoordRows,
			ForPaths: files,
		})
	}
	return out
}
