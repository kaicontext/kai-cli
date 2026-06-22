// Package tasksmd reads, parses, and writes the workspace TASKS.md
// ledger — a Markdown file that survives /clear and process restarts
// and serves as the authoritative source for "what is in flight,
// what is queued, what is done." See docs/tasks-md-spec.md.
//
// Scope: pure file I/O. No graph, no MCP, no network. Failures are
// silent: a missing or malformed file degrades to the zero value, so
// callers can always inject the result into a prompt without first
// checking error.
package tasksmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Task is one entry in the ledger.
type Task struct {
	Subject    string
	Body       string   // free-form body lines minus structured fields
	Files      []string // parsed from "Files: a, b, c"
	Acceptance string   // parsed from "Acceptance: ..."
	DoneDate   string   // "YYYY-MM-DD", set only for Done items
}

// Tasks is the parsed ledger. Path is the absolute path the ledger
// was loaded from, or "" when no conformant file was found.
type Tasks struct {
	Path       string
	InProgress []Task
	Pending    []Task
	Done       []Task
}

// Load reads TASKS.md (or .kai/tasks.md) from workDir. Returns
// (zero value, nil) when no conformant file is present. Only returns
// an error on a true I/O failure against a file we already know
// exists.
func Load(workDir string) (Tasks, error) {
	if workDir == "" {
		return Tasks{}, nil
	}
	path, data, err := readLedger(workDir)
	if err != nil {
		return Tasks{}, err
	}
	if data == nil {
		return Tasks{}, nil
	}
	if !hasTasksHeading(data) {
		warnOnce(path, "missing top-level '# Tasks' heading")
		return Tasks{}, nil
	}
	t := parse(string(data))
	t.Path = path
	return t, nil
}

