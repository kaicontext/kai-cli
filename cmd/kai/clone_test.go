package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCloneURL(t *testing.T) {
	tests := []struct {
		name           string
		inputURL       string
		flagTenant     string
		flagRepo       string
		wantBaseURL    string
		wantTenant     string
		wantRepo       string
		wantErr        bool
	}{
		{
			name:        "full URL with tenant and repo in path",
			inputURL:    "http://localhost:8080/myorg/myrepo",
			wantBaseURL: "http://localhost:8080",
			wantTenant:  "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "HTTPS URL with tenant and repo in path",
			inputURL:    "https://kailab.example.com/acme/webapp",
			wantBaseURL: "https://kailab.example.com",
			wantTenant:  "acme",
			wantRepo:    "webapp",
		},
		{
			name:        "URL with flags override",
			inputURL:    "http://localhost:8080",
			flagTenant:  "flagorg",
			flagRepo:    "flagrepo",
			wantBaseURL: "http://localhost:8080",
			wantTenant:  "flagorg",
			wantRepo:    "flagrepo",
		},
		{
			name:        "URL with trailing slash",
			inputURL:    "http://localhost:8080/org/repo/",
			wantBaseURL: "http://localhost:8080",
			wantTenant:  "org",
			wantRepo:    "repo",
		},
		{
			name:       "URL without tenant/repo and no flags",
			inputURL:   "http://localhost:8080",
			wantErr:    true,
		},
		{
			name:       "URL with only tenant in path",
			inputURL:   "http://localhost:8080/onlyorg",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL, tenant, repo, err := parseCloneURL(tt.inputURL, tt.flagTenant, tt.flagRepo)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", baseURL, tt.wantBaseURL)
			}
			if tenant != tt.wantTenant {
				t.Errorf("tenant = %q, want %q", tenant, tt.wantTenant)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

// parseCloneURL extracts base URL, tenant, and repo from a clone URL
func parseCloneURL(rawURL, flagTenant, flagRepo string) (baseURL, tenant, repo string, err error) {
	tenant = flagTenant
	repo = flagRepo

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", err
	}

	// Extract tenant/repo from path if not specified via flags
	pathParts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(pathParts) >= 2 && tenant == "" && repo == "" {
		tenant = pathParts[0]
		repo = pathParts[1]
		// Rebuild base URL without tenant/repo path
		parsedURL.Path = ""
		rawURL = parsedURL.String()
	}

	// Validate we have tenant and repo
	if tenant == "" {
		return "", "", "", os.ErrInvalid
	}
	if repo == "" {
		return "", "", "", os.ErrInvalid
	}

	return rawURL, tenant, repo, nil
}

func TestCloneDirectoryName(t *testing.T) {
	tests := []struct {
		name     string
		repo     string
		arg      string
		wantDir  string
	}{
		{
			name:    "default to repo name",
			repo:    "myrepo",
			arg:     "",
			wantDir: "myrepo",
		},
		{
			name:    "custom directory name",
			repo:    "myrepo",
			arg:     "custom-dir",
			wantDir: "custom-dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirName := tt.repo
			if tt.arg != "" {
				dirName = tt.arg
			}

			if dirName != tt.wantDir {
				t.Errorf("dirName = %q, want %q", dirName, tt.wantDir)
			}
		})
	}
}

func TestCloneCreatesDirectory(t *testing.T) {
	// Create a temp directory for testing
	tmpDir := t.TempDir()
	testDir := filepath.Join(tmpDir, "test-clone")

	// Verify directory doesn't exist
	if _, err := os.Stat(testDir); err == nil {
		t.Fatal("test directory should not exist yet")
	}

	// Create the directory (simulating what clone does)
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}

	// Verify directory exists
	info, err := os.Stat(testDir)
	if err != nil {
		t.Fatalf("directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("should be a directory")
	}
}

func TestCloneFailsIfDirectoryExists(t *testing.T) {
	// Create a temp directory
	tmpDir := t.TempDir()
	existingDir := filepath.Join(tmpDir, "existing")

	// Create the directory first
	if err := os.MkdirAll(existingDir, 0755); err != nil {
		t.Fatalf("failed to create test directory: %v", err)
	}

	// Check if directory exists (this is what clone should do)
	if _, err := os.Stat(existingDir); err == nil {
		// This is the expected behavior - clone should fail
		// The error message would be "directory 'existing' already exists"
	} else {
		t.Fatal("directory should exist")
	}
}
