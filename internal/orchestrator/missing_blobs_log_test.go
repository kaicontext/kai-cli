package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogMissingBlobsContext_ExtractsDigests pins the digest-extraction
// helper for the in-process self-heal path. The orchestrator dumps
// missing-blob digests to <mainRepo>/.kai/missing-blobs.log so a
// recurring root cause (same digests reappearing across heals) is
// distinguishable from a one-off (different digests each time, e.g.
// blobs being dropped between captures).
func TestLogMissingBlobsContext_ExtractsDigests(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".kai"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	output := `kai checkout: open .kai/objects/00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff: no such file or directory
some other line that should be ignored
also: .kai/objects/aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899: no such file or directory
.kai/objects/00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff: no such file or directory
`
	logMissingBlobsContext(tmp, output)
	logBytes, err := os.ReadFile(filepath.Join(tmp, ".kai", "missing-blobs.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	body := string(logBytes)
	if !strings.Contains(body, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff") {
		t.Errorf("first digest missing from log:\n%s", body)
	}
	if !strings.Contains(body, "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899") {
		t.Errorf("second digest missing from log:\n%s", body)
	}
	// Dedup: 2 unique digests in the input even though one repeats.
	if c := strings.Count(body, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"); c != 1 {
		t.Errorf("expected first digest to appear once (dedup), got %d", c)
	}
}

func TestLogMissingBlobsContext_NoMatchSilent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".kai"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logMissingBlobsContext(tmp, "spawn failed because of something else entirely")
	if _, err := os.Stat(filepath.Join(tmp, ".kai", "missing-blobs.log")); !os.IsNotExist(err) {
		t.Error("expected no log file when no digests in spawn output")
	}
}

func TestLogMissingBlobsContext_BadDigestLengthIgnored(t *testing.T) {
	// Hex sequences shorter or longer than 64 chars are not valid
	// blob digests and must not be logged. Otherwise the log would
	// fill with paths like ".kai/objects/abcdef" from unrelated
	// error messages.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".kai"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logMissingBlobsContext(tmp, ".kai/objects/short: nope\n.kai/objects/0123456789012345: also short")
	if _, err := os.Stat(filepath.Join(tmp, ".kai", "missing-blobs.log")); !os.IsNotExist(err) {
		t.Error("short hex sequences should not produce a log entry")
	}
}
