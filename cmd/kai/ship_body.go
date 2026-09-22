package main

// The pull request description `kai ship` opens a PR with.
//
// It used to be a metadata table — branch, session, file count, a list of
// paths — under "Shipped from a kai session.", and the server prepended a
// "What changed" block whose only bullet was the title ("ship: kai/s-…").
// A reviewer arrived at a PR that said where the change came from and
// never what it was (kaicontext/kai-desktop#440).
//
// Now the description leads with what the change does. The agent that did
// the work writes it (--body / --body-file: what and why, the changes by
// area, how it was verified); without one, it is assembled from what the
// session recorded — the title, its commits' subjects and bodies — and the
// files it touched with their line counts. The provenance table moves into
// a collapsed "Kai details" block at the end.

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// shipSummaryMarker tells the server this body already opens with a
// description, so it does not prepend its own generic "What changed" block
// on top of one.
const shipSummaryMarker = "<!-- kai-ship:summary -->"

// shipBodyFileCap bounds how many files the description lists by name; the
// rest are counted. A 200-file PR's full path list is noise at the top of a
// review, and the Files tab has it anyway.
const shipBodyFileCap = 25

// shipCommitNote is one of the session's own commits: its subject (the
// part's one-line intent) and its body (what was verified and how).
type shipCommitNote struct {
	Subject string
	Body    string
}

// shipFileStat is one changed file with its line counts. Known is false
// when git could not count it (a binary, or a path it has no record of),
// in which case the counts are not shown rather than shown as zero.
type shipFileStat struct {
	Path           string
	Added, Removed int
	Known          bool
}

// shipBodyInput is everything the description is built from.
type shipBodyInput struct {
	Branch, SessionID, SnapHex string
	// Title is the PR title; an opaque one ("ship: kai/…") is ignored.
	Title string
	// Authored is the agent's own description (--body / --body-file).
	Authored string
	Commits  []shipCommitNote
	Files    []shipFileStat
	// KnownIssues is the review-ledger section, already rendered.
	KnownIssues string
}

// shipAuthoredBody reads the description the caller supplied: --body as
// given, or --body-file's contents ("-" is stdin). "" when neither was set.
func shipAuthoredBody(body, bodyFile string, stdin io.Reader) (string, error) {
	if body != "" && bodyFile != "" {
		return "", fmt.Errorf("--body and --body-file are mutually exclusive")
	}
	if bodyFile == "" {
		return strings.TrimSpace(body), nil
	}
	var raw []byte
	var err error
	if bodyFile == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	} else {
		raw, err = os.ReadFile(bodyFile)
	}
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// shipPRBody composes the description. The opening is the authored text or,
// without one, a summary assembled from the title and commits; then the
// files with their line counts; then the provenance, collapsed.
func shipPRBody(in shipBodyInput) string {
	var b strings.Builder
	b.WriteString(shipSummaryMarker + "\n")
	authored := strings.TrimSpace(in.Authored)
	if authored != "" {
		b.WriteString(authored)
		b.WriteString("\n\n")
	} else {
		b.WriteString(shipGeneratedSummary(in))
	}

	// The file list is part of the description when nothing else says what
	// changed; beside an authored description it is detail, so it folds.
	files := shipFilesSection(in.Files)
	if authored == "" && files != "" {
		b.WriteString("## Files\n\n")
		b.WriteString(files)
		b.WriteString("\n")
	}

	b.WriteString("<details><summary>Kai details</summary>\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n| Branch | `%s` |\n", in.Branch)
	if in.SessionID != "" {
		fmt.Fprintf(&b, "| Session | `%s` |\n", in.SessionID)
	}
	if in.SnapHex != "" {
		fmt.Fprintf(&b, "| Snapshot | `%s` |\n", in.SnapHex)
	}
	if authored != "" && files != "" {
		b.WriteString("\n")
		b.WriteString(files)
	}
	b.WriteString("\n</details>\n")
	if in.KnownIssues != "" {
		b.WriteString(in.KnownIssues)
	}
	b.WriteString("\n<!-- kai-ship -->\n")
	return b.String()
}

// shipGeneratedSummary is the opening when the agent wrote none: the title
// as a sentence, the commits' own explanations beneath it, and — when the
// session committed in more than one step — the steps as a list.
func shipGeneratedSummary(in shipBodyInput) string {
	var b strings.Builder
	b.WriteString("## What this does\n\n")
	title := strings.TrimSpace(in.Title)
	if strings.HasPrefix(strings.ToLower(title), "ship: kai/") {
		title = ""
	}
	wrote := false
	if title != "" {
		b.WriteString(strings.TrimRight(title, ".") + ".\n\n")
		wrote = true
	}
	for _, c := range in.Commits {
		if body := strings.TrimSpace(c.Body); body != "" {
			b.WriteString(body + "\n\n")
			wrote = true
		}
	}
	var steps []string
	for _, c := range in.Commits {
		if s := strings.TrimSpace(c.Subject); s != "" && !strings.EqualFold(strings.TrimRight(s, "."), strings.TrimRight(title, ".")) {
			steps = append(steps, s)
		}
	}
	if len(steps) > 0 && (len(in.Commits) > 1 || title != "") {
		b.WriteString("## Changes\n\n")
		for _, s := range steps {
			b.WriteString("- " + s + "\n")
		}
		b.WriteString("\n")
		wrote = true
	}
	if !wrote {
		// Nothing recorded says what the change is; say what it touched.
		b.WriteString(shipScopeSentence(in.Files) + "\n\n")
	}
	return b.String()
}

