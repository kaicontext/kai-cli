package planner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CommandIndex maps CLI subcommand names to their handler function
// names. Built by scanning the workspace's .go files for
// cobra.Command struct literals — the Use field gives the subcommand
// name, the Run or RunE field gives the handler. v1 ships cobra only;
// v1.x will add other CLI frameworks (urfave/cli, bubbletea
// commands, etc.) as observed in real users' codebases.
//
// The index is built lazily on first lookup and cached for the
// lifetime of the planner. Workspace changes during a planner run
// don't invalidate (rare; not worth the bookkeeping). Cost: walking
// .go files in cmd/ + scanning each — fast enough that we don't
// bother with persistence.
type CommandIndex struct {
	// commands maps the trailing subcommand (e.g. "code") to the
	// handler function name (e.g. "runCodeTUI"). Multi-word commands
	// like `kai gate review` are indexed by the trailing word
	// (review) because that's what disambiguates within the cobra
	// tree; matching from the binary name down would require
	// modeling the parent-child relationship, and the trailing-word
	// lookup is sufficient for the v1 success criterion.
	commands map[string]string

	// handlerFiles maps handler function names to the file they're
	// defined in. Populated alongside commands so the lookup
	// pipeline can return (command name → handler → file)
	// without a second pass.
	handlerFiles map[string]string
}

// cobraCommandRE matches the Use + Run(E) fields of a single
// cobra.Command struct literal. `[^{}]*` keeps the match bounded
// inside one struct — it won't cross into a nested literal or a
// later command. The Run/RunE ordering is unspecified in cobra, so
// we accept either form.
//
// Limitations:
//   - Cobra commands that use `Aliases:` instead of Use aren't picked
//     up. Rare in practice; the user types Use names.
//   - Multi-line Use values (rare — cobra uses one-word commands)
//     would miss; the inner pattern is `"[^"]+"` which doesn't
//     cross newlines because Go string literals don't either.
//   - Commands whose RunE is an inline function literal aren't
//     captured. Inline-RunE is uncommon; codebases that do it can
//     be added in v1.x.
var cobraCommandRE = regexp.MustCompile(
	`(?s)Use:\s*"([^"]+)"[^{}]*?Run(?:E)?:\s*(\w+)`,
)

// funcDefRE finds top-level function definitions: `func name(`.
// Used to locate the file each handler is defined in after the
// cobra scan identifies the handler names. Methods (with a
// receiver) are skipped — cobra handlers are top-level functions
// in idiomatic Go CLI code.
var funcDefRE = regexp.MustCompile(`(?m)^func\s+(\w+)\s*\(`)

// LoadCommandIndex scans the workspace for cobra.Command literals
// and returns a CommandIndex. Empty index (no commands found) is
// returned cleanly — callers should treat "command not found" as
// fall-through to the symbol/file lookup stages rather than as an
// error.
//
// Walks .go files under the workspace root. Caps file count to
// keep this cheap on huge codebases (the cmd/ directory of a CLI
// is rarely huge; if it is, the cap fires and we still get a
// reasonable index from the first N files in lexical order).
func LoadCommandIndex(workspaceRoot string) *CommandIndex {
	idx := &CommandIndex{
		commands:     map[string]string{},
		handlerFiles: map[string]string{},
	}
	const maxFiles = 500
	files := 0
	_ = filepath.Walk(workspaceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // tolerate unreadable subtrees
		}
		if info.IsDir() {
			// Skip vendor and hidden dirs — vendor/cobra would
			// double-match the framework's own cobra.Command
			// internal types if it ever defined any, and hidden
			// dirs (.git, .kai) are noise.
			base := info.Name()
			if base == "vendor" || (len(base) > 0 && base[0] == '.') {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if files >= maxFiles {
			return filepath.SkipAll
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)

		// Pass 1: collect cobra commands from this file.
		for _, m := range cobraCommandRE.FindAllStringSubmatch(text, -1) {
			use, handler := m[1], m[2]
			// cobra's Use field convention is `"<name> <arg-pattern>"`
			// (e.g. "review [snapshot-id]") — the command NAME is
			// the first space-separated word, and everything after
			// is argument documentation. Take the first word.
			if i := strings.IndexByte(use, ' '); i >= 0 {
				use = use[:i]
			}
			if use == "" || handler == "" {
				continue
			}
			if _, exists := idx.commands[use]; !exists {
				idx.commands[use] = handler
			}
		}

		// Pass 2: collect function-definition locations from this
		// same file. We don't know yet which functions are handlers
		// (the cobra scan above might have picked up handlers from
		// other files first), so record everything and resolve at
		// the end.
		for _, m := range funcDefRE.FindAllStringSubmatch(text, -1) {
			name := m[1]
			if _, exists := idx.handlerFiles[name]; !exists {
				idx.handlerFiles[name] = path
			}
		}
		return nil
	})
	return idx
}

// LookupCommand returns the handler function and defining file for
// the given subcommand name, or empty strings if not found. The
// subcommand name is matched against the trailing word of the Use
// field — `kai code` → "code", `kai gate review` → "review".
//
// Callers pass the raw token (e.g. "kai code") and this function
// strips the binary prefix automatically: any space-separated
// trailing word is treated as the subcommand. This matches what the
// user types in backticks.
func (c *CommandIndex) LookupCommand(token string) (handler, file string) {
	if c == nil {
		return "", ""
	}
	sub := token
	if i := strings.LastIndexByte(sub, ' '); i >= 0 {
		sub = sub[i+1:]
	}
	handler = c.commands[sub]
	if handler == "" {
		return "", ""
	}
	file = c.handlerFiles[handler]
	return handler, file
}
