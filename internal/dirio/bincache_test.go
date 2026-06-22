package dirio

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBinCache_WriteAndLoad(t *testing.T) {
	dir := t.TempDir()

	// Create a stat cache with entries
	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"src/main.go":    {ModTime: 1000, Size: 500, Digest: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234", Lang: "go"},
			"src/auth.go":    {ModTime: 2000, Size: 300, Digest: "1111222233334444555566667777888811112222333344445555666677778888", Lang: "go"},
			"README.md":      {ModTime: 3000, Size: 100, Digest: "aaaabbbbccccddddeeeeffffaaaabbbbccccddddeeeeffffaaaabbbbccccdddd", Lang: "markdown"},
			"config.json":    {ModTime: 4000, Size: 200, Digest: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Lang: "json"},
			"app.ts":         {ModTime: 5000, Size: 1000, Digest: "0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff", Lang: "ts"},
		},
		DirTimes: map[string]int64{
			"src":        9000,
			"src/models": 8000,
			"lib":        7000,
		},
	}

	// Write binary cache
	if err := WriteBinCache(dir, sc); err != nil {
		t.Fatalf("WriteBinCache: %v", err)
	}

	// Verify file exists
	binPath := filepath.Join(dir, "statcache.bin")
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("statcache.bin not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("statcache.bin is empty")
	}

	// Load it back
	bc := LoadBinCache(dir)
	if bc == nil {
		t.Fatal("LoadBinCache returned nil")
	}
	defer bc.Close()

	// Verify file count
	if bc.fileCount != 5 {
		t.Errorf("fileCount = %d, want 5", bc.fileCount)
	}
	if bc.dirCount != 3 {
		t.Errorf("dirCount = %d, want 3", bc.dirCount)
	}
}

func TestBinCache_LookupFile(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"src/main.go": {ModTime: 1000, Size: 500, Digest: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234", Lang: "go"},
			"src/auth.go": {ModTime: 2000, Size: 300, Digest: "1111222233334444555566667777888811112222333344445555666677778888", Lang: "go"},
			"README.md":   {ModTime: 3000, Size: 100, Digest: "aaaabbbbccccddddeeeeffffaaaabbbbccccddddeeeeffffaaaabbbbccccdddd", Lang: "markdown"},
		},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	// Lookup existing file
	mtime, size, digest, lang, ok := bc.LookupFile("src/main.go")
	if !ok {
		t.Fatal("LookupFile(src/main.go) not found")
	}
	if mtime != 1000 {
		t.Errorf("mtime = %d, want 1000", mtime)
	}
	if size != 500 {
		t.Errorf("size = %d, want 500", size)
	}
	if digest != "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234" {
		t.Errorf("digest = %s", digest)
	}
	if lang != "go" {
		t.Errorf("lang = %s, want go", lang)
	}

	// Lookup another file
	_, _, _, lang, ok = bc.LookupFile("README.md")
	if !ok {
		t.Fatal("LookupFile(README.md) not found")
	}
	if lang != "markdown" {
		t.Errorf("lang = %s, want markdown", lang)
	}

	// Lookup non-existent file
	_, _, _, _, ok = bc.LookupFile("nonexistent.go")
	if ok {
		t.Error("LookupFile(nonexistent.go) should return false")
	}
}

func TestBinCache_LookupDir(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries: map[string]*StatEntry{},
		DirTimes: map[string]int64{
			"src":        9000,
			"src/models": 8000,
			"lib":        7000,
		},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	// Lookup existing dir
	mtime, ok := bc.LookupDir("src")
	if !ok {
		t.Fatal("LookupDir(src) not found")
	}
	if mtime != 9000 {
		t.Errorf("mtime = %d, want 9000", mtime)
	}

	mtime, ok = bc.LookupDir("src/models")
	if !ok {
		t.Fatal("LookupDir(src/models) not found")
	}
	if mtime != 8000 {
		t.Errorf("mtime = %d, want 8000", mtime)
	}

	// Non-existent dir
	_, ok = bc.LookupDir("nonexistent")
	if ok {
		t.Error("LookupDir(nonexistent) should return false")
	}
}

