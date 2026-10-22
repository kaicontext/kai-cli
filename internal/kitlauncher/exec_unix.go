//go:build unix

package kitlauncher

import (
	"os"
	"syscall"
)

// kitBinaryName is the managed kit's file name inside BinDir.
const kitBinaryName = "kit"

// isExecutable reports whether the owner execute bit is set. Owner
// specifically: kai runs kit as the owner, so a group/other-only bit
// would pass an any-bit test yet still fail at syscall.Exec.
func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o100 != 0
}

// execHandoff replaces this process with kit. Returns only on failure.
func execHandoff(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}
