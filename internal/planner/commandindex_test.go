package planner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadCommandIndex_FindsKaiCode is the regression: scanning the
// kai-cli source must produce a "code" → "runCodeTUI" entry, with
// the handler file resolving to cmd/kai/tui.go. This is the actual
// data the v1 success criterion depends on — if this passes, the
// kai code bug's call chain becomes resolvable.
func TestLoadCommandIndex_FindsKaiCode(t *testing.T) {
	// kai-cli root: this test file lives at
	// internal/planner/commandindex_test.go, so two levels up is the
	// module root.
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	idx := LoadCommandIndex(moduleRoot)

	handler, file := idx.LookupCommand("kai code")
	if handler != "runCodeTUI" {
		t.Errorf("LookupCommand(\"kai code\") handler = %q, want runCodeTUI", handler)
	}
	if !filepath.IsAbs(file) || filepath.Base(file) != "tui.go" {
		t.Errorf("LookupCommand(\"kai code\") file = %q, want absolute path ending in tui.go", file)
	}
}

// TestLoadCommandIndex_HandlesSubcommandTree verifies the trailing-
// word indexing for nested commands. cobra registers gate review as
// `Use: "review"` on a child of gateCmd. Looking up "kai gate
// review" or just "review" should both resolve.
func TestLoadCommandIndex_HandlesSubcommandTree(t *testing.T) {
	moduleRoot, _ := filepath.Abs("../..")
	idx := LoadCommandIndex(moduleRoot)

	cases := []struct {
		token string
		want  string // expected handler-name prefix; nil-want means just non-empty
	}{
		{"review", ""},
		{"kai gate review", ""},
		{"approve", ""},
	}
	for _, c := range cases {
		t.Run(c.token, func(t *testing.T) {
			h, _ := idx.LookupCommand(c.token)
			if h == "" {
				t.Errorf("LookupCommand(%q) returned empty handler — gate command tree must be indexed", c.token)
			}
		})
	}
}

// TestLoadCommandIndex_MinimalFixture is a self-contained sanity
// test using a fabricated tiny Go file with one cobra command. Lets
// future maintainers verify the regex isolated from the real kai-cli
// source, which is large and changes frequently.
func TestLoadCommandIndex_MinimalFixture(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import "github.com/spf13/cobra"

var fooCmd = &cobra.Command{
	Use:   "foo",
	Short: "do the foo thing",
	RunE:  runFoo,
}

func runFoo(cmd *cobra.Command, args []string) error {
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := LoadCommandIndex(dir)
	handler, file := idx.LookupCommand("foo")
	if handler != "runFoo" {
		t.Errorf("handler = %q, want runFoo", handler)
	}
	if filepath.Base(file) != "main.go" {
		t.Errorf("file = %q, want main.go", file)
	}
}

// TestLookupCommand_NilSafe documents the contract that lookup
// against a nil index returns empty strings rather than panicking.
// The planner pipeline relies on this so the absence of a command
// index (e.g., from a test that didn't build one) falls through
// cleanly to the symbol/file stages.
func TestLookupCommand_NilSafe(t *testing.T) {
	var idx *CommandIndex
	h, f := idx.LookupCommand("anything")
	if h != "" || f != "" {
		t.Errorf("nil index lookup returned non-empty (%q, %q)", h, f)
	}
}
