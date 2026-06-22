package planner

import (
	"regexp"
	"strings"
)

// Entry-point extraction for graph-powered context injection. This
// file is the front half of the pipeline: it turns the user's raw
// request into a set of code-shaped tokens that get looked up in the
// graph. Plain English words are dropped at this stage — only tokens
// that visibly look like code (backticks, CamelCase, snake_case,
// paths) survive. The point is to keep false positives low: matching
// the English word "open" against a function named Open in any
// codebase produces a false call chain that misleads the agent worse
// than starting with no context at all.
//
// The output feeds resolveEntryPoints in this same package, which
// runs the three-stage lookup (commands → symbols → files) and
// produces graph nodes for the call-chain walker.

// EntryPointToken is one code-shaped substring extracted from the
// user's request. Origin tracks how the matcher classified it so the
// downstream lookup can prefer the right index (a backticked
// "kai code" should hit the command index before falling through to
// symbol/file lookup; a path-shaped "set.go" should skip the symbol
// stage). Raw is the original substring as it appeared — needed for
// the command index, which matches against subcommand names as
// written rather than against an identifier shape.
type EntryPointToken struct {
	// Raw is the substring exactly as it appeared in the user's
	// request (sans backticks if it was backtick-wrapped).
	Raw string

	// Origin classifies how this token was detected. Helps the
	// lookup pipeline pick the right index first.
	Origin TokenOrigin
}

// TokenOrigin is how the matcher tagged a token. Multiple origins
// can match the same substring; matcher reports the most-specific
// one (backtick beats CamelCase beats path-shaped) because backticks
// are an explicit user signal and CamelCase is a strong identifier
// signal, while path-shaped is the broadest.
type TokenOrigin int

const (
	OriginBacktick TokenOrigin = iota
	OriginCamelCase
	OriginSnakeCase
	OriginPath
)

// backtickRE matches the contents of backtick spans. Greedy by
// default in regexp; the `?` makes it non-greedy so a string with
// two backtick pairs produces two tokens, not one giant one.
var backtickRE = regexp.MustCompile("`([^`]+?)`")

// camelCaseRE matches strict CamelCase or pascalCase identifiers:
// 3-80 chars, starts with a letter, contains at least one
// upper-after-lower transition. The transition requirement is what
// excludes "Run" or "Get" (single-segment caps that show up as
// English-adjacent words) while admitting "runCodeTUI" or
// "ParseConfig". 3-char minimum mirrors plannerStopwords' floor —
// shorter tokens are too noisy.
//
// Limitation: words that legitimately have an internal upper (rare
// in English, common in identifiers like "URLParser") all match.
// Acceptable; the lookup stage will drop misses.
var camelCaseRE = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9]{1,79}\b`)

// snakeCaseRE matches snake_case identifiers: 3-80 chars, at least
// one underscore, alphanumeric segments. Strict on the underscore
// requirement so plain words like "open" don't match — "extract_paths"
// is fine, "extractpaths" isn't (it goes through the CamelCase path
// only if it has a case transition).
var snakeCaseRE = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9]*(?:_[a-zA-Z0-9]+)+\b`)

// pathLikeRE matches strings that look like file paths or filenames
// with extensions: contain `/` or end in `.<ext>` where ext is
// 1-6 chars of lowercase letters/digits. Trailing punctuation
// (period, comma, semicolon) is stripped at the end by the caller —
// the regex is loose on purpose; tightening it loses real file
// references like "internal/projects/set.go" that get followed by a
// comma in prose.
var pathLikeRE = regexp.MustCompile(`(?:[a-zA-Z0-9_./-]+/)+[a-zA-Z0-9_./-]+|[a-zA-Z0-9_-]+\.[a-z0-9]{1,6}`)

