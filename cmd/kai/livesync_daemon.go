package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kai/internal/remote"
)

// Auto-sync daemon management. `kai ws checkout <ws>` spawns a detached
// `kai live run` so live peer-sync is on by default — no toggle to forget.
// It's best-effort and network-aware: checkout never fails because of sync,
// and the daemon tolerates being offline (it waits and connects when the
// network returns).

func autoSyncPidPath(kaiDir string) string { return filepath.Join(kaiDir, "autosync.pid") }
func autoSyncWsPath(kaiDir string) string  { return filepath.Join(kaiDir, "autosync.ws") }
func autoSyncLogPath(kaiDir string) string { return filepath.Join(kaiDir, "autosync.log") }

// autoSyncOffPath is a persistent opt-out sentinel (set by `kai live off`,
// cleared by `kai live on`) so a user can keep a workspace private.
func autoSyncOffPath(kaiDir string) string { return filepath.Join(kaiDir, "autosync-off") }

// liveRunPidPath is where a running `kai live run` publishes its own pid so
// `kai live checkpoint` can signal it (SIGUSR1).
func liveRunPidPath(kaiDir string) string { return filepath.Join(kaiDir, "livesync.pid") }

// syncModePath persists the per-repo sync mode ("checkpoint"); absent = "live".
func syncModePath(kaiDir string) string { return filepath.Join(kaiDir, "sync-mode") }

// readSyncMode returns "checkpoint" or "live" (the default).
func readSyncMode(kaiDir string) string {
	data, err := os.ReadFile(syncModePath(kaiDir))
	if err != nil {
		return "live"
	}
	if strings.TrimSpace(string(data)) == "checkpoint" {
		return "checkpoint"
	}
	return "live"
}

// probeOnline does a short-timeout health check so we don't hang or spew
// errors when the network or the server is unreachable.
func probeOnline(baseURL string) bool {
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(strings.TrimRight(baseURL, "/") + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// acquireLiveRunLock takes an exclusive, non-blocking flock on
// <kaiDir>/livesync.lock so AT MOST ONE `kai live run` daemon ever syncs a repo
// at a time — no matter how it was started (auto-spawn on checkout, a manual
// run, or a race). Without this, orphaned daemons from repeated
// checkout/off cycles pile up; since each holds its own in-memory RGA Doc but
// they all write the same files, they fight and corrupt the working tree.
//
// The lock is held for the process lifetime via the returned *os.File (the
// caller keeps it open) and is released by the OS on exit — even kill -9 — so
// there is no stale-lock problem. Returns (file, true) when acquired;
// (nil, false) when another live-run already holds it. On an unexpected
// lock error it fails OPEN (returns nil, true) so sync is never blocked by a
// lockfile quirk.
func acquireLiveRunLock(kaiDir string) (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(kaiDir, "livesync.lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, true // fail open
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, false // another daemon holds it
		}
		return nil, true // unexpected error — fail open
	}
	return f, true
}

// autoSyncRunningPid returns the pid of a live auto-sync daemon, or 0.
func autoSyncRunningPid(kaiDir string) int {
	data, err := os.ReadFile(autoSyncPidPath(kaiDir))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0
	}
	return pid
}

// stopAutoSync stops a running auto-sync daemon (if any) and clears its
// pidfiles. Safe to call when none is running.
func stopAutoSync(kaiDir string) {
	if pid := autoSyncRunningPid(kaiDir); pid > 0 {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(autoSyncPidPath(kaiDir))
	_ = os.Remove(autoSyncWsPath(kaiDir))
}

// startAutoSync spawns a detached `kai live run` for wsName, best-effort.
// Skips silently when auto-sync is opted out, no remote/auth is configured,
// or a daemon is already running for the same workspace. Never returns an
// error — checkout must not fail because of sync.
func startAutoSync(kaiDir, wsName string) {
	if os.Getenv("KAI_NO_AUTOSYNC") != "" {
		return
	}
	if _, err := os.Stat(autoSyncOffPath(kaiDir)); err == nil {
		return // user opted this repo out via `kai live off`
	}

	// Already running for this same workspace? Leave it.
	if pid := autoSyncRunningPid(kaiDir); pid > 0 {
		if cur, _ := os.ReadFile(autoSyncWsPath(kaiDir)); strings.TrimSpace(string(cur)) == wsName {
			return
		}
		stopAutoSync(kaiDir) // switching workspaces — restart for the new one
	}

	// Need a remote + auth to sync at all. If absent, stay silent — local
	// work is unaffected.
	client, err := remote.NewClientForRemote("origin")
	if err != nil || client.AuthToken == "" {
		return
	}

	logFile, err := os.OpenFile(autoSyncLogPath(kaiDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer logFile.Close()

	runArgs := []string{"live", "run"}
	if readSyncMode(kaiDir) == "checkpoint" {
		runArgs = append(runArgs, "--checkpoint")
	}
	cmd := exec.Command(kaiExe(), runArgs...)
	// Inherit the repo cwd (where .kai lives); detach into its own session so
	// it survives the terminal closing.
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	_ = os.WriteFile(autoSyncPidPath(kaiDir), []byte(strconv.Itoa(pid)), 0644)
	_ = os.WriteFile(autoSyncWsPath(kaiDir), []byte(wsName), 0644)

	if probeOnline(client.BaseURL) {
		fmt.Printf("Live sync on for %q (pid %d) — logs: %s\n", wsName, pid, autoSyncLogPath(kaiDir))
	} else {
		fmt.Printf("Live sync armed for %q (pid %d) — offline, will connect when the network returns\n", wsName, pid)
	}
}