func TestBinCache_BinarySearchCorrectness(t *testing.T) {
	dir := t.TempDir()

	// Create entries that test binary search edge cases
	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"a.go":           {ModTime: 1, Size: 1, Digest: "0000000000000000000000000000000000000000000000000000000000000001", Lang: "go"},
			"b.go":           {ModTime: 2, Size: 2, Digest: "0000000000000000000000000000000000000000000000000000000000000002", Lang: "go"},
			"c.go":           {ModTime: 3, Size: 3, Digest: "0000000000000000000000000000000000000000000000000000000000000003", Lang: "go"},
			"z/deep/file.go": {ModTime: 4, Size: 4, Digest: "0000000000000000000000000000000000000000000000000000000000000004", Lang: "go"},
		},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	// First entry
	_, _, _, _, ok := bc.LookupFile("a.go")
	if !ok {
		t.Error("first entry (a.go) not found")
	}

	// Last entry
	_, _, _, _, ok = bc.LookupFile("z/deep/file.go")
	if !ok {
		t.Error("last entry (z/deep/file.go) not found")
	}

	// Middle entry
	_, _, _, _, ok = bc.LookupFile("b.go")
	if !ok {
		t.Error("middle entry (b.go) not found")
	}

	// Just before first
	_, _, _, _, ok = bc.LookupFile("0.go")
	if ok {
		t.Error("entry before first should not be found")
	}

	// Just after last
	_, _, _, _, ok = bc.LookupFile("zzz.go")
	if ok {
		t.Error("entry after last should not be found")
	}
}

func TestBinCache_EmptyCache(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries:  map[string]*StatEntry{},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	if bc == nil {
		t.Fatal("LoadBinCache returned nil for empty cache")
	}
	defer bc.Close()

	if bc.fileCount != 0 {
		t.Errorf("fileCount = %d, want 0", bc.fileCount)
	}
	if bc.dirCount != 0 {
		t.Errorf("dirCount = %d, want 0", bc.dirCount)
	}

	_, _, _, _, ok := bc.LookupFile("anything")
	if ok {
		t.Error("lookup on empty cache should return false")
	}
}

func TestBinCache_InvalidFile(t *testing.T) {
	dir := t.TempDir()

	// No file
	bc := LoadBinCache(dir)
	if bc != nil {
		t.Error("LoadBinCache should return nil when no file exists")
	}

	// Corrupt file
	os.WriteFile(filepath.Join(dir, "statcache.bin"), []byte("garbage"), 0644)
	bc = LoadBinCache(dir)
	if bc != nil {
		t.Error("LoadBinCache should return nil for corrupt file")
	}

	// Too small
	os.WriteFile(filepath.Join(dir, "statcache.bin"), []byte{0, 0, 0, 0}, 0644)
	bc = LoadBinCache(dir)
	if bc != nil {
		t.Error("LoadBinCache should return nil for too-small file")
	}
}

func TestBinCache_LangRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Test all language byte encodings round-trip correctly
	langs := []string{"go", "python", "ruby", "rust", "js", "ts", "jsx", "tsx",
		"json", "yaml", "toml", "xml", "markdown", "text", "ini", "env",
		"java", "c", "cpp", "csharp", "php", "swift", "kotlin", "shell",
		"sql", "html", "css", "blob"}

	entries := make(map[string]*StatEntry)
	for i, lang := range langs {
		digest := "0000000000000000000000000000000000000000000000000000000000000000"
		// Make each digest unique
		d := []byte(digest)
		d[63] = byte('a' + i%26)
		entries["file_"+lang+".txt"] = &StatEntry{
			ModTime: int64(i), Size: int64(i), Digest: string(d), Lang: lang,
		}
	}

	sc := &StatCache{Entries: entries, DirTimes: map[string]int64{}}
	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	for _, lang := range langs {
		_, _, _, gotLang, ok := bc.LookupFile("file_" + lang + ".txt")
		if !ok {
			t.Errorf("file_%s.txt not found", lang)
			continue
		}
		if gotLang != lang {
			t.Errorf("file_%s.txt: lang = %q, want %q", lang, gotLang, lang)
		}
	}
}

