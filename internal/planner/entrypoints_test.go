package planner

import (
	"reflect"
	"testing"
)

// TestExtractEntryPointTokens covers the four origin classes plus
// the negative case where plain English produces nothing. Each
// subtest names the user-facing scenario so a failure report points
// directly at the broken case.
func TestExtractEntryPointTokens(t *testing.T) {
	cases := []struct {
		name    string
		request string
		want    []EntryPointToken
	}{
		{
			name:    "empty input",
			request: "",
			want:    nil,
		},
		{
			name:    "plain english only",
			request: "the page is broken and the homepage opens the wrong project",
			want:    nil,
		},
		{
			name:    "backticked command beats camelcase",
			request: "I ran `kai code` and it opened the wrong project",
			want:    []EntryPointToken{{Raw: "kai code", Origin: OriginBacktick}},
		},
		{
			name:    "camelcase identifier",
			request: "runCodeTUI seems to be misbehaving",
			want:    []EntryPointToken{{Raw: "runCodeTUI", Origin: OriginCamelCase}},
		},
		{
			name:    "snake_case identifier",
			request: "extract_mutated_paths returns stale results",
			want:    []EntryPointToken{{Raw: "extract_mutated_paths", Origin: OriginSnakeCase}},
		},
		{
			name:    "path-shaped with extension",
			request: "look at set.go for the bug",
			want:    []EntryPointToken{{Raw: "set.go", Origin: OriginPath}},
		},
		{
			name:    "path-shaped with directory",
			request: "the bug is in internal/projects/discover.go somewhere",
			want:    []EntryPointToken{{Raw: "internal/projects/discover.go", Origin: OriginPath}},
		},
		{
			name:    "single-segment caps drop (acronym/shouty)",
			request: "URL handling broke and RUN failed",
			want:    nil,
		},
		{
			name:    "common 3-char tokens drop",
			request: "Run Get New",
			want:    nil,
		},
		{
			name:    "trailing punctuation stripped from paths",
			request: "check set.go, internal/x/y.go, please.",
			want: []EntryPointToken{
				{Raw: "set.go", Origin: OriginPath},
				{Raw: "internal/x/y.go", Origin: OriginPath},
			},
		},
		{
			name:    "backtick preserves punctuation inside",
			request: "the `Primary()` function fails",
			want:    []EntryPointToken{{Raw: "Primary()", Origin: OriginBacktick}},
		},
		{
			name:    "deduplicated",
			request: "the runCodeTUI calls runCodeTUI again somehow",
			want:    []EntryPointToken{{Raw: "runCodeTUI", Origin: OriginCamelCase}},
		},
		{
			name:    "mixed mentions",
			request: "I ran `kai code`, checked extract_paths in set.go, and saw runCodeTUI",
			want: []EntryPointToken{
				{Raw: "kai code", Origin: OriginBacktick},
				{Raw: "set.go", Origin: OriginPath},
				{Raw: "extract_paths", Origin: OriginSnakeCase},
				{Raw: "runCodeTUI", Origin: OriginCamelCase},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractEntryPointTokens(c.request)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ExtractEntryPointTokens(%q)\n  got:  %+v\n  want: %+v", c.request, got, c.want)
			}
		})
	}
}

// TestHasUpperLowerTransition guards the helper that excludes
// shouty acronyms from camelCase matching.
func TestHasUpperLowerTransition(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false},
		{"open", false},
		{"OPEN", false},
		{"Open", false},
		{"openClose", true},
		{"runCodeTUI", true},
		{"URLParser", true}, // L→P is the transition
		{"URL", false},
	}
	for _, c := range cases {
		got := hasUpperLowerTransition(c.s)
		if got != c.want {
			t.Errorf("hasUpperLowerTransition(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
