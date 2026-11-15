package main

// review_commit_rescope_chain.go — a scoped re-review also reads what the
// change since the previous review can break.
//
// A scoped re-review (review_commit_rescope.go) is sized to the FOCUS: the
// files changed since the previous review, plus the files earlier points name.
// The prompt asked the reviewer to follow each change into its callers, but
// whether it did depended on the turns it had left, and a fix that breaks an
// unchanged caller is exactly the defect a re-review exists to catch.
//
// So the files around the change are worked out before the review starts and
// handed to the reviewer by name, with the reason each is there. In priority
// order, most likely to break first:
//
//  1. symbol callers: the call sites of each function or method the change
//     since the previous review touched or removed, from the code graph's
//     call-site nodes (matched on the callee's own file, so a same-named
//     function elsewhere is not counted);
//  2. key users: files that use a string key the change touched or removed
//     (an event or metric name, a route, a setting, a map key) — coupling the
//     graph has no edge for, found with one bounded git grep;
//  3. file callers: files that call into or import a focus file. Only for a
//     focus file whose change the symbol lookup could not cover (a type
//     change, or a symbol with no recorded callers): for the rest they are
//     every user of the file, mostly unrelated to what changed;
//  4. tests of a focus file;
//  5. uses: files a focus file calls into or imports.
//
// These RELATED files are read for impact, not reviewed in full: one extra
// turn per two related files, under the same ceiling, and outside the
// coverage gate. A related file may be outside the pull request; an unchanged
// caller is the point. Related files that ARE in the pull request also go to
// both sweeps, beside the focus files.
//
// The list is one hop deep and capped (rcChainDefaultMax). Focus files take
// turns within each kind, so one widely used file cannot fill the list alone.
// KAI_REVIEW_RESCOPE_CHAIN sets the cap; off (or 0) turns chaining off. With
// no focus (a full review) nothing is chained.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/graph"
)

const (
	// rcChainDefaultMax is how many related files a re-review gets by
	// default: enough for the callers and tests of a few changed files,
	// small next to a full review's budget.
	rcChainDefaultMax = 12
	// rcChainFilesPerTurn is how many related files buy one extra turn.
	rcChainFilesPerTurn = 2
	// rcChainMaxKeys bounds how many changed string keys are searched for.
	rcChainMaxKeys = 15
	// rcChainSitesShown is the most call sites named for one related file.
	rcChainSitesShown = 3
)

// rcChainKind is why a related file is in the list; the order is the
// priority.
type rcChainKind int

const (
	rcChainSymbol rcChainKind = iota
	rcChainKey
	rcChainCaller
	rcChainTest
	rcChainUse
	rcChainKinds
)

// rcChained is one related file and why it is read.
type rcChained struct {
	Path    string
	Kind    rcChainKind
	Of      string // the focus file it relates to
	What    string // the changed symbol or key (symbol and key kinds)
	Removed bool   // the symbol or key is gone from the focus file
	Lines   []int  // where in Path (symbol and key kinds), when known
}

func (c rcChained) reason() string {
	gone := "changed"
	if c.Removed {
		gone = "removed"
	}
	at := ""
	if len(c.Lines) > 0 {
		shown := c.Lines
		if len(shown) > rcChainSitesShown {
			shown = shown[:rcChainSitesShown]
		}
		parts := make([]string, len(shown))
		for i, l := range shown {
			parts[i] = strconv.Itoa(l)
		}
		at = " at line " + strings.Join(parts, ", ")
		if len(c.Lines) > len(shown) {
			at += fmt.Sprintf(" (+%d more)", len(c.Lines)-len(shown))
		}
	}
	switch c.Kind {
	case rcChainSymbol:
		return fmt.Sprintf("calls %s (%s in %s)%s", c.What, gone, c.Of, at)
	case rcChainKey:
		return fmt.Sprintf("uses key %q (%s in %s)%s", c.What, gone, c.Of, at)
	case rcChainCaller:
		return "calls into or imports " + c.Of
	case rcChainTest:
		return "tests " + c.Of
	default:
		return "used by " + c.Of
	}
}

// rcChangedSym is a function, method or type the change since the previous
// review touched or removed.
type rcChangedSym struct {
	Name    string
	Type    bool // a type, class or interface: its users are not call sites
	Removed bool
}

// rcChangedKey is a string key the change since the previous review touched,
// with where else it is used.
type rcChangedKey struct {
	Key     string
	Removed bool
	Uses    map[string][]int // path → lines, outside the focus file
}

// rcSite is one call site.
type rcSite struct {
	Path string
	Line int
}

