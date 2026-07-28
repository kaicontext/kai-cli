package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kaicontext/kai-engine/remote"
)

// TestParsePRRef covers both accepted --pr forms: a bare PR number, and a
// full GitHub PR URL (which also yields github_repository — from the URL,
// never from --repo or the origin remote). Also covers rejected input.
func TestParsePRRef(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantNumber  int
		wantGHRepo  string
		wantErr     bool
		errContains string
	}{
		{name: "bare number", in: "123", wantNumber: 123, wantGHRepo: ""},
		{name: "bare number with whitespace", in: "  42  ", wantNumber: 42, wantGHRepo: ""},
		{
			name: "full github URL", in: "https://github.com/kaicontext/kai-server/pull/123",
			wantNumber: 123, wantGHRepo: "kaicontext/kai-server",
		},
		{
			name: "http (not https) URL", in: "http://github.com/kaicontext/kai-server/pull/7",
			wantNumber: 7, wantGHRepo: "kaicontext/kai-server",
		},
		{
			name: "URL with trailing slash", in: "https://github.com/kai/kai-cli/pull/9/",
			wantNumber: 9, wantGHRepo: "kai/kai-cli",
		},
		{name: "empty", in: "", wantErr: true, errContains: "required"},
		{name: "not a number or URL", in: "abc", wantErr: true, errContains: "--pr must be"},
		{name: "zero", in: "0", wantErr: true, errContains: "positive"},
		{name: "negative", in: "-1", wantErr: true, errContains: "positive"},
		{name: "non-github URL", in: "https://gitlab.com/org/repo/pull/1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, gh, err := parsePRRef(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePRRef(%q): want error, got n=%d gh=%q", tt.in, n, gh)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("parsePRRef(%q) error = %q, want it to contain %q", tt.in, err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePRRef(%q): unexpected error: %v", tt.in, err)
			}
			if n != tt.wantNumber {
				t.Errorf("parsePRRef(%q) number = %d, want %d", tt.in, n, tt.wantNumber)
			}
			if gh != tt.wantGHRepo {
				t.Errorf("parsePRRef(%q) github_repository = %q, want %q", tt.in, gh, tt.wantGHRepo)
			}
		})
	}
}

// TestFindingsAPIPost_ErrorMapping is table-driven over every status
// findingsAPIPost special-cases: 401 tells the caller to `kai login`, 403 is a
// clear permission message, 422 surfaces the server's "no review workflow"
// guidance VERBATIM (not wrapped/reworded), 409 likewise verbatim, and any
// other failure status is a non-nil, non-empty error too — the CLI must not
// inherit the pipeline's log-and-continue habit; every failure path here
// yields the non-nil error that becomes `kai`'s non-zero exit + stderr.
func TestFindingsAPIPost_ErrorMapping(t *testing.T) {
	const workflowGuidance = "no review workflow matched this repo: expected a workflow with an `on: review` trigger; retry with \"allow_fallback\": true"
	const conflictGuidance = "a review run is already queued or in progress for this commit"

	tests := []struct {
		name           string
		status         int
		body           string
		wantErrPart    string
		wantStatusEcho int
	}{
		{
			name: "401 unauthorized", status: http.StatusUnauthorized, body: `{"error":"missing authorization"}`,
			wantErrPart: "kai login",
		},
		{
			name: "403 forbidden", status: http.StatusForbidden, body: `{"error":"insufficient permissions"}`,
			wantErrPart: "permission",
		},
		{
			name: "404 not found", status: http.StatusNotFound, body: `{"error":"repo not found"}`,
			wantErrPart: "not found",
		},
		{
			name: "409 conflict verbatim", status: http.StatusConflict,
			body:        fmt.Sprintf(`{"error":%q,"run_id":"run-1"}`, conflictGuidance),
			wantErrPart: conflictGuidance,
		},
		{
			name: "422 unprocessable verbatim", status: http.StatusUnprocessableEntity,
			body:        fmt.Sprintf(`{"error":%q}`, workflowGuidance),
			wantErrPart: workflowGuidance,
		},
		{
			name: "500 server error", status: http.StatusInternalServerError, body: `{"error":"boom"}`,
			wantErrPart: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			body, status, err := findingsAPIPost(srv.URL, "tok", "/api/v1/orgs/o/repos/r/reviews", map[string]interface{}{"pr_number": 1})
			if status != tt.status {
				t.Errorf("status = %d, want %d", status, tt.status)
			}
			if err == nil {
				t.Fatalf("expected a non-nil error for status %d", tt.status)
			}
			if err.Error() == "" {
				t.Fatalf("error message must be non-empty (this is what becomes CLI stderr)")
			}
			if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrPart)
			}
			if len(body) == 0 {
				t.Errorf("findingsAPIPost should still return the raw body alongside the error")
			}
		})
	}
}

