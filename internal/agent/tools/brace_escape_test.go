package tools

import (
	"strings"
	"testing"
)

// TestBraceEscapeNotice_FiresOnSvelteOverEscape covers the 2026-05-25
// chat-debug log pathology: AgentsView.svelte literally contained
// `{"{"}` byte sequences, and the model couldn't tell whether those
// were the file's actual content or display escapes the view tool
// had inserted.
func TestBraceEscapeNotice_FiresOnSvelteOverEscape(t *testing.T) {
	content := `<div class="time-axis-labels">
  {"{"}#each timeMarkers as min{"}"}
    <span>{"{"}min{"}"}m</span>
  {/each}
</div>`
	notice := braceEscapeNotice(content)
	if notice == "" {
		t.Fatal("expected notice for file with literal brace escapes")
	}
	for _, want := range []string{
		"ARE the file's actual content",
		"does NOT apply escaping",
		"Unexpected block closing tag",
		"Unexpected token",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q in:\n%s", want, notice)
		}
	}
}

func TestBraceEscapeNotice_EmptyForCleanFile(t *testing.T) {
	content := `<div class="thing">
  {#each items as item}
    <span>{item}</span>
  {/each}
</div>`
	if got := braceEscapeNotice(content); got != "" {
		t.Errorf("expected empty notice for clean Svelte file, got: %s", got)
	}
}

func TestBraceEscapeNotice_EmptyForNonSvelteFile(t *testing.T) {
	content := `package main
import "fmt"
func main() { fmt.Println("hello") }`
	if got := braceEscapeNotice(content); got != "" {
		t.Errorf("expected empty notice for non-Svelte content, got: %s", got)
	}
}

func TestBraceEscapeNotice_FiresOnClosingEscape(t *testing.T) {
	content := `<div>+ closing brace example: {"}"}+ here</div>`
	if got := braceEscapeNotice(content); got == "" {
		t.Error("expected notice for closing-brace escape pattern")
	}
}
