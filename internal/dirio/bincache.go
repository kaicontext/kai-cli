// Package dirio provides directory-based file source operations.
package dirio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// BinCache is a memory-mapped binary stat cache.
// Zero-allocation lookups via binary search on sorted entries.
//
// File format:
//   Header:  magic(4) version(4) fileCount(4) dirCount(4) strTableOff(4) = 20 bytes
//   Files:   [pathOff(4) pathLen(2) mtime(8) size(8) digest(32) lang(1)] × fileCount = 55 bytes each
//   Dirs:    [pathOff(4) pathLen(2) mtime(8)] × dirCount = 14 bytes each
//   Strings: packed path bytes
type BinCache struct {
	data       []byte // mmap'd data
	fileCount  int
	dirCount   int
	strTableOff int
	fd         int
}

const (
	binMagic      = 0x4B414943 // "KAIC"
	binVersion    = 1
	headerSize    = 20
	fileEntrySize = 55 // 4+2+8+8+32+1
	dirEntrySize  = 14 // 4+2+8
)

// Lang byte encoding
var langBytes = map[string]byte{
	"go": 1, "python": 2, "ruby": 3, "rust": 4,
	"js": 5, "ts": 6, "jsx": 7, "tsx": 8,
	"json": 9, "yaml": 10, "toml": 11, "xml": 12,
	"markdown": 13, "text": 14, "ini": 15, "env": 16,
	"java": 17, "c": 18, "cpp": 19, "csharp": 20,
	"php": 21, "swift": 22, "kotlin": 23, "shell": 24,
	"sql": 25, "html": 26, "css": 27, "blob": 28,
	"svelte": 29,
}

var langStrings map[byte]string

func init() {
	langStrings = make(map[byte]string, len(langBytes))
	for k, v := range langBytes {
		langStrings[v] = k
	}
}

// LoadBinCache memory-maps the binary stat cache from disk.
// Returns nil if the file doesn't exist or is invalid.
func LoadBinCache(kaiDir string) *BinCache {
	path := filepath.Join(kaiDir, "statcache.bin")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() < headerSize {
		return nil
	}

	size := int(info.Size())
	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil
	}

	// Validate header
	magic := binary.LittleEndian.Uint32(data[0:4])
	version := binary.LittleEndian.Uint32(data[4:8])
	if magic != binMagic || version != binVersion {
		syscall.Munmap(data)
		return nil
	}

	bc := &BinCache{
		data:        data,
		fileCount:   int(binary.LittleEndian.Uint32(data[8:12])),
		dirCount:    int(binary.LittleEndian.Uint32(data[12:16])),
		strTableOff: int(binary.LittleEndian.Uint32(data[16:20])),
	}

	return bc
}

// Close unmaps the memory.
func (bc *BinCache) Close() {
	if bc != nil && bc.data != nil {
		syscall.Munmap(bc.data)
		bc.data = nil
	}
}

// fileEntryOffset returns the byte offset of file entry i.
func (bc *BinCache) fileEntryOffset(i int) int {
	return headerSize + i*fileEntrySize
}

// dirEntryOffset returns the byte offset of dir entry i.
func (bc *BinCache) dirEntryOffset(i int) int {
	return headerSize + bc.fileCount*fileEntrySize + i*dirEntrySize
}

// filePath returns the path string for file entry i.
// Returns a proper Go string (copied from mmap'd data) that's safe
// to use after the mmap is closed.
func (bc *BinCache) filePath(i int) string {
	off := bc.fileEntryOffset(i)
	pathOff := int(binary.LittleEndian.Uint32(bc.data[off : off+4]))
	pathLen := int(binary.LittleEndian.Uint16(bc.data[off+4 : off+6]))
	return string(bc.data[bc.strTableOff+pathOff : bc.strTableOff+pathOff+pathLen])
}

// dirPath returns the path string for dir entry i.
func (bc *BinCache) dirPath(i int) string {
	off := bc.dirEntryOffset(i)
	pathOff := int(binary.LittleEndian.Uint32(bc.data[off : off+4]))
	pathLen := int(binary.LittleEndian.Uint16(bc.data[off+4 : off+6]))
	return string(bc.data[bc.strTableOff+pathOff : bc.strTableOff+pathOff+pathLen])
}

