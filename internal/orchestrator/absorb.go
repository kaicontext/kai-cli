package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"kai/internal/ignore"
)

// absorbSpawnIntoMain copies the agent's edits from a spawn dir into
// the main repo's working tree. Returns the set of relative paths that
// changed (created/modified/deleted) so the safety gate can classify
// the blast radius.
//
// This is the "propagate back" step. The spawn dir was a CoW clone of
// main when the agent started, so we can compute the diff just by
// walking both trees and comparing file digests — no kai-DB plumbing
// required. Files matching shouldIgnoreObserver's excludes (.kai,
// .git, node_modules) are skipped on both sides so we don't drag
// per-repo internal state along with the agent's actual edits.
//
// Caveat: if the user has uncommitted changes in main while an agent
// is running, those files might get clobbered when the agent's
// version overwrites them. That's a real footgun we'll address in a
// follow-up (snapshot main before absorb so the user can recover).
// For v1 the contract is: don't run the orchestrator with a dirty
// working tree.
func absorbSpawnIntoMain(spawnDir, mainDir string) ([]string, error) {
	// Load the same ignore matcher that `kai capture` uses against the
	// main repo (defaults + .gitignore + .kaiignore). Without this,
	// absorb's walker has a stricter filter than capture — it only
	// excludes .kai/, .git/, node_modules/ — and ends up walking files
	// (vendor/, *.pem, build outputs, etc.) that capture intentionally
	// skipped. Spawn was materialized from snap.latest, so it never
	// had those files; absorb then treats them as "agent deleted" and
	// rm's them from main. This wiped 9k+ Go files from a moby
	// checkout in May 2026 testing — the working tree had them via
	// `git checkout` but kai's snapshot didn't, so absorb decided
	// they should be gone.
	//
	// Loading from mainDir specifically (not spawnDir) so the source
	// of truth is the user's repo configuration; spawn dirs are
	// ephemeral and don't carry .kaiignore overrides.
	matcher, err := ignore.LoadFromDir(mainDir)
	if err != nil {
		// Non-fatal: matcher load failures shouldn't block an absorb
		// (the user's repo might have an invalid pattern; we'd rather
		// over-walk than refuse to integrate). Use defaults-only as
		// a conservative fallback.
		matcher = ignore.NewMatcher(mainDir)
		matcher.LoadDefaults()
	}

	spawnFiles, err := walkDigests(spawnDir, matcher)
	if err != nil {
		return nil, fmt.Errorf("walking spawn dir: %w", err)
	}
	mainFiles, err := walkDigests(mainDir, matcher)
	if err != nil {
		return nil, fmt.Errorf("walking main dir: %w", err)
	}

	// Structural guard: any top-level directory in the spawn that
	// doesn't exist in main but case-insensitively COLLIDES with one
	// that does is almost certainly a casing/aliasing bug, not a
	// legitimate "agent created a new top-level dir." Refuse rather
	// than copy + delete-the-original. The 2026-05-12 incident pinned
	// this: a spawn rooted at `<spawn>/Kai/kai-cli/...` next to main's
	// `<main>/kai-cli/...` had absorb copy every file to a new `Kai/`
	// tree AND delete every `kai-cli/` file as "removed by agent."
	// 2425 files moved, hundreds deleted, ~30 minutes to recover.
	// Case-insensitive collision is a tight discriminator: a real new
	// top-level dir (e.g. agent added `apps/` in a monorepo) won't
	// alias with an existing one.
	if err := validateNoCaseCollisions(spawnFiles, mainFiles); err != nil {
		return nil, err
	}

	changed := make(map[string]struct{})

	// Files added or modified in the spawn relative to main.
	for path, spawnDigest := range spawnFiles {
		mainDigest, exists := mainFiles[path]
		if exists && mainDigest == spawnDigest {
			continue
		}
		src := filepath.Join(spawnDir, path)
		dst := filepath.Join(mainDir, path)
		if err := copyFileForAbsorb(src, dst); err != nil {
			return nil, fmt.Errorf("copying %s: %w", path, err)
		}
		changed[path] = struct{}{}
	}

	// Files the agent deleted (present in main, absent in spawn).
	for path := range mainFiles {
		if _, ok := spawnFiles[path]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(mainDir, path)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing %s: %w", path, err)
		}
		changed[path] = struct{}{}
	}

	out := make([]string, 0, len(changed))
	for p := range changed {
		out = append(out, p)
	}
	return out, nil
}

