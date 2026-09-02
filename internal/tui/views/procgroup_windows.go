//go:build windows

package views

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup starts the managed process in a new console process
// group. Windows has no POSIX process groups, so teardown walks the
// child tree by parent pid (taskkill /T) rather than signalling a group.
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateGroup force-kills the process tree. There is no catchable
// SIGTERM for a console child on Windows, so the "graceful first" step
// of the unix path collapses into the forced one.
func terminateGroup(cmd *exec.Cmd) { killGroup(cmd) }

// killGroup runs taskkill /F /T against the parent pid — the Windows
// analogue of the negative-pid SIGKILL — with a plain Kill fallback.
func killGroup(cmd *exec.Cmd) {
	if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
