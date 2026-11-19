package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// What the reviewer reads of each changed file. Real code, manifests, infra
// and test snapshots are read in full; a lockfile is cut to the lines a
// person checks in one; generated, minified, binary and re-vendored files
// are only named.
type rcFileKind int

const (
	rcFileReview rcFileKind = iota
	rcFileLockSummary
	rcFileOmit
)

var (
	rcLockfile = regexp.MustCompile(`(?i)(^|/)(package-lock\.json|npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|bun\.lock|go\.sum|poetry\.lock|uv\.lock|pipfile\.lock|pdm\.lock|gemfile\.lock|cargo\.lock|composer\.lock|mix\.lock|pubspec\.lock|podfile\.lock|packages\.lock\.json|flake\.lock)$`)
	// Never read line by line: the source (.proto, schema) is reviewed instead,
	// or the file is not text.
	rcNeverReviewed = regexp.MustCompile(`(?i)(^|/)node_modules/|(^|/)bun\.lockb$|\.min\.(js|css)$|\.(png|jpe?g|gif|webp|ico|pdf|woff2?|ttf|eot|zip|gz|jar)$|_pb2(_grpc)?\.py$|\.pb\.go$|\.generated\.`)
	rcVendored      = regexp.MustCompile(`(?i)(^|/)vendor/`)
	// A change that also moves one of these is a dependency bump, so its
	// vendor/ files are a re-vendor rather than a hand edit to a library.
	rcDepManifest = regexp.MustCompile(`(?i)(^|/)(go\.mod|vendor/modules\.txt|composer\.(json|lock)|gemfile(\.lock)?|cargo\.(toml|lock)|package\.json)$`)
)

func rcVendorBump(paths []string) bool {
	for _, p := range paths {
		if rcDepManifest.MatchString(p) {
			return true
		}
	}
	return false
}

func rcFileKindOf(path string, vendorBump bool) rcFileKind {
	switch {
	case rcNeverReviewed.MatchString(path):
		return rcFileOmit
	case rcLockfile.MatchString(path):
		return rcFileLockSummary
	case vendorBump && rcVendored.MatchString(path):
		return rcFileOmit
	}
	return rcFileReview
}

// rcReviewablePaths keeps the files the reviewer reads in full, so the turn
// budget and the coverage gate never send it to open a lockfile. changed is
// every changed path, which decides whether vendor/ is a bump.
func rcReviewablePaths(scope, changed []string) []string {
	if scope == nil {
		return nil
	}
	bump := rcVendorBump(changed)
	out := make([]string, 0, len(scope))
	for _, p := range scope {
		if rcFileKindOf(p, bump) == rcFileReview {
			out = append(out, p)
		}
	}
	return out
}

const rcLockSummaryMaxLines = 150

var (
	// Lockfile lines a reviewer checks: versions, engine floors, and where a
	// package is fetched from. Besides version fields: Bundler and CocoaPods'
	// name (1.2.3), bun.lock's ["pkg@1.2.3", mix.lock's {:hex, :pkg, "1.2.3",
	// and a Nix flake input's "rev".
	rcLockKeep = regexp.MustCompile(`(?i)"version"\s*:|^\s*version[\s:=]|"engines"|"node"\s*:|"resolved"\s*:|^\s*resolved[\s:]|tarball:|^\s*(-\s+)?[\w.\-/]+ \(\d[^)]*\):?\s*$|\["[^"]+@\d|\{:hex,\s*:\w+,\s*"\d|"rev"\s*:`)
	// The package a kept line belongs to: npm's "node_modules/x": {, a yarn
	// or pnpm entry header, a Yarn Berry "x@npm:1.2.3": header, Cargo and
	// Poetry's name = line, composer.lock's "name": "vendor/pkg".
	rcLockKey = regexp.MustCompile(`^\s*"node_modules/[^"]+":\s*\{\s*$|^"?[^\s#"][^:]*"?:\s*$|^ {2}['"]?/?[@\w][^:]*['"]?:\s*$|^"[^"]+@[a-z]+:[^"]*":\s*$|^\s*name\s*=|^\s*"name"\s*:\s*"[^"]+",?\s*$`)
	// A pnpm entry key that carries the version itself: /foo/1.1.0: (pnpm ≤8)
	// or foo@1.1.0: (pnpm 9), either optionally @scoped and quoted.
	rcLockVersionedKey = regexp.MustCompile(`^ {2}['"]?/?(@[^/\s'"]+/)?[^@/\s'"]+[/@]\d[^:]*['"]?:\s*$`)
	// The default registries; a source anywhere else is the supply-chain
	// change worth showing.
	rcDefaultRegistry = regexp.MustCompile(`(?i)://(registry\.npmjs\.org|registry\.yarnpkg\.com)([/:]|$)`)
	rcLockSource      = regexp.MustCompile(`(?i)resolved|tarball:`)
)

