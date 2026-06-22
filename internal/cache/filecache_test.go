package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheHitAndMiss(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create cache
	cache, err := Open(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("hello world")
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatal(err)
	}

	// First call should compute
	digest1, err := cache.GetOrCompute("test.txt", info, content)
	if err != nil {
		t.Fatal(err)
	}
	if digest1 == "" {
		t.Error("expected non-empty digest")
	}

	// Second call should hit cache (same content returns same digest)
	digest2, err := cache.GetOrCompute("test.txt", info, content)
	if err != nil {
		t.Fatal(err)
	}
	if digest1 != digest2 {
		t.Errorf("expected same digest, got %s and %s", digest1, digest2)
	}

	// Verify it's in cache
	cachedDigest, err := cache.GetDigest("test.txt", info)
	if err != nil {
		t.Fatal(err)
	}
	if cachedDigest != digest1 {
		t.Errorf("expected cached digest %s, got %s", digest1, cachedDigest)
	}
}

func TestCacheInvalidation(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create cache
	cache, err := Open(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	content1 := []byte("hello world")
	if err := os.WriteFile(testFile, content1, 0644); err != nil {
		t.Fatal(err)
	}

	info1, err := os.Stat(testFile)
	if err != nil {
		t.Fatal(err)
	}

	// Cache the digest
	digest1, err := cache.GetOrCompute("test.txt", info1, content1)
	if err != nil {
		t.Fatal(err)
	}

	// Wait a bit and modify the file
	time.Sleep(10 * time.Millisecond)
	content2 := []byte("goodbye world - changed!")
	if err := os.WriteFile(testFile, content2, 0644); err != nil {
		t.Fatal(err)
	}

	info2, err := os.Stat(testFile)
	if err != nil {
		t.Fatal(err)
	}

	// Cache should be invalidated (different size/mtime)
	cachedDigest, err := cache.GetDigest("test.txt", info2)
	if err != nil {
		t.Fatal(err)
	}
	if cachedDigest != "" {
		t.Error("expected cache miss after file change")
	}

	// GetOrCompute should recompute
	digest2, err := cache.GetOrCompute("test.txt", info2, content2)
	if err != nil {
		t.Fatal(err)
	}
	if digest1 == digest2 {
		t.Error("expected different digest for different content")
	}
}

func TestCacheClear(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create cache
	cache, err := Open(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	// Create and cache a file
	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("hello")
	if err := os.WriteFile(testFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatal(err)
	}

	_, err = cache.GetOrCompute("test.txt", info, content)
	if err != nil {
		t.Fatal(err)
	}

	// Verify entry exists
	stats, err := cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalEntries != 1 {
		t.Errorf("expected 1 entry, got %d", stats.TotalEntries)
	}

	// Clear cache
	if err := cache.Clear(); err != nil {
		t.Fatal(err)
	}

	// Verify empty
	stats, err = cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalEntries != 0 {
		t.Errorf("expected 0 entries after clear, got %d", stats.TotalEntries)
	}
}

func TestCacheRemove(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "cache-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create cache
	cache, err := Open(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	// Create and cache two files
	for _, name := range []string{"file1.txt", "file2.txt"} {
		testFile := filepath.Join(tmpDir, name)
		content := []byte("content of " + name)
		if err := os.WriteFile(testFile, content, 0644); err != nil {
			t.Fatal(err)
		}

		info, err := os.Stat(testFile)
		if err != nil {
			t.Fatal(err)
		}

		_, err = cache.GetOrCompute(name, info, content)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Verify 2 entries
	stats, err := cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalEntries != 2 {
		t.Errorf("expected 2 entries, got %d", stats.TotalEntries)
	}

	// Remove one
	if err := cache.Remove("file1.txt"); err != nil {
		t.Fatal(err)
	}

	// Verify 1 entry remaining
	stats, err = cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalEntries != 1 {
		t.Errorf("expected 1 entry after remove, got %d", stats.TotalEntries)
	}
}
