package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoImportEnabled_DefaultTrue(t *testing.T) {
	chdirTemp(t)
	oldKaiDir := kaiDir
	kaiDir = ".kai"
	defer func() { kaiDir = oldKaiDir }()

	// No config at all → the git→kai direction defaults on.
	if !autoImportEnabled() {
		t.Fatal("autoImportEnabled() false with no config; default must be true")
	}

	// Explicit opt-out wins.
	if err := os.MkdirAll(kaiDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kaiDir, "config.yaml"), []byte("bridge:\n  auto_import: false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if autoImportEnabled() {
		t.Fatal("autoImportEnabled() true despite bridge.auto_import: false")
	}
}

// TestImportHookScripts_RunInBackground pins the default-on latency
// guarantee: every import hook backgrounds its kai invocation so git
// returns immediately. A synchronous `kai bridge import` in a default
// hook would put a capture on every commit's critical path.
func TestImportHookScripts_RunInBackground(t *testing.T) {
	for name, s := range map[string]string{
		"postCommit":   postCommitHookScript,
		"postMerge":    postMergeHookScript,
		"postCheckout": postCheckoutHookScript,
		"postRewrite":  postRewriteHookScript,
	} {
		if !strings.Contains(s, ") &") {
			t.Errorf("%s hook runs its import synchronously (no backgrounded subshell)", name)
		}
	}
	// The pre-* hooks must NOT background anything — they exist to gate.
	for name, s := range map[string]string{
		"preCommit": preCommitHookScript,
		"prePush":   prePushHookScript,
	} {
		if strings.Contains(s, ") &") {
			t.Errorf("%s hook backgrounds work; pre-hooks must stay synchronous", name)
		}
	}
}

// The post-rewrite hook reads stdin, which git closes when the hook
// process exits — the script must buffer stdin before backgrounding.
func TestPostRewriteHookScript_BuffersStdinBeforeBackgrounding(t *testing.T) {
	if !strings.Contains(postRewriteHookScript, "PAIRS=$(cat)") {
		t.Fatal("post-rewrite hook must buffer stdin before the backgrounded subshell")
	}
}