// rcDropUnreviewable rewrites a unified diff for the reviewer: lockfiles cut
// to their version and source lines, omitted files reduced to a one-line note,
// everything else unchanged.
func rcDropUnreviewable(diff string) string {
	if !strings.Contains(diff, "diff --git ") {
		return diff
	}
	var b strings.Builder
	b.Grow(len(diff))
	var secs [][]string
	for _, l := range strings.SplitAfter(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			secs = append(secs, []string{l})
		case len(secs) == 0:
			b.WriteString(l)
		default:
			secs[len(secs)-1] = append(secs[len(secs)-1], l)
		}
	}
	paths := make([]string, len(secs))
	for i, sec := range secs {
		paths[i] = rcSectionPath(sec)
	}
	bump := rcVendorBump(paths)

	for i, sec := range secs {
		if paths[i] == "" {
			rcWriteLines(&b, sec)
			continue
		}
		switch rcFileKindOf(paths[i], bump) {
		case rcFileLockSummary:
			rcWriteLockSummary(&b, sec)
		case rcFileOmit:
			rcWriteLines(&b, sec[:1])
			// A binary section has only a "Binary files ... differ" line to count.
			if n := rcChangedLines(sec[1:]); n > 0 || !rcHasBinaryLine(sec) {
				fmt.Fprintf(&b, "(vendored, generated, minified or binary file: %d changed line(s), not shown)\n", n)
			} else {
				b.WriteString("(binary file, not shown)\n")
			}
		default:
			rcWriteLines(&b, sec)
		}
	}
	return b.String()
}

var rcLockHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)

// rcLockLine is a kept lockfile line and where it sits: old and new are the
// line numbers it starts at, adv how far it moves each side.
type rcLockLine struct {
	text                     string
	old, new, advOld, advNew int
}

func rcWriteLockSummary(b *strings.Builder, sec []string) {
	var meta []string
	var kept []rcLockLine
	var lastKey rcLockLine
	keyShown, inHunk := false, false
	oldN, newN := 0, 0
	for _, l := range sec[1:] {
		if m := rcLockHunkHeader.FindStringSubmatch(l); m != nil {
			oldN, _ = strconv.Atoi(m[1])
			newN, _ = strconv.Atoi(m[2])
			inHunk = true
			continue
		}
		if !inHunk {
			if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
				meta = append(meta, l)
			}
			continue
		}
		if l == "" || l == "\n" || rcDiffMetaLine(l) {
			continue
		}
		cur := rcLockLine{text: l, old: oldN, new: newN}
		switch l[0] {
		case '+':
			cur.advNew = 1
		case '-':
			cur.advOld = 1
		default:
			cur.advOld, cur.advNew = 1, 1
		}
		oldN += cur.advOld
		newN += cur.advNew
		content := strings.TrimRight(l[1:], "\n")
		if rcChangedLine(l) && rcLockVersionedKey.MatchString(content) {
			kept = append(kept, cur)
			lastKey, keyShown = cur, true
			continue
		}
		if rcChangedLine(l) && rcLockKeep.MatchString(content) {
			if rcLockSource.MatchString(content) && rcDefaultRegistry.MatchString(content) {
				continue
			}
			if !keyShown && lastKey.text != "" {
				kept = append(kept, lastKey)
				keyShown = true
			}
			kept = append(kept, cur)
			continue
		}
		if rcLockKey.MatchString(content) {
			lastKey, keyShown = cur, false
		}
	}
	rcWriteLines(b, sec[:1])
	rcWriteLines(b, meta)
	fmt.Fprintf(b, "(lockfile: %d changed line(s); only package versions, engine requirements and non-registry sources are shown)\n", rcChangedLines(sec[1:]))
	more := 0
	if len(kept) > rcLockSummaryMaxLines {
		more = len(kept) - rcLockSummaryMaxLines
		kept = kept[:rcLockSummaryMaxLines]
	}
	// A hunk header wherever the kept lines skip ahead on the new side, so
	// anything reading the diff by line number lands on the real line.
	next := -1
	for _, k := range kept {
		if k.new != next {
			fmt.Fprintf(b, "@@ -%d +%d @@\n", k.old, k.new)
		}
		rcWriteLines(b, []string{k.text})
		next = k.new + k.advNew
	}
	if more > 0 {
		fmt.Fprintf(b, "... (%d more lockfile line(s) not shown)\n", more)
	}
}