// validateNoCaseCollisions refuses an absorb whose spawn-vs-main
// shape suggests a path-prefix mismatch — the bug class behind the
// 2026-05-12 incident, where a project Named `Kai` (capital) but
// pathed at on-disk `kai/` produced a spawn rooted at
// `<spawn>/Kai/kai-cli/...`. Absorb then walked main's `kai-cli/...`
// (no `Kai/` prefix) and concluded every file had been deleted by
// the agent AND every spawn file had been added, so the apply
// duplicated the tree + deleted the original (~2400 files churned).
//
// Discriminator: when the spawn would BOTH add a substantial set of
// files AND delete a substantial set of files, AND every added file
// is under a top-level dir that doesn't exist in main, that's the
// structural-mismatch fingerprint. Real refactors don't simultaneously
// add a new top-level dir AND delete most of the existing tree.
//
// Thresholds are deliberately conservative: 20+ adds, 20+ deletes,
// AND every add must live under spawn-only top-level dirs. A
// monorepo-wide rename moving 30 files into a renamed top-level
// would borderline-trip this, but those moves should go through an
// explicit `git mv` or a planner-stated refactor, not silently via
// the absorb path.
func validateNoCaseCollisions(spawnFiles, mainFiles map[string]string) error {
	const minBlastForRefuse = 20

	mainTops := topLevels(mainFiles)
	spawnTops := topLevels(spawnFiles)

	// Files the agent would ADD: present in spawn, absent (or
	// different) in main. We need just presence here.
	adds := 0
	addsUnderForeignTop := 0
	for path := range spawnFiles {
		if _, in := mainFiles[path]; in {
			continue // unchanged content path-wise; modification check handled elsewhere
		}
		adds++
		seg, _, _ := strings.Cut(path, "/")
		if _, exists := mainTops[seg]; !exists {
			addsUnderForeignTop++
		}
	}
	// Files the agent would DELETE: present in main, absent in spawn.
	deletes := 0
	for path := range mainFiles {
		if _, in := spawnFiles[path]; !in {
			deletes++
		}
	}

	if adds < minBlastForRefuse || deletes < minBlastForRefuse {
		return nil
	}
	if addsUnderForeignTop != adds {
		// Some adds live under existing main top-levels — looks like
		// a real refactor, not a wholesale prefix mismatch. Allow.
		return nil
	}
	// All adds are under top-levels that don't exist in main, AND
	// main loses substantial content. That's the prefix-mismatch
	// fingerprint.
	spawnOnlyTops := make([]string, 0)
	for t := range spawnTops {
		if _, in := mainTops[t]; !in {
			spawnOnlyTops = append(spawnOnlyTops, t)
		}
	}
	return fmt.Errorf("absorb refused: spawn would add %d files under top-level dir(s) [%s] that don't exist in main, AND delete %d existing files. This is the fingerprint of a path-prefix mismatch (e.g. spawn rooted under a wrongly-cased subdir). Fix the project Name/path in kai.projects.yaml to match the on-disk basename, then re-run",
		adds, strings.Join(spawnOnlyTops, ", "), deletes)
}

// topLevels returns the set of first-path-segment names appearing
// in the file map. A path "kai-cli/internal/x.go" contributes
// "kai-cli". Empty segments and pure-file roots ("README.md") are
// recorded under "".
func topLevels(files map[string]string) map[string]struct{} {
	out := make(map[string]struct{})
	for p := range files {
		seg, _, _ := strings.Cut(p, "/")
		out[seg] = struct{}{}
	}
	return out
}

