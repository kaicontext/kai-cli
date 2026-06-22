package tools

import (
	"strings"
	"testing"
)

func TestFormatWebResults_BasicShape(t *testing.T) {
	results := []kaiWebSearchResult{
		{Title: "Svelte 5 is out", URL: "https://svelte.dev/blog/svelte-5", Snippet: "Released 2024-10-22."},
		{Title: "Svelte 4 changelog", URL: "https://github.com/sveltejs/svelte/blob/main/CHANGELOG.md", Description: "fallback description"},
	}
	got := formatWebResults("svelte latest", results)
	for _, want := range []string{
		`Web search results for "svelte latest" (2):`,
		"1. Svelte 5 is out",
		"https://svelte.dev/blog/svelte-5",
		"Released 2024-10-22.",
		"2. Svelte 4 changelog",
		"fallback description",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted output missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatWebResults_SnippetCapped(t *testing.T) {
	long := strings.Repeat("x", snippetCap+200)
	results := []kaiWebSearchResult{{Title: "t", URL: "u", Snippet: long}}
	got := formatWebResults("q", results)
	// Should be capped to snippetCap with a trailing ellipsis.
	if strings.Contains(got, long) {
		t.Errorf("uncapped snippet leaked into output (len=%d)", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation marker, got:\n%s", got)
	}
}

func TestFormatWebResults_FallsBackToDescription(t *testing.T) {
	// When Snippet is empty, Description is used.
	results := []kaiWebSearchResult{{Title: "t", URL: "u", Description: "from-description"}}
	got := formatWebResults("q", results)
	if !strings.Contains(got, "from-description") {
		t.Errorf("description fallback missing in:\n%s", got)
	}
}

func TestFormatWebResults_HandlesMissingTitle(t *testing.T) {
	results := []kaiWebSearchResult{{URL: "https://x", Snippet: "s"}}
	got := formatWebResults("q", results)
	if !strings.Contains(got, "(untitled)") {
		t.Errorf("untitled placeholder missing in:\n%s", got)
	}
}
