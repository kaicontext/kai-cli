//go:build unix

package kitlauncher

import "syscall"

// kitBinaryName is the managed kit's file name inside BinDir.
const kitBinaryName = "kit"

// execHandoff replaces this process with kit. Returns only on failure.
func execHandoff(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}
