//go:build windows

package tui

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// raisePriority is the Windows counterpart of a -5 nice: one priority
// class above normal. Best-effort, errors ignored as on unix.
func raisePriority() {
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.ABOVE_NORMAL_PRIORITY_CLASS)
}

// reraise cannot send a signal to ourselves on Windows; exit with the
// status a signal death would have produced (128+signum) so callers that
// check $? see the same thing they would on unix.
func reraise(s os.Signal) {
	code := 1
	if sig, ok := s.(syscall.Signal); ok {
		code = 128 + int(sig)
	}
	os.Exit(code)
}
