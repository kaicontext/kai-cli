package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/tools"
)

// Optional, explicitly provisioned runtime. Never substitute host execution
// when Docker or the image is unavailable. CI operators preload a trusted,
// digest-pinned image with /bin/sh; snippets receive no mounts or host env.
type rcShellSandbox struct{ image string }

func rcConfiguredSandbox() *rcShellSandbox {
	image := strings.TrimSpace(os.Getenv("KAI_REVIEW_SANDBOX_IMAGE"))
	if image == "" {
		return nil
	}
	return &rcShellSandbox{image: image}
}

func rcShellToolInfo() tools.ToolInfo {
	return tools.ToolInfo{
		Name:        "review_shell",
		Description: "Run a small synthetic POSIX /bin/sh reproduction in a fresh, restricted container with no network, repository, credentials or host mounts. /tmp is writable and disposable. Five-second deadline. This does not reproduce an interactive PTY or a Windows shell. Do not install dependencies.",
		Parameters:  map[string]any{"script": map[string]any{"type": "string", "description": "Self-contained POSIX shell experiment (maximum 8192 bytes)."}},
		Required:    []string{"script"},
	}
}

var rcSandboxImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func rcSandboxArgs(image, name string) ([]string, error) {
	if !rcSandboxImagePattern.MatchString(image) {
		return nil, fmt.Errorf("KAI_REVIEW_SANDBOX_IMAGE must name a trusted, preloaded image pinned by sha256 digest")
	}
	return []string{"run", "--rm", "--pull=never", "--name", name,
		"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--user=65534:65534", "--pids-limit=32", "--memory=64m", "--memory-swap=64m", "--cpus=0.5",
		"--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=16m", "--workdir=/tmp",
		"--entrypoint=/bin/sh", "-i", image, "-s"}, nil
}

// Always drain the pipe while retaining a fixed amount, so an experiment cannot
// make the reviewer allocate unbounded output or block Docker on a full pipe.
type rcBoundedOutput struct {
	bytes.Buffer
	truncated bool
}

func (w *rcBoundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := 16*1024 - w.Len()
	if n > left {
		w.truncated = true
		p = p[:left]
	}
	w.Buffer.Write(p)
	return n, nil
}

func (s *rcShellSandbox) run(ctx context.Context, input string) (string, error) {
	var params struct {
		Script string `json:"script"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil || len(params.Script) == 0 || len(params.Script) > 8192 {
		return "", fmt.Errorf("experiment needs a script of 1–8192 bytes")
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	name := "kai-review-" + hex.EncodeToString(id[:])
	args, err := rcSandboxArgs(s.image, name)
	if err != nil {
		return "", err
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return "", fmt.Errorf("Docker is unavailable; host execution is disabled")
	}
	// Killing the Docker client does not necessarily stop its container.
	// Remove by our random name on every exit, including timeout/cancellation.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanup, docker, "rm", "-f", name)
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
	}()
	runctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runctx, docker, args...)
	cmd.Stdin = strings.NewReader(params.Script)
	cmd.WaitDelay = time.Second
	var stdout, stderr rcBoundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runctx.Err() != nil {
		return "", fmt.Errorf("experiment did not finish: %w", runctx.Err())
	}
	exitCode := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			return "", fmt.Errorf("experiment could not execute: %w", err)
		}
		exitCode = exit.ExitCode()
		if exitCode < 0 || exitCode >= 125 {
			return "", fmt.Errorf("container/runtime unavailable (exit %d): %s", exitCode, stderr.String())
		}
	}
	if stdout.truncated || stderr.truncated {
		return "", fmt.Errorf("experiment exceeded the output limit; incomplete output is not evidence")
	}
	return fmt.Sprintf("Runtime: %s /bin/sh (non-interactive, isolated)\nScript:\n%s\nExit code: %d\nstdout:\n%s\nstderr:\n%s", s.image, params.Script, exitCode, stdout.String(), stderr.String()), nil
}
