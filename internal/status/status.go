// Package status provides working directory comparison against snapshots.
package status

import (
	"fmt"
	"os"
	"sort"

	"kai/internal/cache"
	"kai/internal/dirio"
	"kai/internal/graph"
	"kai/internal/ref"
	"kai/internal/snapshot"
	"kai/internal/util"
)

// Result contains the results of comparing working directory to a baseline.
type Result struct {
	BaselineID  []byte
	BaselineRef string
	Added       []string
	Modified    []string
	Deleted     []string
	// NoBaseline is true when there's no snapshot to compare against.
	// In this case, Added contains all files that would be captured.
	NoBaseline bool
}

// Options configures status computation.
type Options struct {
	Dir         string // Directory to check
	Against     string // Baseline ref/selector (empty = auto-resolve)
	UseCache    bool   // Use file digest cache
	CacheDir    string // Cache directory (default: status dir)
}

// Compute compares the working directory against a baseline snapshot.
func Compute(db *graph.DB, opts Options) (*Result, error) {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.CacheDir == "" {
		opts.CacheDir = opts.Dir
	}

	// Get current directory files first
	currentSource, err := dirio.OpenDirectory(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("opening directory: %w", err)
	}

	currentFiles, err := currentSource.GetFiles()
	if err != nil {
		return nil, fmt.Errorf("getting current files: %w", err)
	}

	// Resolve baseline
	baselineID, baselineRef, err := resolveBaseline(db, opts.Against)
	if err != nil {
		return nil, err
	}

	// No baseline - show all files that would be captured
	if baselineID == nil {
		result := &Result{
			NoBaseline: true,
		}
		for _, f := range currentFiles {
			result.Added = append(result.Added, f.Path)
		}
		sort.Strings(result.Added)
		return result, nil
	}

	result := &Result{
		BaselineID:  baselineID,
		BaselineRef: baselineRef,
	}

	// Open file cache if requested
	var fileCache *cache.FileCache
	if opts.UseCache {
		fileCache, err = cache.Open(opts.CacheDir)
		if err != nil {
			// Non-fatal - continue without cache
			fileCache = nil
		} else {
			defer fileCache.Close()
		}
	}

	// Build map of current files by path -> digest
	currentFileMap := make(map[string]string)
	for _, f := range currentFiles {
		var digest string
		if fileCache != nil {
			// Get file info for cache lookup
			fullPath := f.Path
			if opts.Dir != "." {
				fullPath = opts.Dir + "/" + f.Path
			}
			info, err := os.Stat(fullPath)
			if err == nil {
				digest, _ = fileCache.GetOrCompute(f.Path, info, f.Content)
			}
		}
		if digest == "" {
			digest = util.Blake3HashHex(f.Content)
		}
		currentFileMap[f.Path] = digest
	}

	// Get files from baseline snapshot
	snapshotFiles, err := snapshot.NewCreator(db, nil).GetSnapshotFiles(baselineID)
	if err != nil {
		return nil, fmt.Errorf("getting snapshot files: %w", err)
	}

	snapshotFileMap := make(map[string]string)
	for _, f := range snapshotFiles {
		path, _ := f.Payload["path"].(string)
		digest, _ := f.Payload["digest"].(string)
		snapshotFileMap[path] = digest
	}

	// Compare
	for path, currentDigest := range currentFileMap {
		if snapshotDigest, exists := snapshotFileMap[path]; !exists {
			result.Added = append(result.Added, path)
		} else if currentDigest != snapshotDigest {
			result.Modified = append(result.Modified, path)
		}
	}

	for path := range snapshotFileMap {
		if _, exists := currentFileMap[path]; !exists {
			result.Deleted = append(result.Deleted, path)
		}
	}

	// Sort for deterministic output
	sort.Strings(result.Added)
	sort.Strings(result.Modified)
	sort.Strings(result.Deleted)

	return result, nil
}

// HasChanges returns true if there are any changes.
func (r *Result) HasChanges() bool {
	return len(r.Added) > 0 || len(r.Modified) > 0 || len(r.Deleted) > 0
}

// TotalChanges returns the total number of changed files.
func (r *Result) TotalChanges() int {
	return len(r.Added) + len(r.Modified) + len(r.Deleted)
}

// resolveBaseline determines what baseline to compare against.
// Priority:
// 1. Explicit --against flag
// 2. @snap:last (latest snapshot)
func resolveBaseline(db *graph.DB, against string) ([]byte, string, error) {
	resolver := ref.NewResolver(db)
	kind := ref.KindSnapshot

	if against != "" {
		result, err := resolver.Resolve(against, &kind)
		if err != nil {
			return nil, "", fmt.Errorf("resolving baseline '%s': %w", against, err)
		}
		return result.ID, against, nil
	}

	// Default: latest snapshot (snap.latest)
	result, err := resolver.Resolve("@snap:last", &kind)
	if err != nil {
		// No snapshots yet
		if _, ok := err.(*ref.NotFoundError); ok {
			return nil, "", nil
		}
		return nil, "", err
	}

	return result.ID, "snap.latest", nil
}
