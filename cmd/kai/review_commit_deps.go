package main

// review_commit_deps.go — naming what the reviewer cannot read, before it
// guesses.
//
// review_commit_lookups.go answers the questions the reviewer would otherwise
// spend a turn on. This file does the opposite job for the questions it can
// never answer at all, because the answer is not in this checkout.
//
// CLI#95 and TUI#92 (2026-09-08) both moved kai-engine forward and switched
// four call sites onto kaipath.UserPath. Neither review could read that
// function — 17 lines, in another module — and both spent most of their output
// on it anyway: whether it is variadic, whether zero trailing components are
// allowed, whether an empty override preserves the default, whether the
// one-argument call is valid. TUI#92 escalated the arity question to "That's a
// real defect, not a...". Every one of those was speculation about a file
// nobody could open, presented as review.
//
// The reviewer was not being careless. It was told to ground every claim, and
// it was handed a diff whose correctness genuinely rests on a contract it had
// no way to see. What it lacked was permission to say so once and move on.
//
// NOTHING HERE FETCHES. The review pod is debian:bookworm-slim carrying two
// binaries — no Go toolchain, no module cache — and the workflow never runs
// `go mod download` (which for a private module would need GOPRIVATE and auth
// besides). So sibling module source is not merely unfetched, it is absent,
// and pretending otherwise would produce a second kind of wrong answer. What
// this does instead is tell the reviewer exactly which contracts are out of
// reach and that ONE scoped sentence is the whole correct response.
//
// The parsing is the same parsing a real fetch would need — module path,
// version, and the commit inside a pseudo-version — so this is the floor under
// that work, not a detour around it.

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	// rcMaxDepsShown bounds the block. A diff that moves thirty modules is a
	// dependency bump, and listing all of them would bury the one or two the
	// code actually reaches.
	rcMaxDepsShown = 6
	// rcMaxPkgsPerDep bounds the packages named per module, same reasoning.
	rcMaxPkgsPerDep = 5
)

// rcDepChange is one module whose version this diff moves.
type rcDepChange struct {
	Module string // github.com/kaicontext/kai-engine
	From   string // "" when the diff ADDS the dependency
	To     string
	Commit string   // the 12-hex commit inside a pseudo-version, "" otherwise
	Pkgs   []string // packages under this module that the diff imports
}

