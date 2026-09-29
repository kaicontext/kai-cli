package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/tools"
)

// Read-only repository access for the publication gate.
//
// The gate used to see only what the reviewer happened to open. An allegation
// that needed one more file — the caller, the definition, the sibling
// implementation — could be neither supported nor refuted, so it ended
// "unresolved", and a sweep proposal in that state is withheld. On the
// 2026-09-28 run 5 benchmark that was a standing cause of lost findings. The
// gate now gets kai_view and kai_grep over the REVIEWED COMMIT (git show /
// git grep at the hash, never the working copy), and every result becomes a
// numbered source it cites like any other. Nothing executes.
//
// The tools reuse the reviewer's names and output shape on purpose: a
// kai_view result prints "N: text" file lines, so rcToolSource maps it to file
// coordinates exactly as it does the reviewer's own views.

// rcRepo is the reviewed tree and the lines the change touched.
type rcRepo struct {
	hash    string
	changed map[string][][2]int // path → changed line ranges in the new file
}

type rcRepoKey struct{}

// rcWithRepo makes repo available to the gate further down this context. The
// fast pass never sets it: its budget has no room for lookups.
func rcWithRepo(ctx context.Context, repo *rcRepo) context.Context {
	if repo == nil {
		return ctx
	}
	return context.WithValue(ctx, rcRepoKey{}, repo)
}

func rcRepoFrom(ctx context.Context) *rcRepo {
	repo, _ := ctx.Value(rcRepoKey{}).(*rcRepo)
	return repo
}

// rcNewRepo reads the changed line ranges of the reviewed diff (the same range
// rcCommitDiff shows the reviewer). A failure leaves the ranges empty; the
// tools still work.
func rcNewRepo(base, ref, hash string) *rcRepo {
	var out []byte
	var err error
	if strings.TrimSpace(base) != "" {
		out, err = exec.Command("git", "--no-pager", "diff", "--no-color", "--unified=0", base+"..."+ref).Output()
	} else {
		out, err = exec.Command("git", "--no-pager", "show", "--no-color", "--unified=0", "--format=", ref).Output()
	}
	repo := &rcRepo{hash: hash, changed: map[string][][2]int{}}
	if err == nil {
		repo.changed = rcChangedRanges(string(out))
	}
	return repo
}

var rcHunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// rcChangedRanges parses a --unified=0 diff into the new-file line ranges each
// hunk touches. A pure deletion is recorded as the one line it sits at, so a
// defect beside removed code still counts as near the change.
func rcChangedRanges(diff string) map[string][][2]int {
	ranges := map[string][][2]int{}
	file := ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
			continue
		}
		m := rcHunkHeader.FindStringSubmatch(line)
		if m == nil || file == "" {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		end := start + count - 1
		if count == 0 {
			end = start
		}
		ranges[file] = append(ranges[file], [2]int{start, end})
	}
	return ranges
}

// rcNearChangeRadius is how close to a changed line counts as the change's own
// code for the "pre-existing" check: roughly one function around the edit.
const rcNearChangeRadius = 20

// changedNear reports whether path:line is within radius lines of a line the
// change touched, and returns that range for the message.
func (r *rcRepo) changedNear(file string, line, radius int) ([2]int, bool) {
	if r == nil {
		return [2]int{}, false
	}
	file = r.changedPath(file)
	for _, rg := range r.changed[file] {
		if line >= rg[0]-radius && line <= rg[1]+radius {
			return rg, true
		}
	}
	return [2]int{}, false
}

// changedPath resolves a path as the reviewer wrote it — often a bare or
// partial name — to the one changed file it names, the way rcResolvePath does
// against the whole tree. Only changed files matter here, so only they are
// searched; an ambiguous name is left as written.
func (r *rcRepo) changedPath(written string) string {
	if _, ok := r.changed[written]; ok {
		return written
	}
	match := ""
	for p := range r.changed {
		if strings.HasSuffix(p, "/"+written) {
			if match != "" {
				return written
			}
			match = p
		}
	}
	if match == "" {
		return written
	}
	return match
}

