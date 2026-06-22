// Package cache provides file digest caching to speed up status checks.
package cache

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"kai/internal/kaipath"
	"kai/internal/util"
)

// FileCache caches file digests keyed by (path, size, mtime).
// This avoids rehashing unchanged files on repeated status checks.
type FileCache struct {
	db      *sql.DB
	baseDir string
}

const schema = `
CREATE TABLE IF NOT EXISTS file_cache (
	path TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	digest TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_path ON file_cache(path);
`

// Open opens or creates a file cache in the given directory.
// The cache database is stored at <kaiDir>/cache/files.db, where kaiDir
// resolves to .git/kai or .kai depending on the project layout.
func Open(baseDir string) (*FileCache, error) {
	cacheDir := filepath.Join(kaipath.Resolve(baseDir), "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(cacheDir, "files.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// Apply schema
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	return &FileCache{db: db, baseDir: baseDir}, nil
}

// Close closes the cache database.
func (c *FileCache) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// GetOrCompute returns the digest for a file, using cached value if valid.
// If the cache entry is stale (size/mtime changed), it recomputes and updates.
func (c *FileCache) GetOrCompute(path string, info os.FileInfo, content []byte) (string, error) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()

	// Try to get cached digest
	var cachedSize, cachedMtime int64
	var cachedDigest string
	err := c.db.QueryRow(
		"SELECT size, mtime, digest FROM file_cache WHERE path = ?",
		path,
	).Scan(&cachedSize, &cachedMtime, &cachedDigest)

	if err == nil && cachedSize == size && cachedMtime == mtime {
		// Cache hit - return cached digest
		return cachedDigest, nil
	}

	// Cache miss or stale - compute digest
	digest := util.Blake3HashHex(content)

	// Update cache
	_, err = c.db.Exec(
		`INSERT OR REPLACE INTO file_cache (path, size, mtime, digest)
		 VALUES (?, ?, ?, ?)`,
		path, size, mtime, digest,
	)
	if err != nil {
		// Non-fatal - log but continue
		// In production you might want to log this
	}

	return digest, nil
}

// GetDigest returns the cached digest for a path if it matches current stat.
// Returns empty string and nil error if not cached or stale.
func (c *FileCache) GetDigest(path string, info os.FileInfo) (string, error) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()

	var cachedSize, cachedMtime int64
	var cachedDigest string
	err := c.db.QueryRow(
		"SELECT size, mtime, digest FROM file_cache WHERE path = ?",
		path,
	).Scan(&cachedSize, &cachedMtime, &cachedDigest)

	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	if cachedSize == size && cachedMtime == mtime {
		return cachedDigest, nil
	}

	return "", nil // Stale
}

// SetDigest stores a digest in the cache.
func (c *FileCache) SetDigest(path string, info os.FileInfo, digest string) error {
	_, err := c.db.Exec(
		`INSERT OR REPLACE INTO file_cache (path, size, mtime, digest)
		 VALUES (?, ?, ?, ?)`,
		path, info.Size(), info.ModTime().UnixNano(), digest,
	)
	return err
}

// Clear removes all entries from the cache.
func (c *FileCache) Clear() error {
	_, err := c.db.Exec("DELETE FROM file_cache")
	return err
}

// Remove removes a single entry from the cache.
func (c *FileCache) Remove(path string) error {
	_, err := c.db.Exec("DELETE FROM file_cache WHERE path = ?", path)
	return err
}

// Stats returns cache statistics.
type Stats struct {
	TotalEntries int64
}

func (c *FileCache) Stats() (*Stats, error) {
	var count int64
	err := c.db.QueryRow("SELECT COUNT(*) FROM file_cache").Scan(&count)
	if err != nil {
		return nil, err
	}
	return &Stats{TotalEntries: count}, nil
}
