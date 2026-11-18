package main

import (
	"fmt"
	"regexp"
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
	// package is fetched from.
	rcLockKeep = regexp.MustCompile(`(?i)"version"\s*:|^\s*version[\s:=]|"engines"|"node"\s*:|"resolved"\s*:|^\s*resolved[\s:]|tarball:`)
	// The package a kept line belongs to: npm's "node_modules/x": {, a yarn
	// or pnpm entry header, Cargo and Poetry's name = line.
	rcLockKey = regexp.MustCompile(`^\s*"node_modules/[^"]+":\s*\{\s*$|^"?[^\s#"][^:]*"?:\s*$|^ {2}['"]?/?[@\w][^:]*['"]?:\s*$|^\s*name\s*=`)
	// A pnpm entry key that carries the version itself: /foo/1.1.0: (pnpm ≤8)
	// or foo@1.1.0: (pnpm 9), either optionally @scoped and quoted.
	rcLockVersionedKey = regexp.MustCompile(`^ {2}['"]?/?(@[^/\s'"]+/)?[^@/\s'"]+[/@]\d[^:]*['"]?:\s*$`)
	// The default registries; a source anywhere else is the supply-chain
	// change worth showing.
	rcDefaultRegistry = regexp.MustCompile(`(?i)registry\.npmjs\.org|registry\.yarnpkg\.com`)
	rcLockSource      = regexp.MustCompile(`(?i)resolved|tarball:`)
)

// rcDropUnreviewable rewrites a unified diff for the reviewer: lockfiles cut
// to their version and source lines, omitted files reduced to a one-line note,
// everything else unchanged.
func rcDropUnreviewable(diff string) string {
	if !strings.Contains(diff, "diff --git ") {
		return diff
	}
	lines := strings.SplitAfter(diff, "\n")
	var paths []string
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git ") {
			paths = append(paths, rcDiffHeaderPath(l))
		}
	}
	bump := rcVendorBump(paths)

	var b strings.Builder
	b.Grow(len(diff))
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		switch path := rcDiffHeaderPath(cur[0]); {
		case path == "":
			rcWriteLines(&b, cur)
		default:
			switch rcFileKindOf(path, bump) {
			case rcFileLockSummary:
				rcWriteLockSummary(&b, cur)
			case rcFileOmit:
				rcWriteLines(&b, cur[:1])
				fmt.Fprintf(&b, "(vendored, generated, minified or binary file: %d changed line(s), not shown)\n", rcChangedLines(cur[1:]))
			default:
				rcWriteLines(&b, cur)
			}
		}
		cur = cur[:0]
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git ") {
			flush()
		}
		if len(cur) == 0 && !strings.HasPrefix(l, "diff --git ") {
			b.WriteString(l)
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return b.String()
}

func rcWriteLockSummary(b *strings.Builder, sec []string) {
	var kept []string
	lastKey, keyShown := "", false
	for _, l := range sec[1:] {
		if l == "" || l == "\n" || rcDiffMetaLine(l) {
			continue
		}
		content := strings.TrimRight(l[1:], "\n")
		if rcChangedLine(l) && rcLockVersionedKey.MatchString(content) {
			kept = append(kept, l)
			lastKey, keyShown = l, true
			continue
		}
		if rcChangedLine(l) && rcLockKeep.MatchString(content) {
			if rcLockSource.MatchString(content) && rcDefaultRegistry.MatchString(content) {
				continue
			}
			if !keyShown && lastKey != "" {
				kept = append(kept, lastKey)
				keyShown = true
			}
			kept = append(kept, l)
			continue
		}
		if rcLockKey.MatchString(content) {
			lastKey, keyShown = l, false
		}
	}
	rcWriteLines(b, sec[:1])
	fmt.Fprintf(b, "(lockfile: %d changed line(s); only package versions, engine requirements and non-registry sources are shown)\n", rcChangedLines(sec[1:]))
	more := 0
	if len(kept) > rcLockSummaryMaxLines {
		more = len(kept) - rcLockSummaryMaxLines
		kept = kept[:rcLockSummaryMaxLines]
	}
	rcWriteLines(b, kept)
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

func rcChangedLines(ls []string) int {
	n := 0
	for _, l := range ls {
		if rcChangedLine(l) {
			n++
		}
	}
	return n
}

// rcDiffHeaderPath is the b/ path of a "diff --git a/x b/x" line.
func rcDiffHeaderPath(header string) string {
	// "a/<p> b/<p>" splits at its midpoint when the path is unchanged, which
	// keeps paths with spaces whole; renames fall back to the last field.
	rest := strings.TrimRight(strings.TrimPrefix(header, "diff --git "), "\r\n")
	if n := len(rest); n%2 == 1 {
		a, b := rest[:n/2], rest[n/2+1:]
		if rest[n/2] == ' ' && strings.HasPrefix(a, "a/") && strings.HasPrefix(b, "b/") && a[2:] == b[2:] {
			return b[2:]
		}
	}
	f := strings.Fields(header)
	if len(f) < 4 {
		return ""
	}
	return strings.TrimPrefix(f[len(f)-1], "b/")
}