// Bounds on the gate's lookups.
const (
	rcMaxLookups        = 6
	rcRepoViewDefault   = 200
	rcRepoViewMax       = 400
	rcRepoGrepMaxLines  = 100
	rcRepoGrepMaxBytes  = 16 << 10
	rcRepoGrepReadLimit = 1 << 20
)

func rcRepoViewToolInfo() tools.ToolInfo {
	return tools.ToolInfo{Name: "kai_view", Description: "Read a file of the reviewed commit. Returns its lines as \"N: text\" with the file's own line numbers. Read-only.",
		Parameters: map[string]any{
			"file_path": map[string]any{"type": "string", "description": "repository-relative path"},
			"offset":    map[string]any{"type": "integer", "description": "lines to skip before the first one returned (0 = start of file)"},
			"limit":     map[string]any{"type": "integer", "description": fmt.Sprintf("lines to return (default %d, at most %d)", rcRepoViewDefault, rcRepoViewMax)},
		},
		Required: []string{"file_path"}}
}

func rcRepoGrepToolInfo() tools.ToolInfo {
	return tools.ToolInfo{Name: "kai_grep", Description: "Search the reviewed commit. Returns path:line:text matches. Read-only.",
		Parameters: map[string]any{
			"query": map[string]any{"type": "string", "description": "text to find (a fixed string unless regex is true)"},
			"path":  map[string]any{"type": "string", "description": "optional path or directory to limit the search to"},
			"regex": map[string]any{"type": "boolean", "description": "treat query as an extended regular expression"},
		},
		Required: []string{"query"}}
}

// rcRepoPath validates a repository-relative path: no absolute paths, no
// escaping the tree, no options smuggled into git's argument list.
func rcRepoPath(p string) (string, error) {
	p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "./"))
	if p == "" {
		return "", errors.New("no path given")
	}
	clean := path.Clean(p)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "-") {
		return "", fmt.Errorf("%q is not a path inside the repository", p)
	}
	return clean, nil
}

// rcRepoGrepTimeoutVar bounds one gate search (a var so tests can shorten it).
var rcRepoGrepTimeoutVar = 15 * time.Second