// rcChainInputs is what the change since the previous review touched, per
// focus file.
type rcChainInputs struct {
	Symbols map[string][]rcChangedSym
	Keys    map[string][]rcChangedKey
	// Code: the focus files whose code changed since the previous review
	// (not only comments). Whole-file callers, tests and uses are chained
	// only for these: a file in the focus because an earlier point names it,
	// or whose change is a comment, has nothing new for them to break. Nil
	// chains them for every focus file.
	Code map[string]bool
}

// rcChainGraph answers the questions chaining asks of the code graph. Each
// returns repository-relative paths; nil when it cannot tell.
type rcChainGraph interface {
	SymbolCallers(path, symbol string) []rcSite
	Callers(path string) []string
	Tests(path string) []string
	Uses(path string) []string
}

// rcReReviewChain returns the related files of a scoped re-review's focus, at
// most max, in priority order. Focus files themselves are never listed. nil
// when there is no focus or chaining is off.
func rcReReviewChain(focus []string, in rcChainInputs, g rcChainGraph, max int) []rcChained {
	if focus == nil || max <= 0 {
		return nil
	}
	inFocus := make(map[string]bool, len(focus))
	for _, f := range focus {
		inFocus[f] = true
	}

	// One candidate queue per kind per focus file, built lazily so a capped
	// list does not query the graph for files it will never take.
	queues := make([][rcChainKinds][]rcChained, len(focus))
	built := make([][rcChainKinds]bool, len(focus))
	// symbolsCovered[i]: focus file i's change is fully described by call
	// sites, so its whole-file callers are left out.
	symbolsCovered := make([]bool, len(focus))
	// A test file calls what it tests, so the graph lists it among the
	// callers too; it is moved to the tests.
	testCallers := make([][]string, len(focus))

	var build func(i int, k rcChainKind) []rcChained
	build = func(i int, k rcChainKind) []rcChained {
		if built[i][k] {
			return queues[i][k]
		}
		built[i][k] = true
		f := focus[i]
		var q []rcChained
		switch k {
		case rcChainSymbol:
			if g == nil {
				break
			}
			syms := in.Symbols[f]
			found, onlyCalls := false, len(syms) > 0
			for _, s := range syms {
				if s.Type {
					onlyCalls = false
					continue
				}
				byPath := map[string][]int{}
				var order []string
				for _, site := range g.SymbolCallers(f, s.Name) {
					if site.Path == "" || site.Path == f {
						continue
					}
					if _, ok := byPath[site.Path]; !ok {
						order = append(order, site.Path)
					}
					if site.Line > 0 {
						byPath[site.Path] = append(byPath[site.Path], site.Line)
					} else if byPath[site.Path] == nil {
						byPath[site.Path] = []int{}
					}
				}
				if len(order) == 0 {
					onlyCalls = false // nothing recorded: fall back to file callers
					continue
				}
				found = true
				sort.Strings(order)
				for _, p := range order {
					lines := byPath[p]
					sort.Ints(lines)
					q = append(q, rcChained{Path: p, Kind: k, Of: f, What: s.Name, Removed: s.Removed, Lines: lines})
				}
			}
			// Production call sites before test ones: a test that calls the
			// changed function matters less than code that does.
			sort.SliceStable(q, func(a, b int) bool { return !rcIsTestPath(q[a].Path) && rcIsTestPath(q[b].Path) })
			symbolsCovered[i] = found && onlyCalls
		case rcChainKey:
			for _, key := range in.Keys[f] {
				paths := make([]string, 0, len(key.Uses))
				for p := range key.Uses {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				for _, p := range paths {
					q = append(q, rcChained{Path: p, Kind: k, Of: f, What: key.Key, Removed: key.Removed, Lines: key.Uses[p]})
				}
			}
		case rcChainCaller:
			build(i, rcChainSymbol) // decides symbolsCovered
			if g == nil || (in.Code != nil && !in.Code[f]) {
				break
			}
			callers := append([]string(nil), g.Callers(f)...)
			sort.Strings(callers)
			for _, p := range callers {
				if rcIsTestPath(p) {
					testCallers[i] = append(testCallers[i], p)
				} else if !symbolsCovered[i] {
					q = append(q, rcChained{Path: p, Kind: k, Of: f})
				}
			}
		case rcChainTest:
			build(i, rcChainCaller) // collects the test files listed as callers
			if in.Code != nil && !in.Code[f] {
				break
			}
			var tests []string
			if g != nil {
				tests = g.Tests(f)
			}
			tests = append(append([]string(nil), tests...), testCallers[i]...)
			sort.Strings(tests)
			for _, p := range tests {
				q = append(q, rcChained{Path: p, Kind: k, Of: f})
			}
		default:
			if g == nil || (in.Code != nil && !in.Code[f]) {
				break
			}
			uses := append([]string(nil), g.Uses(f)...)
			sort.Strings(uses)
			for _, p := range uses {
				q = append(q, rcChained{Path: p, Kind: k, Of: f})
			}
		}
		queues[i][k] = q
		return q
	}

	taken := map[string]bool{}
	var out []rcChained
	for k := rcChainKind(0); k < rcChainKinds; k++ {
		// Round-robin over the focus files: each takes one of this kind in
		// turn until all are exhausted or the cap is reached.
		for {
			progressed := false
			for i := range focus {
				q := build(i, k)
				for len(q) > 0 {
					c := q[0]
					q = q[1:]
					if c.Path == "" || inFocus[c.Path] || taken[c.Path] || rcChainSkip(c.Path) {
						continue
					}
					taken[c.Path] = true
					out = append(out, c)
					progressed = true
					break
				}
				queues[i][k] = q
				if len(out) >= max {
					return out
				}
			}
			if !progressed {
				break
			}
		}
	}
	return out
}

// rcChainSkip leaves out vendored and generated trees: reading them for
// impact is never worth a turn.
func rcChainSkip(p string) bool {
	for _, dir := range []string{"vendor/", "node_modules/", "third_party/", "dist/", "build/"} {
		if strings.HasPrefix(p, dir) || strings.Contains(p, "/"+dir) {
			return true
		}
	}
	return false
}

// rcChainMax is the cap KAI_REVIEW_RESCOPE_CHAIN sets: a number, or off / 0
// for none. Unset or unreadable keeps the default.
func rcChainMax() int {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("KAI_REVIEW_RESCOPE_CHAIN")))
	switch v {
	case "":
		return rcChainDefaultMax
	case "off", "false", "no":
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return rcChainDefaultMax
}

