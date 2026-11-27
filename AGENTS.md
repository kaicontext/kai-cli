# AGENTS.md

How to write code and pull requests in kai-cli. These rules apply to coding agents and people alike.

## Code

### Pass dependencies in; don't connect code through global state

If a function needs an object, take it as a parameter. Don't link two pieces of code through a package-level variable or callback that one sets and the other reads. The call site then no longer shows what feeds what, and the code only works if the global is set and cleared in the right order.

```go
// Do
importProgressBar := newProgressBar("Importing git history", 20)
importProgressBar.start()
importErr := runGitImport(db, importProgressBar)
importProgressBar.finish(importErr)

// Don't
initProgress = importProgressBar.progress // read somewhere inside runGitImport
importErr := runGitImport(db)
initProgress = nil
```

When a signature is fixed (a cobra `RunE`, an interface method), keep it as a thin wrapper around a function that takes the extra parameter, as `runCapture` does with `runCaptureWithProgress`.

### Constructors build; methods act

A `newX` function returns a value and has no side effects: no output, no goroutines, no I/O. Starting the work is a separate method, such as `start`.

### Handle misuse in the code, not in a comment

If a type's methods can be called in an order that would panic or misbehave (finish before start, finish twice), make that order safe. Don't ship a known panic and document it. Cover those orders with tests.

### Name things for what they are

- Use the term for the thing itself. A progress bar is a `progressBar`, not a `step` or a `spinner`.
- A variable's name says what it holds: `importProgressBar`, `graphProgressBar`, not `bar` or `s`.
- No single-letter names, except where Go has an established convention: loop indices (`i`, `j`), short method receivers (`p *progressBar`), `t *testing.T`, `w io.Writer`.

### Name a result before passing it on

Assign a call's result to a named variable before passing it to another call, so the reader knows what comes back.

```go
// Do
importErr := runGitImport(db, importProgressBar)
importProgressBar.finish(importErr)

// Don't
importProgressBar.finish(runGitImport(db, importProgressBar))
```

### Don't drop errors silently

Handle every error a call returns. If ignoring one is genuinely safe, assign it to `_` and say why in a comment on the same line.

```go
// Do
if err := os.WriteFile(path, data, 0644); err != nil {
	return fmt.Errorf("writing %s: %w", path, err)
}
_ = os.Remove(tmpPath) // best-effort cleanup; a leftover temp file is harmless

// Don't
os.WriteFile(path, data, 0644)
msg, _ := readMessage()
```

### Look for an existing helper before writing one

Before writing something that sounds general-purpose (truncating or padding a string, formatting a duration, walking a tree, retrying a call), search for it: `git grep` this repo, then the Go standard library, then the kai-engine and kai-core modules kai-cli already imports. kai-cli has nine separate string-truncation functions; this rule is there to stop the tenth.

If nothing fits and the code is general-purpose, don't bury it in the feature that needs it. Put it in a package under `internal/` named for what it does, with an API that takes plain values (no feature types, no globals), so the next caller can use it without changing it.

### Keep files small and focused

One file covers one topic. Put a new feature in its own file instead of growing a large one; the init progress bar lives in `cmd/kai/init_progress.go`. Split a file when it covers several unrelated things or grows hard to read in one sitting.

### Separate concerns

Each type and function does one job. The progress bar knows how to show progress; the git import knows how to import and reports how far it has got; `runInit` wires the two together. A detail lives next to the code it describes: the fact that parsing is 80% of the graph build sits beside the parse callback in the capture, not in `runInit`.

## Pull requests

Keep the description proportional to the change: what it does, why, and how it was tested, in a few short bullets. Leave out anything a reviewer doesn't need to judge the change.
