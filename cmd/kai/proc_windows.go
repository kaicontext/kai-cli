//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE from winbase.h — the exit code
// GetExitCodeProcess reports for a process that has not exited.
const stillActive = 259

// processAlive reports whether pid is running. os.Process.Signal(0) is
// "not supported" on Windows and would read every daemon as dead, so
// this asks the kernel directly.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// tryLockFile takes a non-blocking exclusive lock on the whole of f via
// LockFileEx — the Windows analogue of flock(LOCK_EX|LOCK_NB). Like
// flock, the lock is tied to the handle and released when f closes or the
// process exits.
func tryLockFile(f *os.File) (held bool, err error) {
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, ^uint32(0), ^uint32(0), ol)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), ol)
}

// detachFromSession starts cmd with no console and in its own process
// group, so closing the launching terminal does not take it down.
func detachFromSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// terminateProcess: Windows has no catchable SIGTERM for a process we
// did not start, so this is a hard stop. The daemon's lock and pidfile
// are designed to survive exactly that (kill -9 semantics on unix).
func terminateProcess(proc *os.Process) { _ = proc.Kill() }

// The live-sync checkpoint request rides on SIGUSR1, which Windows does
// not have. Until a named-pipe control channel lands, the daemon never
// receives one and `kai live checkpoint` reports that plainly.
func notifyCheckpoint(ch chan<- os.Signal) {}

func sendCheckpoint(proc *os.Process) error {
	return errors.New("kai live checkpoint is not supported on Windows yet")
}
