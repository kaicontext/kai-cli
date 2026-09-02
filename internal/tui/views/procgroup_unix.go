//go:build unix

package views

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the managed process in its own process group so
// terminateGroup/killGroup reach every child (concurrently's vite +
// electron case) instead of orphaning them.
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup SIGTERMs the whole group (negative pid), falling back to
// signalling just the parent when the group kill fails (e.g. macOS
// missing privileges).
func terminateGroup(cmd *exec.Cmd) {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}

// killGroup SIGKILLs the whole group, falling back to the parent alone.
func killGroup(cmd *exec.Cmd) {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