// shipScopeSentence names what a change touched when nothing names what it
// does: "Changes 3 files under `frontend/dist`."
func shipScopeSentence(files []shipFileStat) string {
	if len(files) == 0 {
		return "No files changed."
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	if dir := shipCommonDir(files); dir != "" {
		return fmt.Sprintf("Changes %d %s under `%s`.", len(files), noun, dir)
	}
	return fmt.Sprintf("Changes %d %s.", len(files), noun)
}

// shipCommonDir is the deepest directory every file sits under, "" when
// they share none.
func shipCommonDir(files []shipFileStat) string {
	common := ""
	for i, f := range files {
		dir := path.Dir(f.Path)
		if dir == "." {
			return ""
		}
		if i == 0 {
			common = dir
			continue
		}
		for common != "." && common != "/" && dir != common && !strings.HasPrefix(dir, common+"/") {
			common = path.Dir(common)
		}
		if common == "." || common == "/" {
			return ""
		}
	}
	return common
}

// shipFilesSection lists the files with their line counts and a total,
// capped at shipBodyFileCap names. "" when no files changed.
func shipFilesSection(files []shipFileStat) string {
	if len(files) == 0 {
		return ""
	}
	added, removed := 0, 0
	for _, f := range files {
		added += f.Added
		removed += f.Removed
	}
	var b strings.Builder
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	fmt.Fprintf(&b, "**%d %s** · +%d −%d\n\n", len(files), noun, added, removed)
	for i, f := range files {
		if i == shipBodyFileCap {
			fmt.Fprintf(&b, "- …and %d more\n", len(files)-shipBodyFileCap)
			break
		}
		if f.Known {
			fmt.Fprintf(&b, "- `%s` (+%d −%d)\n", f.Path, f.Added, f.Removed)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", f.Path)
		}
	}
	return b.String()
}

// shipFileStats counts each changed path's lines against base with
// `git diff --numstat`. A file git does not track yet (new and never added)
// has no numstat line; if it can be read, all its lines are additions.
// Best-effort throughout: an error only costs the counts.
func shipFileStats(cwd, base string, paths []string, staged bool) []shipFileStat {
	stats := map[string]shipFileStat{}
	if len(paths) > 0 {
		args := []string{"diff", "--numstat", "--no-renames"}
		if staged {
			args = append(args, "--staged")
		}
		args = append(args, base, "--")
		args = append(args, paths...)
		if out, err := gitOut(cwd, args...); err == nil {
			for _, line := range strings.Split(out, "\n") {
				f := strings.SplitN(line, "\t", 3)
				if len(f) != 3 {
					continue
				}
				a, aerr := strconv.Atoi(f[0])
				d, derr := strconv.Atoi(f[1])
				stats[f[2]] = shipFileStat{Path: f[2], Added: a, Removed: d, Known: aerr == nil && derr == nil}
			}
		}
	}
	out := make([]shipFileStat, 0, len(paths))
	for _, p := range paths {
		if s, ok := stats[p]; ok {
			out = append(out, s)
			continue
		}
		s := shipFileStat{Path: p}
		if raw, err := os.ReadFile(path.Join(cwd, p)); err == nil && !strings.ContainsRune(string(raw), 0) {
			s.Added = strings.Count(string(raw), "\n")
			if len(raw) > 0 && raw[len(raw)-1] != '\n' {
				s.Added++
			}
			s.Known = true
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// shipSessionCommits is the session's own commits in a spawned workspace,
// oldest first: everything past the spawn baseline except kai's
// bookkeeping. nil outside a spawn, or before the session has committed.
func shipSessionCommits(cwd string) []shipCommitNote {
	if shipSpawnEntry(cwd) == nil {
		return nil
	}
	base := shipBaselineCommit(cwd)
	if base == "" || base == "HEAD" {
		return nil
	}
	out, err := gitOut(cwd, "log", "--reverse", "--format=%s%x1f%b%x1e", base+"..HEAD")
	if err != nil {
		return nil
	}
	var notes []shipCommitNote
	for _, rec := range strings.Split(out, "\x1e") {
		subject, body, _ := strings.Cut(strings.TrimLeft(rec, "\n"), "\x1f")
		subject = strings.TrimSpace(subject)
		if subject == "" || shipIsBookkeepingCommit(subject) {
			continue
		}
		notes = append(notes, shipCommitNote{Subject: subject, Body: shipCommitProse(body)})
	}
	return notes
}

// shipIsBookkeepingCommit marks commits kai makes for itself — the spawn
// baseline, warm syncs, merges from main, an earlier ship's fallback — which
// say nothing about what the session changed.
func shipIsBookkeepingCommit(subject string) bool {
	low := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(low, "kai spawn from") || strings.HasPrefix(low, "kai warm sync") ||
		shipIsMergeSubject(low) || strings.HasPrefix(low, "ship: kai/")
}

// shipCommitProse is a commit body without its trailer block (Kai-Session:,
// Kai-Snapshot:, Co-authored-by: …), which is provenance, not explanation.
func shipCommitProse(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	end := len(lines)
	for end > 0 {
		l := strings.TrimSpace(lines[end-1])
		if l == "" {
			end--
			continue
		}
		if k, _, ok := strings.Cut(l, ":"); ok && k != "" && !strings.ContainsAny(k, " \t") && strings.Contains(k, "-") {
			end--
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}