// withFakeLogin isolates HOME to a temp dir and writes a never-expiring
// credentials file, so resolveFindingsTarget()'s remote.GetValidAccessToken()
// succeeds without touching the real user's ~/.kai or any network.
func withFakeLogin(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kai"), 0o700); err != nil {
		t.Fatalf("mkdir .kai: %v", err)
	}
	if err := remote.SaveCredentials(&remote.Credentials{AccessToken: "test-token", ExpiresAt: 0}); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
}

// resetFindingsRunFlags restores the package-level flag vars findingsRunCmd
// reads, so tests that set them directly (bypassing cobra flag parsing) don't
// leak state into other tests.
func resetFindingsRunFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		findingsRepo = ""
		findingsRunPR = ""
		findingsRunEvent = ""
		findingsRunAllowFallback = false
		findingsRunWait = false
	})
}

// TestFindingsRunCmd_RunE is table-driven end-to-end coverage of `kai findings
// run`: it drives the real RunE against a fake control-plane server and a
// faked login, and asserts RunE's error return — which is exactly what
// determines kai's exit code and stderr (see main()) — for every case,
// including the non-error 409 (already-running) case.
func TestFindingsRunCmd_RunE(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		errPart    string
		reqChecker func(t *testing.T, body map[string]interface{})
	}{
		{
			name:   "success 201",
			status: http.StatusCreated,
			body:   `{"run_ids":["run-1"],"run_id":"run-1","review_id":"pr-1","status":"queued"}`,
		},
		{
			name:    "401 unauthorized is a failure",
			status:  http.StatusUnauthorized,
			body:    `{"error":"missing authorization"}`,
			wantErr: true,
			errPart: "kai login",
		},
		{
			name:    "403 forbidden is a failure",
			status:  http.StatusForbidden,
			body:    `{"error":"insufficient permissions"}`,
			wantErr: true,
			errPart: "permission",
		},
		{
			name:    "422 surfaces guidance verbatim",
			status:  http.StatusUnprocessableEntity,
			body:    `{"error":"no review workflow matched this repo: expected a workflow with an on: review trigger"}`,
			wantErr: true,
			errPart: "no review workflow matched this repo",
		},
		{
			name:    "409 duplicate is NOT an error (already running)",
			status:  http.StatusConflict,
			body:    `{"error":"a review run is already queued or in progress for this commit","run_id":"run-existing","review_id":"pr-1"}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeLogin(t)
			resetFindingsRunFlags(t)

			var gotBody map[string]interface{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews") {
					http.NotFound(w, r)
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			t.Setenv("KAI_SERVER", srv.URL)
			findingsRepo = "myorg/myrepo"
			findingsRunPR = "https://github.com/kaicontext/kai-server/pull/55"

			err := findingsRunCmd.RunE(findingsRunCmd, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				if err.Error() == "" {
					t.Fatalf("error message must be non-empty (becomes CLI stderr)")
				}
				if tt.errPart != "" && !strings.Contains(err.Error(), tt.errPart) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errPart)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotBody == nil {
				t.Fatal("server never received the trigger request")
			}
			if got, want := gotBody["pr_number"], float64(55); got != want {
				t.Errorf("request pr_number = %v, want %v", got, want)
			}
			if got, want := gotBody["github_repository"], "kaicontext/kai-server"; got != want {
				t.Errorf("request github_repository = %v, want %v — must come from the --pr URL, not --repo/origin", got, want)
			}
		})
	}
}

// TestFindingsRunCmd_RunE_WaitReturnsRunFailure covers --wait: when the run
// ultimately fails, RunE must return a non-nil error even though the *trigger*
// itself (the 201) succeeded — the CLI shouldn't report success for a review
// that actually failed.
func TestFindingsRunCmd_RunE_WaitReturnsRunFailure(t *testing.T) {
	withFakeLogin(t)
	resetFindingsRunFlags(t)
	origInterval, origTimeout := findingsRunPollInterval, findingsRunWaitTimeout
	findingsRunPollInterval = time.Millisecond
	findingsRunWaitTimeout = time.Second
	t.Cleanup(func() {
		findingsRunPollInterval, findingsRunWaitTimeout = origInterval, origTimeout
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run_ids":["run-9"],"run_id":"run-9","review_id":"pr-9","status":"queued"}`))
		case strings.Contains(r.URL.Path, "/runs/run-9"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"run-9","status":"completed","conclusion":"failure"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("KAI_SERVER", srv.URL)
	findingsRepo = "myorg/myrepo"
	findingsRunPR = "77"
	findingsRunWait = true

	err := findingsRunCmd.RunE(findingsRunCmd, nil)
	if err == nil {
		t.Fatal("expected an error: the waited-for run concluded failure")
	}
	if !strings.Contains(err.Error(), "failure") {
		t.Errorf("error = %q, want it to mention the failure conclusion", err.Error())
	}
}
