package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestConfiguredOrigin_NoRemote: a repo with no `origin` (kai init
// --no-remote) must not yield a client. remote.NewClientForRemote("origin")
// falls back to the default server with org "default", which made
// `kai ws checkout` start a live-sync daemon that retried forever against
// "HTTP 404 org not found".
func TestConfiguredOrigin_NoRemote(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	client, err := configuredOrigin()
	if client != nil {
		// Never print the client itself: it carries the auth token.
		t.Fatalf("configuredOrigin() returned a client for %s %s/%s, want none", client.BaseURL, client.Tenant, client.Repo)
	}
	if !errors.Is(err, errNoOrigin) {
		t.Fatalf("configuredOrigin() error = %v, want errNoOrigin", err)
	}
}

func TestConfiguredOrigin_WithRemote(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(".kai", 0755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"remotes":{"origin":{"url":"https://kai.example","tenant":"acme","repo":"widgets"}}}`
	if err := os.WriteFile(filepath.Join(".kai", "remotes.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := configuredOrigin()
	if err != nil {
		t.Fatalf("configuredOrigin: %v", err)
	}
	if client.BaseURL != "https://kai.example" || client.Tenant != "acme" || client.Repo != "widgets" {
		t.Fatalf("client = %s %s/%s, want https://kai.example acme/widgets", client.BaseURL, client.Tenant, client.Repo)
	}
}

func TestPermanentSyncError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf(`sync subscribe failed: HTTP 404: {"error":"org not found"}`), true},
		{fmt.Errorf("sync subscribe failed: HTTP 401: unauthorized"), true},
		{fmt.Errorf("sync subscribe failed: HTTP 403: forbidden"), true},
		{fmt.Errorf("sync subscribe failed: HTTP 429: slow down"), false},
		{fmt.Errorf("sync subscribe failed: HTTP 408: timeout"), false},
		{fmt.Errorf("sync subscribe failed: HTTP 502: bad gateway"), false},
		{fmt.Errorf("dial tcp: connection refused"), false},
	} {
		if got := permanentSyncError(tc.err); got != tc.want {
			t.Errorf("permanentSyncError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
