// Package filesource provides abstractions for reading source files from different sources.
package filesource

// FileInfo contains information about a source file.
type FileInfo struct {
	Path         string
	Content      []byte // nil when CachedDigest is set (file unchanged, skip read)
	Lang         string // "ts", "js", or empty
	CachedDigest string // BLAKE3 hex digest from stat cache — set when file is unchanged

	// AbsPath is the absolute filesystem path to this file, set
	// by sources that have a concrete on-disk location
	// (DirectorySource). Used for blob recovery: when the
	// snapshot creator detects a missing blob in the object
	// store, it can re-read AbsPath, verify the hash, and
	// re-write the blob from the working tree. Empty for
	// sources where there's no working-tree file (gitio
	// imports historical commits from the git object store).
	AbsPath string
}

// FileSource abstracts the source of files (Git, filesystem, etc.).
type FileSource interface {
	// GetFiles returns all supported source files.
	GetFiles() ([]*FileInfo, error)

	// GetFile returns a specific file by path.
	GetFile(path string) (*FileInfo, error)

	// Identifier returns a unique identifier for this source state.
	// For Git: commit hash. For directories: content hash.
	Identifier() string

	// SourceType returns the type of source ("git" or "directory").
	SourceType() string
}