// rcReReviewTurns is a scoped re-review's turn budget: the focus's, plus one
// turn per rcChainFilesPerTurn related files, under the same ceiling.
func rcReReviewTurns(scope int, chained int) int {
	n := rcReviewMaxTurns(scope)
	if chained > 0 {
		n += (chained + rcChainFilesPerTurn - 1) / rcChainFilesPerTurn
	}
	if n > rcReviewTurnCeiling {
		return rcReviewTurnCeiling
	}
	return n
}

// rcSweepFocus is what the sweeps read on a scoped re-review: the focus, plus
// the related files that are in the pull request (the sweeps only ever see
// the pull request's own hunks). nil — the whole change — stays nil.
func rcSweepFocus(focus []string, chain []rcChained) []string {
	if focus == nil {
		return nil
	}
	// make, not append onto nil: an empty focus (a re-check with no file to
	// read) must stay non-nil, or the sweeps would read the whole change.
	out := make([]string, len(focus), len(focus)+len(chain))
	copy(out, focus)
	for _, c := range chain {
		out = append(out, c.Path)
	}
	return out
}

// rcReReviewChainBlock names the related files in the prompt, after the
// RE-REVIEW FOCUS block. "" when there are none.
func rcReReviewChainBlock(chain []rcChained) string {
	if len(chain) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("RE-REVIEW RELATED FILES (worked out before this review from the change since the previous review: ")
	b.WriteString("call sites of the functions it changed or removed, users of the string keys it changed, then tests and dependencies; one hop). ")
	b.WriteString("After the focus files, open each of these at the lines given and check it against what changed: ")
	b.WriteString("a caller still passing old arguments or relying on an old return, error or nil case; ")
	b.WriteString("a key read or written under a name that no longer matches; a test asserting the old behaviour. ")
	b.WriteString("They are read for impact, not reviewed line by line, and some are outside this pull request:\n")
	for _, c := range chain {
		fmt.Fprintf(&b, "- %s — %s\n", c.Path, c.reason())
	}
	b.WriteString("\n")
	return b.String()
}

// --- what changed since the previous review ----------------------------------

