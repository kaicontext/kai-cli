// Package dirio provides directory-based file source operations.
package dirio

import (
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatCache stores file stat metadata to skip re-reading unchanged files.
// Analogous to git's .git/index — if mtime+size match, the file hasn't changed.
type StatCache struct {
	Entries  map[string]*StatEntry `json:"entries"`
	DirTimes map[string]int64     `json:"dirTimes,omitempty"` // dir path -> mtime (UnixNano)
	mu       sync.RWMutex
	bin      *BinCache // mmap'd binary cache for fast reads
}

// StatEntry holds cached stat info and content digest for one file.
type StatEntry struct {
	ModTime int64  `json:"mtime"`  // UnixNano
	Size    int64  `json:"size"`
	Digest  string `json:"digest"` // BLAKE3 hex digest of content
	Lang    string `json:"lang"`
}

// LoadStatCache loads the stat cache from disk, or returns an empty cache.
// Tries binary mmap first (fastest), then gob, then JSON (legacy).
func LoadStatCache(kaiDir string) *StatCache {
	sc := &StatCache{Entries: make(map[string]*StatEntry), DirTimes: make(map[string]int64)}

	// Try binary mmap first — zero-allocation reads
	if bc := LoadBinCache(kaiDir); bc != nil {
		sc.bin = bc
		// Don't load entries into maps — the binary cache handles reads.
		// Maps will be populated lazily as entries are updated.
		return sc
	}

	// Try gob format
	gobPath := filepath.Join(kaiDir, "statcache.gob")
	if f, err := os.Open(gobPath); err == nil {
		defer f.Close()
		if gob.NewDecoder(f).Decode(sc) == nil {
			if sc.Entries == nil {
				sc.Entries = make(map[string]*StatEntry)
			}
			if sc.DirTimes == nil {
				sc.DirTimes = make(map[string]int64)
			}
			return sc
		}
	}

	// Fall back to JSON (legacy migration)
	jsonPath := filepath.Join(kaiDir, "statcache.json")
	if data, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(data, sc)
	}
	if sc.Entries == nil {
		sc.Entries = make(map[string]*StatEntry)
	}
	if sc.DirTimes == nil {
		sc.DirTimes = make(map[string]int64)
	}
	return sc
}

// Save writes the stat cache in binary format.
// Merges any entries from the mmap'd binary cache into the maps first,
// then writes the combined result. Removes legacy files.
func (sc *StatCache) Save(kaiDir string) error {
	// If we loaded from binary cache, merge its entries into maps
	// so the write includes everything (existing + new/updated).
	if sc.bin != nil {
		for i := 0; i < sc.bin.fileCount; i++ {
			path := sc.bin.filePath(i)
			// Only add if not already overridden in the map
			sc.mu.RLock()
			_, exists := sc.Entries[path]
			sc.mu.RUnlock()
			if !exists {
				off := sc.bin.fileEntryOffset(i)
				mtime := int64(binary.LittleEndian.Uint64(sc.bin.data[off+6 : off+14]))
				size := int64(binary.LittleEndian.Uint64(sc.bin.data[off+14 : off+22]))
				digest := hexEncode(sc.bin.data[off+22 : off+54])
				lang := langStrings[sc.bin.data[off+54]]
				sc.mu.Lock()
				sc.Entries[path] = &StatEntry{ModTime: mtime, Size: size, Digest: digest, Lang: lang}
				sc.mu.Unlock()
			}
		}
		for i := 0; i < sc.bin.dirCount; i++ {
			path := sc.bin.dirPath(i)
			sc.mu.RLock()
			_, exists := sc.DirTimes[path]
			sc.mu.RUnlock()
			if !exists {
				off := sc.bin.dirEntryOffset(i)
				mtime := int64(binary.LittleEndian.Uint64(sc.bin.data[off+6 : off+14]))
				sc.mu.Lock()
				sc.DirTimes[path] = mtime
				sc.mu.Unlock()
			}
		}
		sc.bin.Close()
		sc.bin = nil
	}

	err := WriteBinCache(kaiDir, sc)
	if err != nil {
		return err
	}

	// Clean up legacy files
	os.Remove(filepath.Join(kaiDir, "statcache.json"))
	os.Remove(filepath.Join(kaiDir, "statcache.gob"))
	return nil
}