// rcPseudoCommit pulls the commit out of a Go pseudo-version.
//
// v0.6.59-0.20260908192613-5ab1b102fc2f names commit 5ab1b102fc2f. That is the
// exact source the reviewer would need, and it is sitting in the diff it was
// handed — which is what makes the speculation on TUI#92 so costly. It is
// reported so the limitation can name a commit rather than a version range,
// and so a later fetch has the identifier already parsed.
func rcPseudoCommit(version string) string {
	parts := strings.Split(version, "-")
	if len(parts) < 3 {
		return ""
	}
	last := parts[len(parts)-1]
	if len(last) != 12 {
		return ""
	}
	for _, r := range last {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return last
}

// rcGoModRequire matches a require line in a go.mod diff hunk: optional
// "require ", a module path, a version, and whatever trailing comment.
var rcGoModRequire = regexp.MustCompile(`^[+-]\s*(?:require\s+)?([a-zA-Z0-9._~-]+\.[a-zA-Z0-9._~/-]+)\s+(v[0-9][^\s]*)`)

// rcChangedDeps reads the diff's go.mod hunks and reports which modules move.
//
// Deliberately schema-loose, like rcChangedSymbols: it reads require lines and
// ignores everything else a go.mod can hold. A missed module only shortens a
// list, and a wrong one would only over-declare a limitation the reviewer
// already has.
func rcChangedDeps(diff string) []rcDepChange {
	type versions struct{ from, to string }
	moved := map[string]*versions{}

	inGoMod := false
	for _, line := range strings.Split(diff, "\n") {
		// Track which file the hunk belongs to. Only go.mod carries module
		// versions; go.sum restates them twice each and would double every
		// entry with nothing added.
		if strings.HasPrefix(line, "+++ b/") || strings.HasPrefix(line, "--- a/") {
			inGoMod = path.Base(strings.TrimSpace(line[6:])) == "go.mod"
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			inGoMod = strings.HasSuffix(strings.TrimSpace(line), "go.mod")
			continue
		}
		if !inGoMod || len(line) == 0 {
			continue
		}
		if line[0] != '+' && line[0] != '-' {
			continue
		}
		m := rcGoModRequire.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		mod, ver := m[1], m[2]
		if moved[mod] == nil {
			moved[mod] = &versions{}
		}
		if line[0] == '+' {
			moved[mod].to = ver
		} else {
			moved[mod].from = ver
		}
	}

	var out []rcDepChange
	for mod, v := range moved {
		// A module with no "+" line was REMOVED, and a removed dependency is
		// not a contract this diff rests on.
		if v.to == "" || v.to == v.from {
			continue
		}
		out = append(out, rcDepChange{
			Module: mod,
			From:   v.from,
			To:     v.to,
			Commit: rcPseudoCommit(v.to),
			Pkgs:   rcImportedPkgs(diff, mod),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Modules the diff actually imports first: those are the ones whose
		// contract the change depends on, as opposed to a transitive bump.
		if (len(out[i].Pkgs) > 0) != (len(out[j].Pkgs) > 0) {
			return len(out[i].Pkgs) > 0
		}
		return out[i].Module < out[j].Module
	})
	if len(out) > rcMaxDepsShown {
		out = out[:rcMaxDepsShown]
	}
	return out
}

// rcImportedPkgs finds packages under mod that appear as import paths anywhere
// in the diff — added, removed or context lines alike, because an import the
// diff does not touch still names a contract the changed code below it uses.
func rcImportedPkgs(diff, mod string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		i := strings.Index(line, `"`+mod)
		if i < 0 {
			continue
		}
		rest := line[i+1+len(mod):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		pkg := strings.Trim(rest[:j], "/")
		if pkg == "" {
			pkg = path.Base(mod) // the module root is itself a package
		}
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
		if len(out) >= rcMaxPkgsPerDep {
			break
		}
	}
	sort.Strings(out)
	return out
}

// rcDepLimitsBlock is the prompt section: what cannot be read, and what the
// single correct response to that is.
//
// The instruction is as important as the list. Left to itself the reviewer
// produces a concern per call site, because each one genuinely is unverified —
// and a reader cannot tell that speculation from the findings around it. One
// scoped sentence carries the same information and costs the reader nothing.
func rcDepLimitsBlock(deps []rcDepChange) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("DEPENDENCIES THIS DIFF MOVES (resolved from the diff before this review started).\n")
	b.WriteString("You cannot read these. They are other modules, this checkout holds none of their\n")
	b.WriteString("source, and no tool you have will open them — do not spend a turn trying.\n")
	for _, d := range deps {
		b.WriteString("  ")
		b.WriteString(d.Module)
		if d.From != "" {
			fmt.Fprintf(&b, "  %s -> %s", d.From, d.To)
		} else {
			fmt.Fprintf(&b, "  added at %s", d.To)
		}
		if d.Commit != "" {
			fmt.Fprintf(&b, " (commit %s)", d.Commit)
		}
		b.WriteString("\n")
		if len(d.Pkgs) > 0 {
			fmt.Fprintf(&b, "    imported here: %s\n", strings.Join(d.Pkgs, ", "))
		}
	}
	b.WriteString("\nAn unread contract is a LIMITATION, not a defect. Say it ONCE — name the module and\n")
	b.WriteString("what rests on it — then review what you can actually see. Do not open a concern per\n")
	b.WriteString("call site, do not ask the author to confirm a signature you could not read, and do\n")
	b.WriteString("not call it a defect: you have no evidence either way, and a reader cannot tell that\n")
	b.WriteString("speculation from the findings around it.\n\n")
	return b.String()
}