// rcSinceDiff is the zero-context diff of the focus files between the previous
// review's commit and HEAD; "" when git cannot produce it. Tests replace it.
var rcSinceDiff = func(head, root string, focus []string) string {
	if head == "" || len(focus) == 0 {
		return ""
	}
	// The pull request's own new edits: when the base moved, the previously
	// reviewed commit replayed onto the new base, against HEAD. Commit to
	// commit, the diff would also hold every base change to these files, and
	// their functions would be chained as if the pull request had touched
	// them. If the replay is not possible (a conflict, an older git), the
	// commit-to-commit diff stands.
	from := head
	if tree := rcReplayOnNewBase(head, root); tree != "" {
		from = tree
	}
	paths := make([]string, len(focus))
	for i, f := range focus {
		paths[i] = rcGitUnquote(f) // a pathspec is the real path
	}
	args := append([]string{"-c", "core.quotePath=false", "--no-pager", "diff", "-U0", "--no-color", "--no-ext-diff", from, "HEAD", "--"}, paths...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = 2 * time.Second
	// The focus paths are relative to the repository root; as pathspecs they
	// are read relative to the working directory, which may be below it.
	if root != "" {
		cmd.Dir = root
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ""
	}
	if err := cmd.Start(); err != nil {
		return ""
	}
	out, _ := io.ReadAll(io.LimitReader(stdout, rcSinceDiffLimit))
	// At the cap git may be blocked writing the rest: stop it rather than
	// wait for the deadline. What was read is used.
	cancel()
	_ = cmd.Wait()
	return string(out)
}

// rcReplayOnNewBase is the tree of head's pull request replayed onto the
// base HEAD sits on, when the base moved since head; "" when it did not, the
// base is unknown, or the replay conflicts. git merge-tree --write-tree
// writes objects only: no ref, index or working-tree change.
func rcReplayOnNewBase(head, root string) string {
	if strings.TrimSpace(reviewCommitBase) == "" {
		return ""
	}
	oldBase, newBase := rcMergeBase(head, reviewCommitBase), rcMergeBase("HEAD", reviewCommitBase)
	if oldBase == "" || newBase == "" || oldBase == newBase {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--no-messages", newBase, head)
	cmd.WaitDelay = 2 * time.Second
	if root != "" {
		cmd.Dir = root
	}
	out, err := cmd.Output()
	if err != nil { // exit 1 is a conflict; anything else, no replay
		return ""
	}
	tree := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(tree) < 40 {
		return ""
	}
	return tree
}

// rcSinceDiffLimit caps the since-diff read; tests lower it.
var rcSinceDiffLimit int64 = 4 << 20

// rcDiffFiles walks a diff and calls fn for each line with the file it belongs
// to (the new path; the old one for a deleted file).
func rcDiffFiles(diff string, fn func(file, line string)) {
	file, oldFile := "", ""
	// "---"/"+++" are header lines only before a file's first "@@"; in the
	// body they are a removed "-- x" or an added "++ x".
	inBody := false
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			file, oldFile, inBody = "", "", false
			continue
		}
		if !inBody {
			switch {
			case strings.HasPrefix(line, "--- "):
				oldFile = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
				continue
			case strings.HasPrefix(line, "+++ "):
				file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
				if file == "/dev/null" {
					file = oldFile
				}
				continue
			case strings.HasPrefix(line, "@@"):
				inBody = true
			}
		}
		if file != "" && file != "/dev/null" {
			fn(file, line)
		}
	}
}

// rcDeclName is the name a declaration line declares, and whether it is a
// type; "" when the line declares nothing.
func rcDeclName(t string) (string, bool) {
	t = strings.TrimSpace(t)
	for again := true; again; {
		again = false
		for _, p := range []string{"export default ", "export ", "async ", "pub(crate) ", "pub ", "public ", "private ", "protected ", "static ", "abstract "} {
			if strings.HasPrefix(t, p) {
				t, again = strings.TrimPrefix(t, p), true
			}
		}
	}
	for _, kw := range []struct {
		kw  string
		typ bool
	}{{"func ", false}, {"function ", false}, {"def ", false}, {"fn ", false},
		{"type ", true}, {"class ", true}, {"interface ", true}, {"struct ", true}, {"trait ", true}, {"enum ", true}} {
		if !strings.HasPrefix(t, kw.kw) {
			continue
		}
		rest := strings.TrimSpace(t[len(kw.kw):])
		// Go method receivers: skip "(r *T) " to the method name.
		if kw.kw == "func " && strings.HasPrefix(rest, "(") {
			if i := strings.Index(rest, ")"); i >= 0 {
				rest = strings.TrimSpace(rest[i+1:])
			}
		}
		name := rest
		for i, c := range name {
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				name = name[:i]
				break
			}
		}
		return name, kw.typ
	}
	return "", false
}

// rcSinceSymbols lists, per file, the functions, methods and types a
// zero-context diff touched: those declared on a changed line, and those
// whose body a hunk falls in (the hunk header names the enclosing function).
// A name declared only on removed lines is marked removed.
func rcSinceSymbols(diff string) map[string][]rcChangedSym {
	type acc struct {
		typ, added, removed, touched bool
	}
	per := map[string]map[string]*acc{}
	var order = map[string][]string{}
	note := func(file, name string, typ bool, how byte) {
		if name == "" {
			return
		}
		m := per[file]
		if m == nil {
			m = map[string]*acc{}
			per[file] = m
		}
		a := m[name]
		if a == nil {
			a = &acc{}
			m[name] = a
			order[file] = append(order[file], name)
		}
		a.typ = a.typ || typ
		switch how {
		case '+':
			a.added = true
		case '-':
			a.removed = true
		default:
			a.touched = true
		}
	}
	// The enclosing function a hunk header names counts only if the hunk
	// changes code: a hunk of comments and blank lines touches nothing a
	// caller can see.
	hunkName, hunkTyp := "", false
	rcDiffFiles(diff, func(file, line string) {
		if rcProseFile(file) {
			return // a README's "class Foo handles…" declares nothing
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			// "@@ -10,3 +10,4 @@ func (r *T) Foo(x int) {"
			hunkName = ""
			if i := strings.Index(line[2:], "@@"); i >= 0 {
				hunkName, hunkTyp = rcDeclName(line[2+i+2:])
			}
		case len(line) > 1 && (line[0] == '+' || line[0] == '-'):
			if hunkName != "" && !rcCommentOrBlank(file, line[1:]) {
				note(file, hunkName, hunkTyp, ' ') // its first code line
				hunkName = ""
			}
			name, typ := rcDeclName(line[1:])
			note(file, name, typ, line[0])
		}
	})
	out := map[string][]rcChangedSym{}
	for file, names := range order {
		for _, n := range names {
			a := per[file][n]
			out[file] = append(out[file], rcChangedSym{Name: n, Type: a.typ, Removed: a.removed && !a.added && !a.touched})
		}
	}
	return out
}