// view is kai_view over the reviewed commit.
func (r *rcRepo) view(input string) (string, error) {
	var args struct {
		FilePath string          `json:"file_path"`
		Offset   json.RawMessage `json:"offset"`
		Limit    json.RawMessage `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", fmt.Errorf("arguments could not be read: %v", err)
	}
	file, err := rcRepoPath(args.FilePath)
	if err != nil {
		return "", err
	}
	offset, err := rcViewOffset(args.Offset)
	if err != nil {
		return "", err
	}
	limit, err := rcViewOffset(args.Limit)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = rcRepoViewDefault
	}
	limit = min(limit, rcRepoViewMax)
	lines, ok := rcFileLines(r.hash, file)
	if !ok {
		return "", fmt.Errorf("%s does not exist at %s", file, rcShort(r.hash))
	}
	if offset >= len(lines) {
		return "", fmt.Errorf("%s has %d lines; offset %d is past the end", file, len(lines), offset)
	}
	end := min(offset+limit, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "%s at %s (lines %d-%d of %d)\n", file, rcShort(r.hash), offset+1, end, len(lines))
	for i := offset; i < end; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "(truncated; %d more lines — call again with offset %d)\n", len(lines)-end, end)
	}
	return b.String(), nil
}

// grep is kai_grep over the reviewed commit, bounded in time and output.
func (r *rcRepo) grep(input string) (string, error) {
	var args struct {
		Query string `json:"query"`
		Path  string `json:"path"`
		Regex bool   `json:"regex"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", fmt.Errorf("arguments could not be read: %v", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errors.New("empty query")
	}
	gitArgs := []string{"grep", "-n", "-I", "--no-color"}
	if args.Regex {
		gitArgs = append(gitArgs, "-E")
	} else {
		gitArgs = append(gitArgs, "--fixed-strings")
	}
	gitArgs = append(gitArgs, "-e", args.Query, r.hash)
	if strings.TrimSpace(args.Path) != "" {
		p, err := rcRepoPath(args.Path)
		if err != nil {
			return "", err
		}
		gitArgs = append(gitArgs, "--", p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rcRepoGrepTimeoutVar)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	out, _ := io.ReadAll(io.LimitReader(stdout, rcRepoGrepReadLimit))
	full := len(out) == rcRepoGrepReadLimit
	if full {
		cancel() // enough read; stop git
	}
	waitErr := cmd.Wait()
	// Exit 1 is "no matches". Anything else with no output — a bad regex, an
	// unknown path — is an error, not an empty result: absence of matches
	// must not be reported when the search never ran.
	var exit *exec.ExitError
	if len(out) == 0 && errors.As(waitErr, &exit) && exit.ExitCode() != 1 {
		return "", fmt.Errorf("search failed: %s", strings.TrimSpace(stderr.String()))
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if len(out) == 0 && timedOut {
		return "", fmt.Errorf("search timed out after %s", rcRepoGrepTimeoutVar)
	}
	// A search cut short by the clock or the read cap returns what it found,
	// marked as partial: the gate must not read a missing match as absent.
	partial := ""
	if timedOut || full {
		partial = "(search stopped early — these matches are INCOMPLETE; absence of a match here proves nothing)\n"
		if full {
			if i := strings.LastIndexByte(string(out), '\n'); i > 0 {
				out = out[:i]
			}
		}
	}
	prefix := r.hash + ":"
	var b strings.Builder
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if n == rcRepoGrepMaxLines || b.Len()+len(line) > rcRepoGrepMaxBytes {
			fmt.Fprintf(&b, "(more matches omitted; narrow the query or the path)\n")
			break
		}
		b.WriteString(strings.TrimPrefix(line, prefix))
		b.WriteByte('\n')
		n++
	}
	if n == 0 && partial == "" {
		return fmt.Sprintf("no matches for %q at %s", args.Query, rcShort(r.hash)), nil
	}
	return b.String() + partial, nil
}

// run executes one of the gate's repository tools.
func (r *rcRepo) run(name, input string) (string, error) {
	switch name {
	case "kai_view":
		return r.view(input)
	case "kai_grep":
		return r.grep(input)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// rcReasoningEffort is the thinking depth the grounded review asks for, from
// KAI_REVIEW_REASONING_EFFORT: "minimal", "low", "medium" or "high". Unset (or
// anything else), requests carry no effort and the provider keeps GLM-5.2's
// reasoning switched off, which is how every review ran before.
//
// Run 5 (2026-09-28) traced 19 of 45 findable misses to the reviewer opening
// the right code and not raising the defect — the failure reasoning targets —
// on a model whose reasoning the proxy had turned off for latency. GLM-5.3,
// whose reasoning cannot be disabled, showed the cost of thinking at its
// default depth under the old budgets (46 of 50 reviews ran out of time);
// an explicit effort, with the larger turn and time budgets, is the knob
// between the two.
func rcReasoningEffort() string {
	switch e := strings.ToLower(strings.TrimSpace(os.Getenv("KAI_REVIEW_REASONING_EFFORT"))); e {
	case "minimal", "low", "medium", "high":
		return e
	}
	return ""
}

// rcGateEffort is the effort for a publication-gate call: the review's, on the
// grounded path only. The fast pass shares a 100-second budget with its draft
// and never carries the repository (rcWithRepo), so it keeps no effort.
func rcGateEffort(ctx context.Context) string {
	if rcRepoFrom(ctx) == nil {
		return ""
	}
	return rcReasoningEffort()
}