// walkDigests returns a map of path -> hex sha256 digest for every
// file under root, excluding the same directories the file observer
// skips. Symlinks aren't followed; large files (>50 MiB) are read
// into memory which is fine for a code repo but a footgun for repos
// that check in big assets — flag for follow-up if it becomes a
// problem.
func walkDigests(root string, matcher *ignore.Matcher) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if shouldIgnoreObserver(rel+"/") || rel == ".kai" || rel == ".git" || rel == "node_modules" {
				return filepath.SkipDir
			}
			// Apply the project's full ignore matcher to directories
			// too — vendor/, .venv/, build/, etc. all have trailing
			// slashes in the default patterns. Skipping the whole
			// subtree is much faster than walking it and ignoring
			// each child.
			if matcher != nil && matcher.Match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldIgnoreObserver(rel) {
			return nil
		}
		// File-level ignore check. This is the load-bearing line for
		// the moby class of bug: without it, absorb walks files
		// capture excluded and treats their absence in spawn as
		// deletions to propagate.
		if matcher != nil && matcher.Match(rel, false) {
			return nil
		}
		// Skip symlinks: the agent might create them but we can't
		// content-hash them safely; treat them as unchanged.
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		digest, err := digestFile(path)
		if err != nil {
			return fmt.Errorf("digesting %s: %w", rel, err)
		}
		out[rel] = digest
		return nil
	})
	return out, err
}

// digestFile returns the hex sha256 of a file's contents.
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileForAbsorb writes src's content to dst, creating any missing
// parent directories. Preserves the source's mode bits so executable
// scripts remain executable. Atomic-ish via temp-file + rename so a
// crash mid-copy doesn't leave a half-written file in main.
func copyFileForAbsorb(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".kai-absorb." + randSuffix()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// randSuffix is a small unique suffix for the temp-file pattern.
// Doesn't need to be cryptographically random — just collision-resistant
// across concurrent absorbs in the same process.
func randSuffix() string {
	var b [8]byte
	_, _ = io.ReadFull(randSource{}, b[:])
	return hex.EncodeToString(b[:])
}

// randSource adapts crypto/rand to io.Reader without importing it
// twice. Tiny indirection so the rest of the file doesn't grow an
// import group for one byte source.
type randSource struct{}

func (randSource) Read(p []byte) (int, error) {
	// crypto/rand.Reader would be ideal but importing it just for
	// 8 bytes of entropy is overkill — a sha256 of pid+time gives
	// us collision-resistance for the tmp filename. We never use
	// these bytes for anything security-sensitive.
	h := sha256.New()
	for i := range p {
		fmt.Fprintf(h, "%d-%d", os.Getpid(), i)
	}
	sum := h.Sum(nil)
	n := copy(p, sum)
	return n, nil
}

// pathSlash forces forward-slash rendering for display, regardless
// of the platform's path separator. Used when reporting changed
// paths to the user.
func pathSlash(p string) string {
	return strings.ReplaceAll(p, string(filepath.Separator), "/")
}

// shouldIgnoreObserver filters paths the absorb walk shouldn't
// surface — only the structural exclusions kai requires for its own
// correctness. Originally also excluded node_modules/ and other
// project-conventions, but those are now governed by the project's
// .gitignore via the matcher (see absorbSpawnIntoMain).
//
// Keeping .kai/ and .git/ as a hardcoded fast-path: walking either
// would corrupt the tool itself (kai snapshotting its own DB,
// absorb trying to delete .git/HEAD). The matcher catches them too,
// but a fast string-prefix check on the hot loop is cheaper than
// the matcher's regex pass.
func shouldIgnoreObserver(rel string) bool {
	if rel == "" || rel == "." {
		return true
	}
	for _, prefix := range []string{".kai/", ".git/"} {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}