// rcSinceCode lists the files whose changed lines include code, not only
// comments and blank lines.
func rcSinceCode(diff string) map[string]bool {
	out := map[string]bool{}
	rcDiffFiles(diff, func(file, line string) {
		// rcDiffFiles passes body lines only, so "+++i;" here is an added "++i;".
		if len(line) > 1 && (line[0] == '+' || line[0] == '-') && !rcCommentOrBlank(file, line[1:]) {
			out[file] = true
		}
	})
	return out
}

// rcCommentOrBlank reports whether a changed line of file is blank or only a
// comment. Comment syntax is per language: "#" starts a comment in Python
// and shell but code in C (#include) and Rust (#[derive]); "--" a comment in
// SQL and Lua but code in C (--i); "*" a block-comment continuation but also
// a Go or C dereference (*p = v). A language not listed is taken as code, so
// a change in it still chains its callers.
func rcCommentOrBlank(file, l string) bool {
	t := strings.TrimSpace(l)
	if t == "" {
		return true
	}
	for _, p := range rcCommentLeads(file) {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	// A block-comment continuation: "* text", a lone "*", or "*/". Not "*p".
	if rcHasBlockComments(file) && (t == "*" || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "*/")) {
		return true
	}
	return false
}

// rcProseFile reports whether file is prose, where a line reading like a
// declaration is a sentence.
func rcProseFile(file string) bool {
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(file)), ".") {
	case "md", "markdown", "txt", "rst", "adoc":
		return true
	}
	return false
}

// rcCommentLeads are the line-comment and comment-opening prefixes of
// file's language; nil when it is not known.
func rcCommentLeads(file string) []string {
	base := strings.ToLower(filepath.Base(file))
	switch base {
	case "makefile", "dockerfile", "gemfile", "rakefile", "procfile", ".gitignore", ".dockerignore":
		return []string{"#"}
	}
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(base)), ".") {
	case "go", "c", "h", "cc", "cpp", "cxx", "hpp", "hh", "m", "mm", "java", "kt", "kts", "scala",
		"swift", "js", "mjs", "cjs", "jsx", "ts", "tsx", "cs", "rs", "dart", "groovy", "gradle", "proto", "zig":
		return []string{"//", "/*"}
	case "css", "scss", "less":
		return []string{"/*", "//"}
	case "py", "pyi", "rb", "sh", "bash", "zsh", "fish", "pl", "pm", "r", "yaml", "yml", "toml",
		"ini", "cfg", "conf", "tf", "tfvars", "nix", "cmake", "ps1", "jl", "ex", "exs", "cr", "mk", "dockerfile":
		return []string{"#"}
	case "php":
		return []string{"//", "/*", "# "} // "#[" is an attribute
	case "sql", "lua", "hs", "elm", "ada", "adb", "ads", "vhd", "vhdl":
		return []string{"--"}
	case "html", "htm", "xml", "svg", "md", "markdown", "vue", "svelte":
		return []string{"<!--"}
	case "clj", "cljs", "lisp", "el", "scm":
		return []string{";"}
	case "erl", "hrl", "tex":
		return []string{"%"}
	case "vim":
		return []string{"\""}
	}
	return nil
}

// rcHasBlockComments reports whether file's language has /* */ comments,
// whose continuation lines start with "*".
func rcHasBlockComments(file string) bool {
	for _, p := range rcCommentLeads(file) {
		if p == "/*" {
			return true
		}
	}
	return false
}

// rcKeyLitRe matches a quoted string literal short enough to be a key.
var rcKeyLitRe = regexp.MustCompile("[\"'`]([A-Za-z0-9_.:/\\-]{3,80})[\"'`]")

