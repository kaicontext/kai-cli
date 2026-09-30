package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Review hints: lines in the diff that have the SHAPE of the defects the
// reviewer keeps missing, handed to it up front with the question to answer.
//
// The 2026-09-24 benchmark and the first two nightly corpus runs
// (kai-cli#144) showed the prompt rules for contracts and concurrency working
// only sometimes: Cal.com #14943's `retryCount: reminder.retryCount + 1` was
// caught in one nightly run and missed in the next on the same code, and the
// Keycloak resource-key mismatches were never caught — in files the review
// had opened. The lines are not hard to find; the model does not look at them
// with the right question. So the pipeline finds them and asks.
//
// These are NOT findings. The block says so, and every rule about a reachable
// trigger and a demonstrated failure still applies: a hint the reviewer checks
// and clears costs nothing, and one it confirms is a finding it would
// otherwise have missed.

type rcHint struct {
	Path  string
	Line  int
	Code  string
	Added bool // false for an unchanged context line
}

var (
	// `field: obj.field + 1` / `x.n = x.n - 1`: a value written back from the
	// value just read.
	rcRMWRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*[:=]\s*[A-Za-z_][A-Za-z0-9_.]*\.([A-Za-z_][A-Za-z0-9_]*)\s*[+-]\s*\d+\b`)
	// A membership check on something the same change later writes back.
	rcCheckRe = regexp.MustCompile(`\.(indexOf|includes|has|contains)\(`)
	rcWriteRe = regexp.MustCompile(`\.(update|updateMany|upsert|save|set|delete|splice)\(|\bUPDATE\b`)
	// A lookup by a key the caller supplies.
	rcLookupRe = regexp.MustCompile(`\b(findByName|findById|findByOwner|findUnique|findFirst|get[A-Z][A-Za-z]*By(Id|Name|Key))\s*\(`)
	// An identifier collected from a lookup result, for someone else to use.
	rcCollectRe = regexp.MustCompile(`\.(add|push|append|put)\(\s*[A-Za-z_][A-Za-z0-9_]*\.(getId|getName|getKey|id|name|key)\b`)
	// A token refresh, and a later read of a credential field.
	rcRefreshRe  = regexp.MustCompile(`(?i)refresh[A-Za-z]*\s*\(`)
	rcTokenUseRe = regexp.MustCompile(`\.(access_token|accessToken|refresh_token|refreshToken)\b`)
	// A client or connection built in a file that refreshes credentials: is it
	// built from the refreshed values or from the ones read before?
	rcClientNewRe = regexp.MustCompile(`\bnew\s+[A-Za-z_][A-Za-z0-9_.]*(Connection|Client)\s*\(`)
	rcCallStartRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

// rcAddedLines walks a unified diff and returns, per file, its added and
// context lines with their new-side line numbers (Added tells them apart).
// Comment-only lines are skipped.
func rcAddedLines(diff string) map[string][]rcHint {
	out := map[string][]rcHint{}
	var path string
	newLine := 0
	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			f := strings.Fields(raw)
			path = ""
			if len(f) >= 4 {
				path = strings.TrimPrefix(f[len(f)-1], "b/")
			}
		case strings.HasPrefix(raw, "+++ "), strings.HasPrefix(raw, "--- "):
		case strings.HasPrefix(raw, "@@ "):
			if i := strings.Index(raw, "+"); i >= 0 {
				num := raw[i+1:]
				if j := strings.IndexAny(num, ", "); j >= 0 {
					num = num[:j]
				}
				newLine, _ = strconv.Atoi(num)
			}
		case strings.HasPrefix(raw, "-"):
		case strings.HasPrefix(raw, "+"), strings.HasPrefix(raw, " "):
			added := raw[0] == '+'
			code := strings.TrimSpace(raw[1:])
			cur := newLine
			newLine++
			if path == "" || code == "" || strings.HasPrefix(code, "//") || strings.HasPrefix(code, "*") ||
				strings.HasPrefix(code, "#") || strings.HasPrefix(code, "/*") {
				continue
			}
			out[path] = append(out[path], rcHint{Path: path, Line: cur, Code: code, Added: added})
		default:
			newLine++
		}
	}
	return out
}

func rcIsTestPath(p string) bool {
	l := strings.ToLower(p)
	return strings.Contains(l, "/test/") || strings.Contains(l, "/tests/") || strings.Contains(l, "_test.") ||
		strings.Contains(l, ".test.") || strings.Contains(l, ".spec.") || strings.Contains(l, "/__tests__/") ||
		strings.Contains(l, "/e2e/") || strings.Contains(l, "/playwright/")
}

// rcReviewHints renders the hint block for the reviewer's input, or "" when
// the diff has none of the shapes. Test files are skipped: the defects these
// point at live in the code under test.
func rcReviewHints(diff string) string {
	const perKind = 6
	byFile := rcAddedLines(diff)
	var rmw, check, lookup, collect, fresh []string
	add := func(dst *[]string, h rcHint) {
		if len(*dst) < perKind {
			*dst = append(*dst, fmt.Sprintf("- %s:%d  %s", h.Path, h.Line, rcOneLine(h.Code, 140)))
		}
	}
	var outliers, clients []string
	calls := map[string][]rcCall{}
	for _, path := range sortedKeys(byFile) {
		if rcIsTestPath(path) {
			continue
		}
		lines := byFile[path]
		hasWrite, hasRefresh := false, false
		for _, h := range lines {
			if h.Added {
				hasWrite = hasWrite || rcWriteRe.MatchString(h.Code)
				hasRefresh = hasRefresh || rcRefreshRe.MatchString(h.Code)
			}
		}
		for _, h := range lines {
			// A client built from credentials is the likelier stale read, so
			// those go first; the cap then drops plain field reads.
			if hasRefresh && rcClientNewRe.MatchString(h.Code) {
				add(&clients, h)
			} else if hasRefresh && rcTokenUseRe.MatchString(h.Code) {
				add(&fresh, h)
			}
			if !h.Added {
				continue
			}
			if m := rcRMWRe.FindStringSubmatch(h.Code); m != nil && m[1] == m[2] {
				add(&rmw, h)
			}
			if hasWrite && rcCheckRe.MatchString(h.Code) {
				add(&check, h)
			}
			if rcLookupRe.MatchString(h.Code) {
				add(&lookup, h)
			}
			if rcCollectRe.MatchString(h.Code) {
				add(&collect, h)
			}
		}
		for _, c := range rcAddedCalls(lines) {
			calls[c.Name] = append(calls[c.Name], c)
		}
	}
	for _, name := range sortedCallNames(calls) {
		for _, c := range rcOutlierCalls(calls[name]) {
			if len(outliers) < perKind {
				outliers = append(outliers, fmt.Sprintf("- %s:%d  %s(…) passes %q as argument %d where the other %d calls pass %q",
					c.Path, c.Line, name, c.Odd, c.Pos+1, c.Others, c.Usual))
			}
		}
	}
	if len(rmw)+len(check)+len(lookup)+len(collect)+len(fresh)+len(clients)+len(outliers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("REVIEW HINTS (lines with the shape of defects reviews often miss. These are NOT findings: check each one, report it only with a reachable trigger and the failure it causes, and otherwise drop it):\n")
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString(title + "\n")
		for _, it := range items {
			b.WriteString(it + "\n")
		}
	}
	section("A value written back from the value just read — can two concurrent requests both read the old value and lose an update (TWO REQUESTS AT ONCE)?", rmw)
	section("A membership check in a file that also writes the collection back — can two requests both pass the check before either write lands?", check)
	section("A lookup by a key — is the key the same KIND the record was stored under (internal id vs external id vs name)? Find where the record is created (TRACE WHAT CROSSES A CALL).", lookup)
	section("Identifiers collected from lookup results and returned — which kind (id or name), and what do the callers pass them to?", collect)
	section("A client or connection built in a file that refreshes credentials — is it built from the refreshed values or from ones read before the refresh?", clients)
	section("Credential fields read in a file that refreshes them — does each read come after the refresh, from the refreshed value?", fresh)
	section("A call that passes something different from every other call of the same function — is the odd one the value the parameter means?", outliers)
	b.WriteString("\n")
	return b.String()
}

func sortedKeys(m map[string][]rcHint) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// rcCall is one call of a function in the added lines, with its arguments.
type rcCall struct {
	Name string
	Path string
	Line int
	Args []string
	// Filled for an outlier by rcOutlierCalls.
	Pos    int
	Odd    string
	Usual  string
	Others int
}

// rcAddedCalls finds calls whose opening line is added and collects their
// arguments across the following lines (a call written one argument per
// line is how the Cal.com #11059 outlier was hidden), up to 25 lines.
func rcAddedCalls(lines []rcHint) []rcCall {
	var out []rcCall
	for i, h := range lines {
		if !h.Added {
			continue
		}
		for _, m := range rcCallStartRe.FindAllStringSubmatchIndex(h.Code, -1) {
			name := h.Code[m[2]:m[3]]
			switch name {
			case "if", "for", "while", "switch", "return", "function", "catch", "typeof", "new", "await", "async":
				continue
			}
			var buf strings.Builder
			buf.WriteString(h.Code[m[1]:])
			args, ok := rcSplitArgs(buf.String())
			for j := i + 1; !ok && j < len(lines) && j <= i+25; j++ {
				if !lines[j].Added {
					break
				}
				buf.WriteString(" " + lines[j].Code)
				args, ok = rcSplitArgs(buf.String())
			}
			if ok && len(args) >= 2 {
				out = append(out, rcCall{Name: name, Path: h.Path, Line: h.Line, Args: args})
			}
		}
	}
	return out
}

// rcSplitArgs splits the text after an opening parenthesis into top-level
// arguments. ok is false until the matching close parenthesis is seen.
func rcSplitArgs(s string) (args []string, ok bool) {
	depth := 1
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		if quote != 0 {
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '"', '\'', '`':
			quote = r
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				if a := strings.TrimSpace(cur.String()); a != "" {
					args = append(args, a)
				}
				return args, true
			}
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(r)
	}
	return nil, false
}

var rcSimpleArgRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// rcOutlierCalls reports calls of one function whose argument at some
// position is a plain identifier different from the one every other call
// (at least three of them) passes there. Positions where the calls already
// disagree among themselves say nothing and are skipped.
func rcOutlierCalls(calls []rcCall) []rcCall {
	if len(calls) < 4 {
		return nil
	}
	var out []rcCall
	maxArgs := 0
	for _, c := range calls {
		if len(c.Args) > maxArgs {
			maxArgs = len(c.Args)
		}
	}
	for pos := 0; pos < maxArgs; pos++ {
		counts := map[string]int{}
		for _, c := range calls {
			if pos < len(c.Args) && rcSimpleArgRe.MatchString(c.Args[pos]) {
				counts[c.Args[pos]]++
			}
		}
		if len(counts) != 2 {
			continue
		}
		var usual, odd string
		for v, n := range counts {
			if n == 1 {
				odd = v
			} else if n >= 3 {
				usual = v
			}
		}
		if usual == "" || odd == "" {
			continue
		}
		for _, c := range calls {
			if pos < len(c.Args) && c.Args[pos] == odd {
				c.Pos, c.Odd, c.Usual, c.Others = pos, odd, usual, counts[usual]
				out = append(out, c)
			}
		}
	}
	return out
}

func sortedCallNames(m map[string][]rcCall) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
