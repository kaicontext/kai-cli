//go:build unix

package kitlauncher

import (
	"os"
	"path/filepath"
	"testing"
)

// Only the owner bit counts: kai runs kit as the owner, so 0o070 must not
// pass even though an any-bit (&0o111) test would accept it.
func TestIsExecutable_OwnerBit(t *testing.T) {
	for _, tt := range []struct {
		mode os.FileMode
		want bool
	}{
		{0o755, true},
		{0o700, true},
		{0o644, false},
		{0o070, false},
	} {
		p := filepath.Join(t.TempDir(), "kit")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, tt.mode); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := isExecutable(info); got != tt.want {
			t.Errorf("isExecutable(%v) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}