// FormatForPrompt renders the In progress + Pending sections as a
// compact block suitable for injection into a per-turn user message.
// Returns "" when both sections are empty. Done items are
// intentionally omitted — they are already in git history and would
// dilute the active scope.
func (t Tasks) FormatForPrompt() string {
	if len(t.InProgress) == 0 && len(t.Pending) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Workspace task ledger (TASKS.md). This file is authoritative for what is in flight and what is queued. If the user refers to \"the task\" or \"the next task\" without naming one, use this list.")
	if len(t.InProgress) > 0 {
		b.WriteString("\n\n## In progress\n")
		for _, it := range t.InProgress {
			b.WriteString("- ")
			b.WriteString(it.Subject)
			writeTaskBody(&b, it, "  ")
		}
	}
	if len(t.Pending) > 0 {
		b.WriteString("\n\n## Pending\n")
		for i, it := range t.Pending {
			fmt.Fprintf(&b, "%d. %s", i+1, it.Subject)
			writeTaskBody(&b, it, "   ")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeTaskBody renders the Files / Acceptance / free-form body
// lines below a task subject. indent is the prefix that aligns the
// child lines under the bullet (two spaces for "- ", three for "N. ").
func writeTaskBody(b *strings.Builder, t Task, indent string) {
	b.WriteByte('\n')
	if len(t.Files) > 0 {
		fmt.Fprintf(b, "%sFiles: %s\n", indent, strings.Join(t.Files, ", "))
	}
	if t.Acceptance != "" {
		fmt.Fprintf(b, "%sAcceptance: %s\n", indent, t.Acceptance)
	}
	if t.Body != "" {
		for _, line := range strings.Split(strings.TrimRight(t.Body, "\n"), "\n") {
			fmt.Fprintf(b, "%s%s\n", indent, line)
		}
	}
}

// Save writes the ledger back to t.Path in canonical form. Refuses
// when Path is empty (nothing to update — caller didn't Load from a
// real file). Writes via temp-file + rename so a crash mid-write
// can't truncate the user's existing ledger.
func (t Tasks) Save() error {
	if t.Path == "" {
		return fmt.Errorf("tasksmd: Save called on Tasks with empty Path")
	}
	out := t.render()
	dir := filepath.Dir(t.Path)
	tmp, err := os.CreateTemp(dir, ".tasksmd-*")
	if err != nil {
		return fmt.Errorf("tasksmd: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("tasksmd: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("tasksmd: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, t.Path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("tasksmd: rename: %w", err)
	}
	return nil
}

// AddPending appends a new Pending task with the given subject to the
// workspace ledger and saves it, returning the resulting Pending
// count. When no conformant ledger exists yet it creates one at
// <workDir>/TASKS.md (render always emits the canonical heading +
// sections). Used by the TUI to capture mid-run user input as a task
// the running agent picks up on its next turn (TASKS.md is reloaded
// and injected every turn) instead of an in-memory queue.
func AddPending(workDir, subject string) (int, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return 0, fmt.Errorf("tasksmd: empty task subject")
	}
	t, err := Load(workDir)
	if err != nil {
		return 0, err
	}
	if t.Path == "" {
		t.Path = filepath.Join(workDir, "TASKS.md")
	}
	t.Pending = append(t.Pending, Task{Subject: subject})
	if err := t.Save(); err != nil {
		return 0, err
	}
	return len(t.Pending), nil
}

// render produces the canonical on-disk form. Always emits sections
// in the spec order (In progress, Pending, Done). Empty sections are
// still emitted as headers so a hand-editor can fill them in without
// guessing the format.
func (t Tasks) render() string {
	var b strings.Builder
	b.WriteString("# Tasks\n\n## In progress\n")
	for _, it := range t.InProgress {
		writeDiskTask(&b, it, false)
	}
	b.WriteString("\n## Pending\n")
	for _, it := range t.Pending {
		writeDiskTask(&b, it, false)
	}
	b.WriteString("\n## Done\n")
	for _, it := range t.Done {
		writeDiskTask(&b, it, true)
	}
	return b.String()
}

func writeDiskTask(b *strings.Builder, t Task, done bool) {
	box := "[ ]"
	subject := t.Subject
	if done {
		box = "[x]"
		if t.DoneDate != "" {
			subject = fmt.Sprintf("%s (%s)", subject, t.DoneDate)
		}
	}
	fmt.Fprintf(b, "- %s %s\n", box, subject)
	const indent = "  "
	if len(t.Files) > 0 {
		fmt.Fprintf(b, "%sFiles: %s\n", indent, strings.Join(t.Files, ", "))
	}
	if t.Acceptance != "" {
		fmt.Fprintf(b, "%sAcceptance: %s\n", indent, t.Acceptance)
	}
	if t.Body != "" {
		for _, line := range strings.Split(strings.TrimRight(t.Body, "\n"), "\n") {
			fmt.Fprintf(b, "%s%s\n", indent, line)
		}
	}
}

// readLedger probes the two known locations and returns the first
// match. workDir/TASKS.md wins over workDir/.kai/tasks.md so the
// in-repo ledger is canonical when both exist (and a user who keeps
// a personal scratch file at .kai/tasks.md isn't surprised by the
// project's ledger silently being shadowed).
func readLedger(workDir string) (path string, data []byte, err error) {
	primary := filepath.Join(workDir, "TASKS.md")
	if d, e := os.ReadFile(primary); e == nil {
		return primary, d, nil
	} else if !os.IsNotExist(e) {
		return "", nil, e
	}
	secondary := filepath.Join(workDir, ".kai", "tasks.md")
	if d, e := os.ReadFile(secondary); e == nil {
		return secondary, d, nil
	} else if !os.IsNotExist(e) {
		return "", nil, e
	}
	return "", nil, nil
}

func hasTasksHeading(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		// First non-empty line must be the heading.
		return strings.EqualFold(l, "# Tasks")
	}
	return false
}

// parse runs a one-pass line scanner over the ledger. Recognizes the
// three section headers, GFM task items, and the structured Files: /
// Acceptance: prefixes inside indented body blocks. Tolerant of
// section reordering, extra blank lines, and case variation in headers.
func parse(src string) Tasks {
	var t Tasks
	var section string // "inprogress" | "pending" | "done" | ""
	lines := strings.Split(src, "\n")

	flush := func(cur *Task) {
		if cur == nil || cur.Subject == "" {
			return
		}
		switch section {
		case "inprogress":
			t.InProgress = append(t.InProgress, *cur)
		case "pending":
			t.Pending = append(t.Pending, *cur)
		case "done":
			t.Done = append(t.Done, *cur)
		}
	}

	var cur *Task
	var bodyLines []string

	finalizeBody := func() {
		if cur == nil {
			return
		}
		// Walk bodyLines and extract structured fields. Lines that
		// don't match a known prefix accumulate into Body verbatim.
		var freeform []string
		for _, ln := range bodyLines {
			trimmed := strings.TrimSpace(ln)
			low := strings.ToLower(trimmed)
			switch {
			case strings.HasPrefix(low, "files:"):
				val := strings.TrimSpace(trimmed[len("files:"):])
				cur.Files = splitCSV(val)
			case strings.HasPrefix(low, "acceptance:"):
				val := strings.TrimSpace(trimmed[len("acceptance:"):])
				cur.Acceptance = val
			default:
				freeform = append(freeform, trimmed)
			}
		}
		cur.Body = strings.Join(freeform, "\n")
		bodyLines = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		low := strings.ToLower(trimmed)
		switch {
		case low == "## in progress":
			finalizeBody()
			flush(cur)
			cur = nil
			section = "inprogress"
		case low == "## pending":
			finalizeBody()
			flush(cur)
			cur = nil
			section = "pending"
		case low == "## done":
			finalizeBody()
			flush(cur)
			cur = nil
			section = "done"
		case strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "- [x]") || strings.HasPrefix(trimmed, "- [X]"):
			finalizeBody()
			flush(cur)
			done := !strings.HasPrefix(trimmed, "- [ ]")
			subject := strings.TrimSpace(trimmed[len("- [ ]"):])
			task := Task{}
			if done && section == "done" {
				task.Subject, task.DoneDate = splitDoneSubject(subject)
			} else {
				task.Subject = subject
			}
			cur = &task
		case cur != nil && strings.HasPrefix(line, "  ") && trimmed != "":
			// Indented body line (2+ spaces).
			bodyLines = append(bodyLines, line[2:])
		case trimmed == "":
			// Blank line: do not close the task — bodyLines may still
			// continue after a stylistic blank. The next non-indented
			// non-empty line (a new item or section header) will
			// close it.
		case strings.HasPrefix(trimmed, "#"):
			// Heading other than the recognized sections (e.g. the
			// top-level "# Tasks" or a stray subsection). Close the
			// current item to avoid eating the heading into its body.
			finalizeBody()
			flush(cur)
			cur = nil
		default:
			// Non-indented non-bullet text — closes the current item.
			// Stray prose lives at the file level, not as task body.
			finalizeBody()
			flush(cur)
			cur = nil
		}
	}
	finalizeBody()
	flush(cur)
	return t
}

// splitDoneSubject pulls a trailing "(YYYY-MM-DD)" date off a Done
// item's subject. The date stays in DoneDate; everything before the
// final " (" goes back as Subject. Tolerates missing date.
func splitDoneSubject(s string) (subject, date string) {
	if !strings.HasSuffix(s, ")") {
		return s, ""
	}
	open := strings.LastIndex(s, " (")
	if open < 0 {
		return s, ""
	}
	candidate := s[open+2 : len(s)-1]
	// A YYYY-MM-DD has length 10 and digits/dashes only. Anything
	// else is part of the subject (e.g. "fix bug (#123)").
	if len(candidate) != 10 || candidate[4] != '-' || candidate[7] != '-' {
		return s, ""
	}
	for i, r := range candidate {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return s, ""
		}
	}
	return s[:open], candidate
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// warnOnce emits a single warning per (process, path) for
// malformed ledgers so we don't spam the user across every turn.
var (
	warnedMu   sync.Mutex
	warnedSeen = map[string]bool{}
)

func warnOnce(path, reason string) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warnedSeen[path] {
		return
	}
	warnedSeen[path] = true
	fmt.Fprintf(os.Stderr, "kai: TASKS.md at %s ignored: %s\n", path, reason)
}
