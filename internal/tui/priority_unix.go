//go:build unix

package tui

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// raisePriority lowers our nice value by 5 — see the call site in Run for
// why. Best-effort: EPERM (the common non-root case) is ignored.
func raisePriority() { _ = unix.Setpriority(unix.PRIO_PROCESS, 0, -5) }

// reraise re-delivers s to ourselves after its handler has been reset, so
// the process exits with the conventional 128+signum status.
func reraise(s os.Signal) {
	if sig, ok := s.(syscall.Signal); ok {
		_ = syscall.Kill(syscall.Getpid(), sig)
	}
}