// hasUpperLowerTransition reports whether s has the shape of a
// compound identifier — either a canonical camelCase lower→upper
// transition, or the acronym-prefixed PascalCase shape ("URLParser",
// "HTTPHandler") where there are multiple uppercase letters mixed
// with lowercase. Standalone uppercase tokens like "URL" or "RUN"
// don't pass — they're indistinguishable from shouty English in
// user prose. Plain PascalCase ("Primary", "Manager") also doesn't
// pass for the same reason: too close to English nouns. Backticks
// are the user's escape hatch for those cases.
func hasUpperLowerTransition(s string) bool {
	upper, lower := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			upper++
		case c >= 'a' && c <= 'z':
			lower++
		}
		if i > 0 {
			prev := s[i-1]
			if prev >= 'a' && prev <= 'z' && c >= 'A' && c <= 'Z' {
				return true
			}
		}
	}
	// No lower→upper transition. Accept the URLParser pattern:
	// ≥2 uppercase letters AND at least one lowercase. This admits
	// "URLParser", "HTTPHandler", "JSONDecoder" while still rejecting
	// "URL" or "RUN" (which lack the trailing lowercase tail).
	return upper >= 2 && lower >= 1
}

// ExtractEntryPointTokens scans the user's request and returns
// every code-shaped substring, deduplicated, with its origin
// classification. Order in the returned slice is detection order so
// downstream code can preserve "leftmost mentioned" priority if it
// wants to (the lookup pipeline currently doesn't, but might later
// when ranking ambiguous matches).
//
// The deduplication is case-sensitive on Raw — "Primary" and
// "primary" are different tokens because they may resolve to
// different symbols. Origin is preserved per first-occurrence.
func ExtractEntryPointTokens(request string) []EntryPointToken {
	if strings.TrimSpace(request) == "" {
		return nil
	}

	seen := map[string]bool{}
	var out []EntryPointToken
	add := func(raw string, origin TokenOrigin) {
		raw = strings.TrimSpace(raw)
		// Strip a trailing punctuation char so "set.go," doesn't
		// turn into a path that includes the comma. Skipped for
		// backtick origin — the user explicitly demarcated the
		// substring, and a token like `Primary()` should preserve
		// the parens because they're part of the user's reference.
		// Only strip one char; paths legitimately end in `.go` or `.yaml`.
		if origin != OriginBacktick {
			if n := len(raw); n > 0 {
				switch raw[n-1] {
				case ',', ';', ':', ')', ']', '}', '.', '?', '!':
					raw = raw[:n-1]
				}
			}
		}
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, EntryPointToken{Raw: raw, Origin: origin})
	}

	// Pass 1: backticked spans. Process first so the substring
	// inside the backticks is claimed by OriginBacktick rather than
	// re-matched as CamelCase later. The downstream pipeline checks
	// `seen` so a backtick claim wins.
	for _, m := range backtickRE.FindAllStringSubmatch(request, -1) {
		if len(m) >= 2 {
			add(m[1], OriginBacktick)
		}
	}

	// Pass 2: explicit path-shaped tokens. Run before camelCase so a
	// path like "internal/projects/set.go" is tagged once as a path
	// rather than three times as camelCase fragments.
	for _, m := range pathLikeRE.FindAllString(request, -1) {
		add(m, OriginPath)
	}

	// Pass 3: snake_case. Strict — must contain an underscore. Runs
	// before camelCase because some identifiers (rare) have both
	// (mixedCase_thing) and we want the explicit underscore to be
	// the dominant signal.
	for _, m := range snakeCaseRE.FindAllString(request, -1) {
		add(m, OriginSnakeCase)
	}

	// Pass 4: CamelCase. Filtered to require an upper-after-lower
	// transition so plain words like "Open" / "Run" / "New" don't
	// match. "ParseConfig" / "runCodeTUI" / "URLParser" pass.
	for _, m := range camelCaseRE.FindAllString(request, -1) {
		if !hasUpperLowerTransition(m) {
			continue
		}
		// Skip very short matches even with a transition — "iT" /
		// "aB" can show up in normal prose ("aBc" patterns).
		if len(m) < 4 {
			continue
		}
		add(m, OriginCamelCase)
	}

	return out
}
