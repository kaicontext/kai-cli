//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// processAlive reports whether pid is running, via the null signal.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// tryLockFile takes a non-blocking exclusive flock on f. held=false means
// another process holds it; err is any other failure. The lock rides on
// the descriptor, so closing f (or exiting, even by kill -9) releases it.
func tryLockFile(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// detachFromSession starts cmd in its own session so it survives the
// terminal that launched it closing.
func detachFromSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess asks proc to exit cleanly (SIGTERM).
func terminateProcess(proc *os.Process) { _ = proc.Signal(syscall.SIGTERM) }

// notifyCheckpoint / sendCheckpoint carry the `kai live checkpoint`
// request to the running daemon over SIGUSR1.
func notifyCheckpoint(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGUSR1) }

func sendCheckpoint(proc *os.Process) error { return proc.Signal(syscall.SIGUSR1) }