func TestBinCache_LargeCache(t *testing.T) {
	dir := t.TempDir()

	// Simulate a moderately large repo
	entries := make(map[string]*StatEntry)
	dirTimes := make(map[string]int64)
	for i := 0; i < 1000; i++ {
		path := fmt.Sprintf("src/pkg%03d/file%04d.go", i/10, i)
		digest := "0000000000000000000000000000000000000000000000000000000000000000"
		entries[path] = &StatEntry{ModTime: int64(i * 1000), Size: int64(i * 100), Digest: digest, Lang: "go"}
	}
	for i := 0; i < 100; i++ {
		dirPath := fmt.Sprintf("src/pkg%03d", i)
		dirTimes[dirPath] = int64(i * 5000)
	}

	sc := &StatCache{Entries: entries, DirTimes: dirTimes}
	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	if bc == nil {
		t.Fatal("LoadBinCache returned nil for large cache")
	}
	defer bc.Close()

	if bc.fileCount != 1000 {
		t.Errorf("fileCount = %d, want 1000", bc.fileCount)
	}

	// Spot check some lookups
	found := 0
	for path := range entries {
		_, _, _, _, ok := bc.LookupFile(path)
		if ok {
			found++
		}
	}
	if found != 1000 {
		t.Errorf("found %d of 1000 files", found)
	}
}

func TestStatCache_BinCacheIntegration(t *testing.T) {
	dir := t.TempDir()

	// Create stat cache, save it (writes binary), reload it (reads binary)
	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"main.go":    {ModTime: 1000, Size: 500, Digest: "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234", Lang: "go"},
			"config.yml": {ModTime: 2000, Size: 200, Digest: "1111222233334444555566667777888811112222333344445555666677778888", Lang: "yaml"},
		},
		DirTimes: map[string]int64{
			"src": 5000,
		},
	}

	// Save (should write binary format)
	if err := sc.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify binary file exists
	if _, err := os.Stat(filepath.Join(dir, "statcache.bin")); err != nil {
		t.Fatalf("statcache.bin not created: %v", err)
	}

	// Reload — should use binary cache for reads
	sc2 := LoadStatCache(dir)
	if sc2.bin == nil {
		t.Fatal("loaded cache should have binary cache attached")
	}

	// Lookup through the stat cache (which delegates to binary)
	digest, lang, ok := sc2.LookupByPath("main.go")
	if !ok {
		t.Fatal("LookupByPath(main.go) not found")
	}
	if digest != "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234" {
		t.Errorf("digest = %s", digest)
	}
	if lang != "go" {
		t.Errorf("lang = %s", lang)
	}

	// Dir lookup through stat cache
	if !sc2.DirUnchanged("src", fakeFileInfo{mtime: 5000}) {
		t.Error("DirUnchanged(src) should be true for matching mtime")
	}
	if sc2.DirUnchanged("src", fakeFileInfo{mtime: 6000}) {
		t.Error("DirUnchanged(src) should be false for different mtime")
	}

	// Clean up mmap
	if sc2.bin != nil {
		sc2.bin.Close()
	}
}

func TestBinCache_DigestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Test that hex digest encoding/decoding is exact
	digest := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"test.go": {ModTime: 100, Size: 50, Digest: digest, Lang: "go"},
		},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	_, _, gotDigest, _, ok := bc.LookupFile("test.go")
	if !ok {
		t.Fatal("not found")
	}
	if gotDigest != digest {
		t.Errorf("digest mismatch:\n  got:  %s\n  want: %s", gotDigest, digest)
	}
}

func TestBinCache_SpecialCharsInPath(t *testing.T) {
	dir := t.TempDir()

	// Paths with brackets (SvelteKit), dots, dashes, underscores
	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"src/routes/[slug]/[repo]/[...path]/+page.svelte": {ModTime: 1, Size: 1, Digest: "0000000000000000000000000000000000000000000000000000000000000001", Lang: "blob"},
			"src/my-component.test.tsx":                        {ModTime: 2, Size: 2, Digest: "0000000000000000000000000000000000000000000000000000000000000002", Lang: "tsx"},
			"lib/__tests__/utils.spec.js":                      {ModTime: 3, Size: 3, Digest: "0000000000000000000000000000000000000000000000000000000000000003", Lang: "js"},
			".env.local":                                       {ModTime: 4, Size: 4, Digest: "0000000000000000000000000000000000000000000000000000000000000004", Lang: "env"},
		},
		DirTimes: map[string]int64{
			"src/routes/[slug]/[repo]/[...path]": 100,
		},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	_, _, _, _, ok := bc.LookupFile("src/routes/[slug]/[repo]/[...path]/+page.svelte")
	if !ok {
		t.Error("[...path] file not found — bracket chars in path broken")
	}

	_, _, _, _, ok = bc.LookupFile(".env.local")
	if !ok {
		t.Error("dotfile not found")
	}

	mtime, ok := bc.LookupDir("src/routes/[slug]/[repo]/[...path]")
	if !ok {
		t.Error("[...path] dir not found")
	}
	if mtime != 100 {
		t.Errorf("dir mtime = %d, want 100", mtime)
	}
}

