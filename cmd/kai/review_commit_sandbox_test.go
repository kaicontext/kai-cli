package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestReviewSandboxRestrictions(t *testing.T) {
	image := "example.org/review@sha256:" + strings.Repeat("a", 64)
	args, err := rcSandboxArgs(image, "kai-review-test")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, required := range []string{"--pull=never", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=65534:65534", "--pids-limit=32", "--memory=64m", "--memory-swap=64m", "--cpus=0.5", "--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=16m", "--entrypoint=/bin/sh"} {
		if !strings.Contains(joined, required) {
			t.Errorf("missing %s", required)
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--volume") || strings.HasPrefix(arg, "--mount") || strings.HasPrefix(arg, "--env") || arg == "--privileged" {
			t.Fatalf("sandbox exposes host resources: %v", args)
		}
	}
	for _, bad := range []string{"alpine:latest", "--privileged", "alpine@sha256:nope", "alpine@sha256:" + strings.Repeat("a", 63)} {
		if _, err := rcSandboxArgs(bad, "test"); err == nil {
			t.Errorf("accepted unpinned/invalid image %q", bad)
		}
	}
}

func TestReviewSandboxBoundsOutput(t *testing.T) {
	var out rcBoundedOutput
	p := []byte(strings.Repeat("x", 100000))
	for i := 0; i < 2; i++ {
		if n, err := out.Write(p); err != nil || n != len(p) {
			t.Fatalf("did not drain output: %d %v", n, err)
		}
	}
	if out.Len() != 16*1024 || !out.truncated {
		t.Fatalf("output was unbounded: %d", out.Len())
	}
}

// Opt-in because Docker is not installed on every developer/CI host. These
// execute the actual isolated tool, including timeout cleanup, rather than
// modeling the shell's semantics in the test.
func TestReviewSandboxDesktop418(t *testing.T) {
	if os.Getenv("KAI_REVIEW_SANDBOX_TEST") != "1" {
		t.Skip("set KAI_REVIEW_SANDBOX_TEST=1 with a preloaded KAI_REVIEW_SANDBOX_IMAGE")
	}
	sandbox := rcConfiguredSandbox()
	if sandbox == nil {
		t.Fatal("KAI_REVIEW_SANDBOX_IMAGE required")
	}
	for _, tc := range []struct{ name, script, want string }{
		{"cd persists", "mkdir -p /tmp/ws\ncd /tmp/ws && pwd\npwd\n", "stdout:\n/tmp/ws\n/tmp/ws\n"},
		{"original heredoc works", "cd /tmp && cat <<EOF\nhello\nEOF\n", "stdout:\nhello\n"},
		{"suggested wrapper breaks heredoc", "cd /tmp && { cat <<EOF\nhello\nEOF; }\n", "Exit code: 2"},
		{"double quoted path expands", "mkdir -p /tmp/ws\nPART=ws\ncd \"/tmp/$PART\" && pwd\n", "stdout:\n/tmp/ws\n"},
		{"cd failure leaves later line runnable", "cd /nonexistent-kai-review-directory && echo first\nprintf 'later line still runs\\n'\n", "later line still runs\n\nstderr:"},
		{"unprivileged identity", "id -u\n", "stdout:\n65534\n"},
		{"root filesystem is read only", "touch /etc/kai-review-test\n", "Exit code: 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"script": tc.script})
			got, err := sandbox.run(context.Background(), string(input))
			if err != nil || !strings.Contains(got, tc.want) {
				t.Fatalf("want %q; got %s, %v", tc.want, got, err)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		start := time.Now()
		_, err := sandbox.run(context.Background(), `{"script":"while :; do :; done"}`)
		if err == nil || !strings.Contains(err.Error(), "did not finish") || time.Since(start) > 10*time.Second {
			t.Fatalf("experiment was not bounded: %s %v", time.Since(start), err)
		}
	})
}