// rcLooksLikeKey keeps literals shaped like a key someone else reads: an
// event or metric name, a route, a setting, a dotted path. A plain word, a
// file name or a number is not one.
func rcLooksLikeKey(s string) bool {
	letter := false
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			letter = true
			break
		}
	}
	if !letter || strings.Contains(s, "://") {
		return false
	}
	if rcFileExts[strings.ToLower(strings.TrimPrefix(filepath.Ext(s), "."))] && !strings.ContainsAny(s, ":") {
		return false // a.go, config.yaml, docs/x.md — a file, not a key
	}
	if strings.ContainsAny(s, "_.:/-") {
		return true
	}
	return rcLooksLikeCode(s) // camelCase or CONSTANT_CASE
}

// rcFileExts are extensions that make a literal a file name. "user.name" is
// a key; "user.go" is a file.
var rcFileExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields("go py js mjs cjs ts tsx jsx json yaml yml toml md txt sql html css scss sh rb rs java kt swift c h cc cpp hpp cs php png jpg svg gif csv lock mod sum xml env cfg ini log proto") {
		rcFileExts[e] = true
	}
}

// rcSinceKeys lists, per file, the key-shaped string literals on the diff's
// changed lines. One only on removed lines is marked removed.
func rcSinceKeys(diff string) map[string][]rcChangedKey {
	type acc struct{ added, removed bool }
	per := map[string]map[string]*acc{}
	order := map[string][]string{}
	rcDiffFiles(diff, func(file, line string) {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
			return
		}
		for _, m := range rcKeyLitRe.FindAllStringSubmatch(line[1:], -1) {
			k := m[1]
			if !rcLooksLikeKey(k) {
				continue
			}
			if per[file] == nil {
				per[file] = map[string]*acc{}
			}
			a := per[file][k]
			if a == nil {
				a = &acc{}
				per[file][k] = a
				order[file] = append(order[file], k)
			}
			if line[0] == '+' {
				a.added = true
			} else {
				a.removed = true
			}
		}
	})
	out := map[string][]rcChangedKey{}
	for file, keys := range order {
		for _, k := range keys {
			a := per[file][k]
			out[file] = append(out[file], rcChangedKey{Key: k, Removed: a.removed && !a.added})
		}
	}
	return out
}

// rcKeyGrep finds where each key occurs; tests replace it.
var rcKeyGrep = rcGrepIdentifiers

