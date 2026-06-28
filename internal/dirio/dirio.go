// Package dirio provides directory-based file source operations.
package dirio

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"lukechampine.com/blake3"

	"kai/internal/filesource"
	"github.com/kaicontext/kai-engine/ignore"
)

// DirectorySource reads files from a filesystem directory.
type DirectorySource struct {
	rootPath   string
	files      []*filesource.FileInfo
	identifier string
	ignore     *ignore.Matcher
	statCache  *StatCache
}

// Option configures a DirectorySource.
type Option func(*DirectorySource)

// WithIgnore sets a custom ignore matcher.
func WithIgnore(m *ignore.Matcher) Option {
	return func(ds *DirectorySource) {
		ds.ignore = m
	}
}

// WithStatCache provides a stat cache to skip reading unchanged files.
func WithStatCache(sc *StatCache) Option {
	return func(ds *DirectorySource) {
		ds.statCache = sc
	}
}

// OpenDirectory opens a directory as a file source.
// Options can be passed to configure behavior (e.g., WithIgnore, WithStatCache).
func OpenDirectory(dirPath string, opts ...Option) (*DirectorySource, error) {
	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		return nil, fmt.Errorf("getting absolute path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absPath)
	}

	ds := &DirectorySource{rootPath: absPath}

	// Apply options
	for _, opt := range opts {
		opt(ds)
	}

	// If no ignore matcher provided, load from directory
	if ds.ignore == nil {
		ds.ignore, err = ignore.LoadFromDir(absPath)
		if err != nil {
			return nil, fmt.Errorf("loading ignore patterns: %w", err)
		}
	}

	// Walk directory and collect files
	if err := ds.collectFiles(); err != nil {
		return nil, err
	}

	// Compute content hash identifier
	ds.computeIdentifier()

	// If using stat cache, prune entries for deleted files and save
	if ds.statCache != nil {
		currentPaths := make(map[string]bool, len(ds.files))
		for _, f := range ds.files {
			currentPaths[f.Path] = true
		}
		ds.statCache.Prune(currentPaths)
	}

	return ds, nil
}

// GetFiles returns all supported source files.
func (ds *DirectorySource) GetFiles() ([]*filesource.FileInfo, error) {
	return ds.files, nil
}

// GetFile returns a specific file by path.
func (ds *DirectorySource) GetFile(path string) (*filesource.FileInfo, error) {
	for _, f := range ds.files {
		if f.Path == path {
			return f, nil
		}
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

// Identifier returns the content hash of all files.
func (ds *DirectorySource) Identifier() string {
	return ds.identifier
}

// SourceType returns "directory".
func (ds *DirectorySource) SourceType() string {
	return "directory"
}

// fileEntry holds walk results before parallel reading.
type fileEntry struct {
	absPath string
	relPath string
	lang    string
	info    fs.FileInfo
	cached  string // pre-resolved digest from dir cache skip
}

// collectFiles uses a custom recursive walk that can skip entire unchanged
// directory subtrees. When a directory's mtime matches the stat cache,
// we replay all cached entries from that subtree without any readdir/stat
// syscalls — the biggest performance win for large repos.
func (ds *DirectorySource) collectFiles() error {
	// Phase 1: Recursive walk with subtree skipping.
	var entries []fileEntry

	err := ds.walkDir(ds.rootPath, ".", &entries)
	if err != nil {
		return fmt.Errorf("walking directory: %w", err)
	}

	// Phase 2: Read file contents in parallel.
	files := make([]*filesource.FileInfo, len(entries))
	errs := make([]error, len(entries))

	workers := runtime.NumCPU()
	if workers > 16 {
		workers = 16
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	work := make(chan int, len(entries))

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				e := entries[i]

				// Replayed from unchanged dir — digest already resolved.
				if e.cached != "" {
					files[i] = &filesource.FileInfo{
						Path:         e.relPath,
						Content:      nil,
						Lang:         e.lang,
						CachedDigest: e.cached,
						AbsPath:      e.absPath,
					}
					continue
				}

				// Check stat cache — if mtime+size match, skip the file read entirely.
				if ds.statCache != nil {
					if cachedDigest, cachedLang, ok := ds.statCache.Lookup(e.relPath, e.info); ok {
						lang := e.lang
						if cachedLang != "" {
							lang = cachedLang
						}
						// Don't read the file — pass the cached digest so downstream
						// can reuse existing nodes without re-reading or re-hashing.
						files[i] = &filesource.FileInfo{
							Path:         e.relPath,
							Content:      nil, // skip read
							Lang:         lang,
							CachedDigest: cachedDigest,
							AbsPath:      e.absPath,
						}
						continue
					}
				}

				content, err := os.ReadFile(e.absPath)
				if err != nil {
					errs[i] = fmt.Errorf("reading file %s: %w", e.absPath, err)
					continue
				}

				files[i] = &filesource.FileInfo{
					Path:    e.relPath,
					Content: content,
					Lang:    e.lang,
					AbsPath: e.absPath,
				}

				// Update stat cache with the new file's info.
				if ds.statCache != nil {
					digest := fmt.Sprintf("%x", blake3.Sum256(content))
					ds.statCache.Update(e.relPath, e.info, digest, e.lang)
				}
			}
		}()
	}

	for i := range entries {
		work <- i
	}
	close(work)
	wg.Wait()

	// Check for errors.
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	// Filter out any nil entries (shouldn't happen, but be safe).
	result := make([]*filesource.FileInfo, 0, len(files))
	for _, f := range files {
		if f != nil {
			result = append(result, f)
		}
	}

	ds.files = result
	return nil
}