func rcWriteLines(b *strings.Builder, ls []string) {
	for _, l := range ls {
		if l == "" {
			continue
		}
		b.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			b.WriteString("\n")
		}
	}
}

func rcDiffMetaLine(l string) bool {
	for _, p := range []string{"+++ ", "--- ", "@@", "index ", "new file mode ", "deleted file mode ", "old mode ", "new mode ", "similarity index ", "rename ", "Binary files ", `\ No newline`} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func rcChangedLine(l string) bool {
	return (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) && !rcDiffMetaLine(l)
}

func rcHasBinaryLine(ls []string) bool {
	for _, l := range ls {
		if strings.HasPrefix(l, "Binary files ") {
			return true
		}
	}
	return false
}

func rcChangedLines(ls []string) int {
	n := 0
	for _, l := range ls {
		if rcChangedLine(l) {
			n++
		}
	}
	return n
}

// rcSectionPath is the new path of one file's diff section: git's own
// rename/copy target or +++ b/ line, else what the header gives.
func rcSectionPath(sec []string) string {
	minus := ""
	for _, l := range sec[1:] {
		t := strings.TrimRight(l, "\r\n")
		switch {
		case strings.HasPrefix(t, "@@"):
			return rcDiffHeaderPath(sec[0])
		case strings.HasPrefix(t, "rename to "):
			return strings.TrimPrefix(t, "rename to ")
		case strings.HasPrefix(t, "copy to "):
			return strings.TrimPrefix(t, "copy to ")
		case strings.HasPrefix(t, "--- a/"):
			minus = strings.TrimRight(t[len("--- a/"):], "\t")
		case strings.HasPrefix(t, "+++ b/"):
			return strings.TrimRight(t[len("+++ b/"):], "\t")
		case t == "+++ /dev/null" && minus != "":
			return minus
		}
	}
	return rcDiffHeaderPath(sec[0])
}

// rcDiffHeaderPath is the b/ path of a "diff --git a/x b/y" line.
func rcDiffHeaderPath(header string) string {
	// "a/<p> b/<p>" splits at its midpoint when the path is unchanged, which
	// keeps paths with spaces whole; a rename splits at its last " b/".
	rest := strings.TrimRight(strings.TrimPrefix(header, "diff --git "), "\r\n")
	if n := len(rest); n%2 == 1 {
		a, b := rest[:n/2], rest[n/2+1:]
		if rest[n/2] == ' ' && strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return b[2:]
		}
	}
	if i := strings.LastIndex(rest, " b/"); strings.HasPrefix(rest, "a/") && i > 0 {
		return rest[i+len(" b/"):]
	}
	f := strings.Fields(header)
	if len(f) < 4 {
		return ""
	}
	return strings.TrimPrefix(f[len(f)-1], "b/")
}