// rcResolveKeys fills in where else each changed key is used: other files,
// at most rcLookupTooCommon sites (a key used everywhere tells the reviewer
// nothing). Removed keys are searched first, then the rest, up to
// rcChainMaxKeys in all. Keys with no other user are dropped.
func rcResolveKeys(keys map[string][]rcChangedKey, root string) map[string][]rcChangedKey {
	type ref struct {
		file string
		i    int
	}
	var removed, rest []ref
	files := make([]string, 0, len(keys))
	for f := range keys {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for i, k := range keys[f] {
			if k.Removed {
				removed = append(removed, ref{f, i})
			} else {
				rest = append(rest, ref{f, i})
			}
		}
	}
	refs := append(removed, rest...)
	if len(refs) > rcChainMaxKeys {
		refs = refs[:rcChainMaxKeys]
	}
	if len(refs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var lits []string
	for _, r := range refs {
		if k := keys[r.file][r.i].Key; !seen[k] {
			seen[k] = true
			lits = append(lits, k)
		}
	}
	hits := rcKeyGrep(lits, root)
	out := map[string][]rcChangedKey{}
	for _, r := range refs {
		k := keys[r.file][r.i]
		sites := hits[k.Key]
		if len(sites) == 0 || len(sites) > rcLookupTooCommon {
			continue
		}
		uses := map[string][]int{}
		for _, s := range sites { // path:line
			i := strings.LastIndex(s, ":")
			if i <= 0 {
				continue
			}
			p := rcGitUnquote(s[:i]) // git grep quotes non-ASCII paths
			if p == r.file {
				continue
			}
			n, _ := strconv.Atoi(s[i+1:])
			uses[p] = append(uses[p], n)
		}
		if len(uses) == 0 {
			continue
		}
		k.Uses = uses
		out[r.file] = append(out[r.file], k)
	}
	return out
}

// rcReReviewRelated works out a scoped re-review's related files: what changed
// in the focus since the previous review, then who it can break. nil when
// there is no focus or chaining is off.
func rcReReviewRelated(prior *rcPrior, focus []string, gdb *graph.DB, root string) []rcChained {
	max := rcChainMax()
	if prior == nil || focus == nil || max <= 0 {
		return nil
	}
	// Chaining works on real paths: the graph's, git grep's (unquoted) and
	// the since-diff's. The review's list may quote a non-ASCII one.
	real := make([]string, len(focus))
	for i, f := range focus {
		real[i] = rcGitUnquote(f)
	}
	focus = real
	diff := rcSinceDiff(prior.Head, root, focus)
	in := rcChainInputs{Symbols: rcSinceSymbols(diff), Keys: rcResolveKeys(rcSinceKeys(diff), root), Code: rcSinceCode(diff)}
	deadline := time.Now().Add(rcChainBudget)
	var g rcChainGraph = rcGrepFallback{root: root, deadline: deadline}
	if gdb != nil {
		g = rcGrepFallback{rcChainGraph: newRCGraphChain(gdb, root), root: root, deadline: deadline}
	}
	started := time.Now()
	chain := rcReReviewChain(focus, in, g, max)
	if time.Now().After(deadline) {
		fmt.Fprintf(os.Stderr, "  re-review: related-file lookups stopped at their %s budget; %d found\n", rcChainBudget, len(chain))
	} else {
		fmt.Fprintf(os.Stderr, "  re-review: related files worked out in %s\n", time.Since(started).Round(time.Millisecond))
	}
	return chain
}

// rcChainBudget bounds the related-file lookups as a whole. Each callee-name
// lookup scans the graph's call-site nodes; on a very large graph a long list
// of changed symbols must not hold the review up. Lookups that would start
// past it answer nothing, so the list is whatever was found in time.
var rcChainBudget = 20 * time.Second

// rcGrepFallback answers SymbolCallers with a word search when the graph has
// no call site for the symbol. The graph records none for a file its parser
// skipped — on kai-cli, the 800 kB cmd/kai/main.go has a file node and not one
// call in or out — and none for calls it could not resolve. A name used in
// more than rcLookupTooCommon places is not listed: it is a word, not a lookup.
type rcGrepFallback struct {
	rcChainGraph // nil: grep only
	root         string
	deadline     time.Time // zero: none
}

func (g rcGrepFallback) late() bool {
	return !g.deadline.IsZero() && time.Now().After(g.deadline)
}

func (g rcGrepFallback) SymbolCallers(path, symbol string) []rcSite {
	if g.late() {
		return nil
	}
	if g.rcChainGraph != nil {
		if s := g.rcChainGraph.SymbolCallers(path, symbol); len(s) > 0 {
			return s
		}
	}
	if len(symbol) < 4 {
		return nil
	}
	var out []rcSite
	for _, hit := range rcWordGrep(symbol, g.root) {
		i := strings.LastIndex(hit, ":")
		if i <= 0 || hit[:i] == path {
			continue
		}
		n, _ := strconv.Atoi(hit[i+1:])
		out = append(out, rcSite{Path: hit[:i], Line: n})
	}
	if len(out) > rcLookupTooCommon {
		return nil
	}
	return out
}

func (g rcGrepFallback) Callers(p string) []string {
	if g.rcChainGraph == nil || g.late() {
		return nil
	}
	return g.rcChainGraph.Callers(p)
}

func (g rcGrepFallback) Tests(p string) []string {
	if g.rcChainGraph == nil || g.late() {
		return nil
	}
	return g.rcChainGraph.Tests(p)
}

func (g rcGrepFallback) Uses(p string) []string {
	if g.rcChainGraph == nil || g.late() {
		return nil
	}
	return g.rcChainGraph.Uses(p)
}

// rcWordGrep lists path:line for whole-word occurrences of name followed by an
// opening parenthesis (a call, or a declaration), bounded like the lookups'
// search. Tests replace it.
var rcWordGrep = func(name, root string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), rcLookupGrepTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "grep", "-n", "--no-color", "-E", `(^|[^A-Za-z0-9_])`+regexp.QuoteMeta(name)+`\(`)
	cmd.WaitDelay = 2 * time.Second
	if root != "" {
		cmd.Dir = root
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		return nil
	}
	out, _ := io.ReadAll(io.LimitReader(stdout, 1<<20))
	cancel()
	_ = cmd.Wait()
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		first := strings.Index(line, ":")
		if first < 0 {
			continue
		}
		second := strings.Index(line[first+1:], ":")
		if second < 0 {
			continue
		}
		body := line[first+second+2:]
		// Its own declaration elsewhere (a same-named function in another
		// package) is not a call.
		if d, _ := rcDeclName(body); d == name {
			continue
		}
		hits = append(hits, line[:first+1+second])
		if len(hits) > rcLookupTooCommon {
			break
		}
	}
	return hits
}

// --- the code graph -----------------------------------------------------------

// rcGraphChain is rcChainGraph over the review's code graph, scoped to its
// current snapshot so files a branch move deleted are not listed.
type rcGraphChain struct {
	db    *graph.DB
	root  string
	snap  []byte
	files map[string]bool // node ids (as strings) of the snapshot's files; lazy
	paths map[string]bool // path → in the snapshot, as looked up (inSnapshot)
	text  map[string]string
}