func TestBinCache_SingleEntry(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"only.go": {ModTime: 42, Size: 99, Digest: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", Lang: "go"},
		},
		DirTimes: map[string]int64{
			"only_dir": 42,
		},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)
	defer bc.Close()

	if bc.fileCount != 1 {
		t.Errorf("fileCount = %d, want 1", bc.fileCount)
	}

	mtime, size, _, _, ok := bc.LookupFile("only.go")
	if !ok {
		t.Fatal("single file not found")
	}
	if mtime != 42 || size != 99 {
		t.Errorf("mtime=%d size=%d, want 42, 99", mtime, size)
	}

	_, ok = bc.LookupDir("only_dir")
	if !ok {
		t.Fatal("single dir not found")
	}
}

func TestBinCache_MmapCloseSafety(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries: map[string]*StatEntry{
			"a.go": {ModTime: 1, Size: 1, Digest: "0000000000000000000000000000000000000000000000000000000000000001", Lang: "go"},
			"b.go": {ModTime: 2, Size: 2, Digest: "0000000000000000000000000000000000000000000000000000000000000002", Lang: "go"},
		},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)

	// Copy strings BEFORE closing mmap
	_, _, digest1, _, _ := bc.LookupFile("a.go")
	_, _, digest2, _, _ := bc.LookupFile("b.go")
	path1 := bc.filePath(0)
	path2 := bc.filePath(1)

	// Close mmap
	bc.Close()

	// Strings should still be valid (they're copies, not mmap references)
	if digest1 != "0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("digest1 corrupted after Close: %s", digest1)
	}
	if digest2 != "0000000000000000000000000000000000000000000000000000000000000002" {
		t.Errorf("digest2 corrupted after Close: %s", digest2)
	}
	if path1 != "a.go" {
		t.Errorf("path1 corrupted after Close: %s", path1)
	}
	if path2 != "b.go" {
		t.Errorf("path2 corrupted after Close: %s", path2)
	}
}

func TestBinCache_DoubleClose(t *testing.T) {
	dir := t.TempDir()

	sc := &StatCache{
		Entries:  map[string]*StatEntry{"a.go": {ModTime: 1, Size: 1, Digest: "0000000000000000000000000000000000000000000000000000000000000001", Lang: "go"}},
		DirTimes: map[string]int64{},
	}

	WriteBinCache(dir, sc)
	bc := LoadBinCache(dir)

	// Double close should not panic
	bc.Close()
	bc.Close()
}

func TestBinCache_WriteThenOverwrite(t *testing.T) {
	dir := t.TempDir()

	// Write initial cache
	sc1 := &StatCache{
		Entries:  map[string]*StatEntry{"old.go": {ModTime: 1, Size: 1, Digest: "0000000000000000000000000000000000000000000000000000000000000001", Lang: "go"}},
		DirTimes: map[string]int64{},
	}
	WriteBinCache(dir, sc1)

	// Overwrite with different data
	sc2 := &StatCache{
		Entries:  map[string]*StatEntry{"new.go": {ModTime: 2, Size: 2, Digest: "0000000000000000000000000000000000000000000000000000000000000002", Lang: "go"}},
		DirTimes: map[string]int64{"newdir": 999},
	}
	WriteBinCache(dir, sc2)

	bc := LoadBinCache(dir)
	defer bc.Close()

	// Old entry should be gone
	_, _, _, _, ok := bc.LookupFile("old.go")
	if ok {
		t.Error("old.go should not exist after overwrite")
	}

	// New entry should be present
	_, _, _, _, ok = bc.LookupFile("new.go")
	if !ok {
		t.Error("new.go not found after overwrite")
	}

	_, ok = bc.LookupDir("newdir")
	if !ok {
		t.Error("newdir not found after overwrite")
	}
}

// fakeFileInfo implements os.FileInfo for testing
type fakeFileInfo struct {
	mtime int64
	fsize int64
}

func (f fakeFileInfo) Name() string        { return "" }
func (f fakeFileInfo) Size() int64         { return f.fsize }
func (f fakeFileInfo) Mode() os.FileMode   { return 0 }
func (f fakeFileInfo) ModTime() time.Time  { return time.Unix(0, f.mtime) }
func (f fakeFileInfo) IsDir() bool         { return false }
func (f fakeFileInfo) Sys() interface{}    { return nil }