// computeIdentifier computes a BLAKE3 hash of all file paths and contents.
func (ds *DirectorySource) computeIdentifier() {
	// Sort files by path for deterministic ordering
	sortedFiles := make([]*filesource.FileInfo, len(ds.files))
	copy(sortedFiles, ds.files)
	sort.Slice(sortedFiles, func(i, j int) bool {
		return sortedFiles[i].Path < sortedFiles[j].Path
	})

	hasher := blake3.New(32, nil)

	for _, f := range sortedFiles {
		hasher.Write([]byte(f.Path))
		hasher.Write([]byte{0})
		if f.CachedDigest != "" {
			hasher.Write([]byte(f.CachedDigest))
		} else {
			d := fmt.Sprintf("%x", blake3.Sum256(f.Content))
			hasher.Write([]byte(d))
		}
		hasher.Write([]byte{0})
	}

	ds.identifier = fmt.Sprintf("%x", hasher.Sum(nil))
}

// walkDir recursively walks a directory, skipping readdir + file processing
// for directories whose mtime is unchanged. Still recurses into subdirs
// so each level is checked independently.
func (ds *DirectorySource) walkDir(absDir, relDir string, entries *[]fileEntry) error {
	// Check if this directory is ignored
	if relDir != "." && ds.ignore != nil && ds.ignore.Match(relDir, true) {
		return nil
	}

	// Read directory entries — we always need this to find subdirs
	dirEntries, err := os.ReadDir(absDir)
	if err != nil {
		return nil // skip unreadable dirs
	}

	// Update directory mtime in stat cache (used for readdir optimization only,
	// NOT for skipping individual file stat checks — see comment below).
	if ds.statCache != nil && relDir != "." {
		dirInfo, err := os.Stat(absDir)
		if err == nil {
			ds.statCache.UpdateDir(relDir, dirInfo)
		}
	}

	for _, d := range dirEntries {
		name := d.Name()
		absPath := filepath.Join(absDir, name)
		var relPath string
		if relDir == "." {
			relPath = name
		} else {
			relPath = relDir + "/" + name
		}

		if d.IsDir() {
			if err := ds.walkDir(absPath, relPath, entries); err != nil {
				return err
			}
			continue
		}

		// Early extension check — cheapest filter
		lang := detectLang(name)
		if lang == "" {
			continue
		}

		// Note: we do NOT skip individual file stat checks based on directory mtime.
		// Modifying a file's content changes the file's mtime but NOT the parent
		// directory's mtime (directory mtime only changes on create/delete).
		// Always stat individual files and let the stat cache (Phase 2) compare
		// mtime+size to decide whether to re-read.

		// Check ignore patterns (expensive — skip for unchanged dirs)
		if ds.ignore != nil && ds.ignore.Match(relPath, false) {
			continue
		}

		// Skip symlinks entirely. The previous behavior (resolving
		// the target via os.Stat and capturing as if it were a
		// regular file) didn't round-trip cleanly: kai capture
		// snapshotted the target's CONTENT, then kai checkout
		// materialized that content as a regular file in a fresh
		// workspace — silently converting the symlink to a
		// duplicate of its target. Then absorb's filesystem diff
		// would copy the duplicate back over the original symlink,
		// permanently losing the link.
		//
		// Verified May 2026 against moby's integration-cli/fixtures/
		// https/ tree: 5 symlinks (ca.pem, server-cert.pem, etc.)
		// pointed to other files in the same dir; kai's round-trip
		// turned them all into independent file copies. Skipping
		// symlinks during capture leaves them entirely outside
		// kai's purview — the user's git already tracks the
		// symlink-ness, and absorb's walker also skips symlinks
		// (orchestrator/absorb.go:146), so neither side touches
		// them.
		if d.Type()&fs.ModeSymlink != 0 {
			continue
		}

		info, err := d.Info()
		if err != nil {
			continue
		}

		*entries = append(*entries, fileEntry{
			absPath: absPath,
			relPath: relPath,
			lang:    lang,
			info:    info,
		})
	}

	return nil
}

// detectLang detects the language based on file extension.
// DetectLang detects the language based on file extension.
func DetectLang(path string) string {
	return detectLang(path)
}

func detectLang(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	// JavaScript/TypeScript
	case ".ts", ".tsx":
		return "ts"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "js"
	// Structured data
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	// Documentation
	case ".md", ".markdown":
		return "markdown"
	case ".txt", ".text":
		return "text"
	// Config
	case ".ini", ".cfg", ".conf":
		return "ini"
	case ".env":
		return "env"
	// Other code (tracked but no semantic analysis yet)
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc", ".cxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".sql":
		return "sql"
	case ".html", ".htm":
		return "html"
	case ".css", ".scss", ".sass", ".less":
		return "css"
	default:
		return "blob"
	}
}