// LookupFile binary-searches for a file path. Returns mtime, size, digest, lang, ok.
func (bc *BinCache) LookupFile(path string) (int64, int64, string, string, bool) {
	lo, hi := 0, bc.fileCount-1
	for lo <= hi {
		mid := (lo + hi) / 2
		midPath := bc.filePath(mid)
		cmp := strings.Compare(path, midPath)
		if cmp == 0 {
			off := bc.fileEntryOffset(mid)
			mtime := int64(binary.LittleEndian.Uint64(bc.data[off+6 : off+14]))
			size := int64(binary.LittleEndian.Uint64(bc.data[off+14 : off+22]))
			digestBytes := bc.data[off+22 : off+54]
			digest := hexEncode(digestBytes)
			langByte := bc.data[off+54]
			lang := langStrings[langByte]
			return mtime, size, digest, lang, true
		} else if cmp < 0 {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	return 0, 0, "", "", false
}

// LookupDir binary-searches for a directory path. Returns mtime, ok.
func (bc *BinCache) LookupDir(path string) (int64, bool) {
	lo, hi := 0, bc.dirCount-1
	for lo <= hi {
		mid := (lo + hi) / 2
		midPath := bc.dirPath(mid)
		cmp := strings.Compare(path, midPath)
		if cmp == 0 {
			off := bc.dirEntryOffset(mid)
			mtime := int64(binary.LittleEndian.Uint64(bc.data[off+6 : off+14]))
			return mtime, true
		} else if cmp < 0 {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	return 0, false
}

// WriteBinCache writes the stat cache in binary format.
func WriteBinCache(kaiDir string, sc *StatCache) error {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	// Sort file paths
	filePaths := make([]string, 0, len(sc.Entries))
	for p := range sc.Entries {
		filePaths = append(filePaths, p)
	}
	sort.Strings(filePaths)

	// Sort dir paths
	dirPaths := make([]string, 0, len(sc.DirTimes))
	for p := range sc.DirTimes {
		dirPaths = append(dirPaths, p)
	}
	sort.Strings(dirPaths)

	// Build string table
	strTable := make([]byte, 0, len(filePaths)*30) // estimate
	strOffsets := make(map[string]int, len(filePaths)+len(dirPaths))
	addStr := func(s string) (int, int) {
		if off, ok := strOffsets[s]; ok {
			return off, len(s)
		}
		off := len(strTable)
		strTable = append(strTable, s...)
		strOffsets[s] = off
		return off, len(s)
	}

	// Pre-add all paths to string table
	for _, p := range filePaths {
		addStr(p)
	}
	for _, p := range dirPaths {
		addStr(p)
	}

	// Calculate sizes
	fileCount := len(filePaths)
	dirCount := len(dirPaths)
	strTableOff := headerSize + fileCount*fileEntrySize + dirCount*dirEntrySize
	totalSize := strTableOff + len(strTable)

	// Build buffer
	buf := make([]byte, totalSize)

	// Header
	binary.LittleEndian.PutUint32(buf[0:4], binMagic)
	binary.LittleEndian.PutUint32(buf[4:8], binVersion)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(fileCount))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(dirCount))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(strTableOff))

	// File entries
	for i, path := range filePaths {
		entry := sc.Entries[path]
		off := headerSize + i*fileEntrySize
		pathOff, pathLen := addStr(path)
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(pathOff))
		binary.LittleEndian.PutUint16(buf[off+4:off+6], uint16(pathLen))
		binary.LittleEndian.PutUint64(buf[off+6:off+14], uint64(entry.ModTime))
		binary.LittleEndian.PutUint64(buf[off+14:off+22], uint64(entry.Size))
		hexDecode(entry.Digest, buf[off+22:off+54])
		lb := langBytes[entry.Lang]
		buf[off+54] = lb
	}

	// Dir entries
	for i, path := range dirPaths {
		off := headerSize + fileCount*fileEntrySize + i*dirEntrySize
		pathOff, pathLen := addStr(path)
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(pathOff))
		binary.LittleEndian.PutUint16(buf[off+4:off+6], uint16(pathLen))
		binary.LittleEndian.PutUint64(buf[off+6:off+14], uint64(sc.DirTimes[path]))
	}

	// String table
	copy(buf[strTableOff:], strTable)

	// Atomic write
	tmpPath := filepath.Join(kaiDir, "statcache.bin.tmp")
	if err := os.WriteFile(tmpPath, buf, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, filepath.Join(kaiDir, "statcache.bin"))
}

// hexEncode converts 32 bytes to 64-char hex string.
func hexEncode(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}

// hexDecode converts hex string to bytes. Writes to dst.
func hexDecode(s string, dst []byte) {
	for i := 0; i < len(dst) && i*2+1 < len(s); i++ {
		dst[i] = unhex(s[i*2])<<4 | unhex(s[i*2+1])
	}
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