func newRCGraphChain(db *graph.DB, root string) *rcGraphChain {
	return &rcGraphChain{db: db, root: root, snap: db.CurrentSnapshotID(), text: map[string]string{}}
}

func (g *rcGraphChain) into(path string, types ...graph.EdgeType) []string {
	seen := map[string]bool{}
	var out []string
	for _, et := range types {
		edges, err := g.db.GetEdgesToByPathScoped(path, et, g.snap)
		if err != nil {
			continue
		}
		for _, e := range edges {
			if p := g.pathOf(e.Src); p != "" && p != path && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func (g *rcGraphChain) Callers(path string) []string {
	return g.into(path, graph.EdgeCalls, graph.EdgeImports)
}

func (g *rcGraphChain) Tests(path string) []string {
	return g.into(path, graph.EdgeTests)
}

// SymbolCallers reads the call-site nodes recorded for symbol and keeps those
// whose callee resolved to path. Call-site nodes outlive the snapshot that
// made them, so a site is kept only if its file is in the current snapshot
// and still names the symbol.
func (g *rcGraphChain) SymbolCallers(path, symbol string) []rcSite {
	if !g.loadSnapshotFiles() {
		return nil
	}
	edges, err := g.db.GetEdgesByCalleeName(symbol, graph.EdgeCalls)
	if err != nil {
		return nil
	}
	seen := map[rcSite]bool{}
	var out []rcSite
	for _, e := range edges {
		if len(e.At) == 0 {
			continue
		}
		n, err := g.db.GetNode(e.At)
		if err != nil || n == nil {
			continue
		}
		callee, _ := n.Payload["calleeName"].(string)
		calleeFile, _ := n.Payload["calleeFile"].(string)
		caller, _ := n.Payload["callerFile"].(string)
		if rcBareName(callee) != symbol || calleeFile != path || caller == "" || !g.inSnapshot(caller) {
			continue
		}
		if !strings.Contains(g.read(caller), symbol) {
			continue
		}
		site := rcSite{Path: caller}
		if l, ok := n.Payload["line"].(float64); ok {
			site.Line = int(l) + 1 // the parser counts rows from 0
		}
		if !seen[site] {
			seen[site] = true
			out = append(out, site)
		}
	}
	return out
}

// rcBareName strips a qualifier: Type.Method → Method, crate::foo → foo.
func rcBareName(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return s
}

func (g *rcGraphChain) read(path string) string {
	if t, ok := g.text[path]; ok {
		return t
	}
	b, _ := os.ReadFile(filepath.Join(g.root, path))
	g.text[path] = string(b)
	return g.text[path]
}

// Uses follows the focus file's own CALLS and IMPORTS edges out. A path has a
// node per content version, so only the version in the current snapshot is
// followed, and only to files in it.
func (g *rcGraphChain) Uses(path string) []string {
	if !g.loadSnapshotFiles() {
		return nil
	}
	nodes, err := g.db.FindNodesByPayloadPath(string(graph.KindFile), path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if !g.files[string(n.ID)] {
			continue
		}
		for _, et := range []graph.EdgeType{graph.EdgeCalls, graph.EdgeImports} {
			edges, err := g.db.GetEdges(n.ID, et)
			if err != nil {
				continue
			}
			for _, e := range edges {
				if !g.files[string(e.Dst)] {
					continue
				}
				if p := g.pathOf(e.Dst); p != "" && p != path && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func (g *rcGraphChain) loadSnapshotFiles() bool {
	if g.files != nil {
		return len(g.files) > 0
	}
	g.files, g.paths = map[string]bool{}, map[string]bool{}
	if len(g.snap) == 0 {
		return false
	}
	// One query for the snapshot's file ids. Paths are looked up only for
	// the caller files a lookup meets (inSnapshot), not for every file:
	// on a large repository that was one query per file up front.
	edges, err := g.db.GetEdges(g.snap, graph.EdgeHasFile)
	if err != nil {
		return false
	}
	for _, e := range edges {
		g.files[string(e.Dst)] = true
	}
	return len(g.files) > 0
}

// inSnapshot reports whether path is a file of the current snapshot.
// Remembered per path.
func (g *rcGraphChain) inSnapshot(path string) bool {
	if in, ok := g.paths[path]; ok {
		return in
	}
	in := false
	if nodes, err := g.db.FindNodesByPayloadPath(string(graph.KindFile), path); err == nil {
		for _, n := range nodes {
			if g.files[string(n.ID)] {
				in = true
				break
			}
		}
	}
	g.paths[path] = in
	return in
}

func (g *rcGraphChain) pathOf(id []byte) string {
	if len(id) == 0 {
		return ""
	}
	n, err := g.db.GetNode(id)
	if err != nil || n == nil {
		return ""
	}
	p, _ := n.Payload["path"].(string)
	return p
}