// Lookup checks if a file's stat matches the cache. Returns the cached digest
// and true if the file is unchanged, or ("", false) if it needs re-reading.
func (sc *StatCache) Lookup(relPath string, info os.FileInfo) (string, string, bool) {
	// Try binary cache first (zero allocation)
	if sc.bin != nil {
		mtime, size, digest, lang, ok := sc.bin.LookupFile(relPath)
		if ok {
			cachedTime := time.Unix(0, mtime).Truncate(time.Microsecond)
			fileTime := info.ModTime().Truncate(time.Microsecond)
			if cachedTime.Equal(fileTime) && size == info.Size() {
				return digest, lang, true
			}
		}
		return "", "", false
	}

	sc.mu.RLock()
	defer sc.mu.RUnlock()
	entry, ok := sc.Entries[relPath]
	if !ok {
		return "", "", false
	}
	cachedTime := time.Unix(0, entry.ModTime).Truncate(time.Microsecond)
	fileTime := info.ModTime().Truncate(time.Microsecond)
	if cachedTime.Equal(fileTime) && entry.Size == info.Size() {
		return entry.Digest, entry.Lang, true
	}
	return "", "", false
}

// LookupByPath checks if a file exists in the cache by path only.
func (sc *StatCache) LookupByPath(relPath string) (string, string, bool) {
	if sc.bin != nil {
		_, _, digest, lang, ok := sc.bin.LookupFile(relPath)
		if ok {
			return digest, lang, true
		}
		// Also check map (for newly added entries this session)
	}

	sc.mu.RLock()
	defer sc.mu.RUnlock()
	entry, ok := sc.Entries[relPath]
	if !ok {
		return "", "", false
	}
	return entry.Digest, entry.Lang, true
}

// Update records a file's stat + digest in the cache.
func (sc *StatCache) Update(relPath string, info os.FileInfo, digest string, lang string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.Entries[relPath] = &StatEntry{
		ModTime: info.ModTime().UnixNano(),
		Size:    info.Size(),
		Digest:  digest,
		Lang:    lang,
	}
}

// Prune removes entries for files that no longer exist in the given set.
func (sc *StatCache) Prune(currentPaths map[string]bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for path := range sc.Entries {
		if !currentPaths[path] {
			delete(sc.Entries, path)
		}
	}
}

// DirUnchanged checks if a directory's mtime matches the cache.
func (sc *StatCache) DirUnchanged(relPath string, info os.FileInfo) bool {
	if sc.bin != nil {
		cached, ok := sc.bin.LookupDir(relPath)
		if ok {
			cachedTime := time.Unix(0, cached).Truncate(time.Microsecond)
			dirTime := info.ModTime().Truncate(time.Microsecond)
			return cachedTime.Equal(dirTime)
		}
		return false
	}

	sc.mu.RLock()
	defer sc.mu.RUnlock()
	cached, ok := sc.DirTimes[relPath]
	if !ok {
		return false
	}
	cachedTime := time.Unix(0, cached).Truncate(time.Microsecond)
	dirTime := info.ModTime().Truncate(time.Microsecond)
	return cachedTime.Equal(dirTime)
}

// UpdateDir records a directory's mtime.
func (sc *StatCache) UpdateDir(relPath string, info os.FileInfo) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.DirTimes[relPath] = info.ModTime().UnixNano()
}


// PruneDirs removes directory entries that no longer exist.
func (sc *StatCache) PruneDirs(currentDirs map[string]bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for path := range sc.DirTimes {
		if !currentDirs[path] {
			delete(sc.DirTimes, path)
		}
	}
}
