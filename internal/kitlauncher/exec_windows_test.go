//go:build windows

package kitlauncher

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows has no execute bits: os.Stat reports a regular file as 0666, so
// a valid kit.exe must resolve rather than fail as "not executable".
func TestResolveKitPath_WindowsManagedNoExecBits(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o444} { // 0444 = read-only attribute
		t.Run(mode.String(), func(t *testing.T) {
			l := testLauncher(t)
			managed := filepath.Join(l.BinDir, kitBinaryName)
			if err := os.WriteFile(managed, []byte("MZ"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(managed, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(managed, 0o644) }) // let TempDir remove it
			got, err := l.resolveKitPath()
			if err != nil {
				t.Fatalf("kit.exe without exec bits must resolve on Windows: %v", err)
			}
			if got != managed {
				t.Errorf("got %q, want %q", got, managed)
			}
		})
	}
}

func TestResolveKitPath_WindowsEnvOverrideNoExecBits(t *testing.T) {
	kit := filepath.Join(t.TempDir(), kitBinaryName)
	if err := os.WriteFile(kit, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAI_KIT_BIN", kit)
	l := &Launcher{BinDir: t.TempDir()}
	got, err := l.resolveKitPath()
	if err != nil || got != kit {
		t.Fatalf("KAI_KIT_BIN override must resolve on Windows: %q %v", got, err)
	}
}
