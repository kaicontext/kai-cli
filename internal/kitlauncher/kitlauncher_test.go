package kitlauncher

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// --- helpers ---------------------------------------------------------------

// gzipBytes returns data gzip-compressed, the on-the-wire shape of a kit
// asset.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// testLauncher returns a Launcher wired for hermetic tests: a temp BinDir,
// a no-op codesign (the real one would reject our fake non-Mach-O bytes on
// macOS), a LookPath that finds nothing, and linux/amd64 so the asset name
// is deterministic.
func testLauncher(t *testing.T) *Launcher {
	t.Helper()
	return &Launcher{
		Client:   http.DefaultClient,
		BinDir:   t.TempDir(),
		GOOS:     "linux",
		GOARCH:   "amd64",
		Codesign: func(string) error { return nil },
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		Environ:  os.Environ,
		Stderr:   &bytes.Buffer{},
	}
}

// errRoundTripper makes every request fail, simulating a network error.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network failure")
}

// --- assetName -------------------------------------------------------------

func TestAssetName(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         string
		wantErr      bool
	}{
		{"linux", "amd64", "kit-linux-amd64.gz", false},
		{"linux", "arm64", "kit-linux-arm64.gz", false},
		{"darwin", "amd64", "kit-darwin-amd64.gz", false},
		{"darwin", "arm64", "kit-darwin-arm64.gz", false},
		{"windows", "amd64", "", true},
		{"linux", "386", "", true},
		{"plan9", "arm64", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.goarch, func(t *testing.T) {
			l := &Launcher{GOOS: tt.goos, GOARCH: tt.goarch}
			got, err := l.assetName()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s/%s, got %q", tt.goos, tt.goarch, got)
				}
				if !errors.Is(err, ErrUnsupportedPlatform) {
					t.Errorf("expected ErrUnsupportedPlatform, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("assetName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- resolveKitPath --------------------------------------------------------

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveKitPath_Managed(t *testing.T) {
	l := testLauncher(t)
	managed := filepath.Join(l.BinDir, "kit")
	writeExecutable(t, managed)

	got, err := l.resolveKitPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != managed {
		t.Errorf("got %q, want managed path %q", got, managed)
	}
}

func TestResolveKitPath_PathFallback(t *testing.T) {
	l := testLauncher(t) // empty BinDir
	pathKit := filepath.Join(t.TempDir(), "kit")
	writeExecutable(t, pathKit)
	l.LookPath = func(file string) (string, error) {
		if file == "kit" {
			return pathKit, nil
		}
		return "", errors.New("not found")
	}

	got, err := l.resolveKitPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != pathKit {
		t.Errorf("got %q, want PATH path %q", got, pathKit)
	}
}

func TestResolveKitPath_ManagedTakesPrecedenceOverPath(t *testing.T) {
	l := testLauncher(t)
	managed := filepath.Join(l.BinDir, "kit")
	writeExecutable(t, managed)
	// PATH also has one; managed must win.
	pathKit := filepath.Join(t.TempDir(), "kit")
	writeExecutable(t, pathKit)
	l.LookPath = func(string) (string, error) { return pathKit, nil }

	got, err := l.resolveKitPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != managed {
		t.Errorf("managed should take precedence: got %q, want %q", got, managed)
	}
}

func TestResolveKitPath_NotExecutable(t *testing.T) {
	l := testLauncher(t)
	managed := filepath.Join(l.BinDir, "kit")
	if err := os.WriteFile(managed, []byte("kit"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := l.resolveKitPath()
	if !errors.Is(err, ErrKitNotExecutable) {
		t.Fatalf("expected ErrKitNotExecutable, got %v", err)
	}
	if got != managed {
		t.Errorf("needs-repair path should still be reported: got %q, want %q", got, managed)
	}
}

func TestResolveKitPath_NotFound(t *testing.T) {
	l := testLauncher(t) // empty BinDir, LookPath finds nothing
	got, err := l.resolveKitPath()
	if !errors.Is(err, ErrKitNotFound) {
		t.Fatalf("expected ErrKitNotFound, got %v", err)
	}
	if got != "" {
		t.Errorf("not-found must return an empty path, got %q", got)
	}
}

// --- install ---------------------------------------------------------------

// serveGzip starts an httptest server that returns body for the asset path
// and 404 otherwise.
func serveGzip(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kit-linux-amd64.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstall_Success(t *testing.T) {
	want := []byte("\x7fELF fake kit binary contents")
	srv := serveGzip(t, gzipBytes(t, want))

	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	signed := false
	l.Codesign = func(string) error { signed = true; return nil }

	path, err := l.install(context.Background())
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if path != filepath.Join(l.BinDir, "kit") {
		t.Errorf("installed to %q, want %q", path, filepath.Join(l.BinDir, "kit"))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("installed content mismatch: got %q want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed kit is not executable: mode %v", info.Mode())
	}
	if !signed {
		t.Error("codesign step was not invoked")
	}
	// No leftover temp files in BinDir.
	assertNoTempLeftovers(t, l.BinDir)
}

func TestInstall_HTTP404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	_, err := l.install(context.Background())
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	assertNoKitInstalled(t, l.BinDir)
}

func TestInstall_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	if _, err := l.install(context.Background()); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	assertNoKitInstalled(t, l.BinDir)
}

func TestInstall_NetworkFailure(t *testing.T) {
	l := testLauncher(t)
	l.BaseURL = "http://example.invalid/"
	l.Client = &http.Client{Transport: errRoundTripper{}}
	_, err := l.install(context.Background())
	if err == nil {
		t.Fatal("expected error on network failure, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("downloading kit")) {
		t.Errorf("error should mention the download: %v", err)
	}
}

func TestInstall_CorruptGzip(t *testing.T) {
	srv := serveGzip(t, []byte("this is definitely not gzip data"))
	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	if _, err := l.install(context.Background()); err == nil {
		t.Fatal("expected error on corrupt gzip, got nil")
	}
	assertNoKitInstalled(t, l.BinDir)
}

func TestInstall_TruncatedGzip(t *testing.T) {
	full := gzipBytes(t, bytes.Repeat([]byte("kit binary payload "), 64))
	// Keep the gzip header but cut the stream short mid-body.
	truncated := full[:len(full)/2]
	srv := serveGzip(t, truncated)
	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	if _, err := l.install(context.Background()); err == nil {
		t.Fatal("expected error on truncated gzip, got nil")
	}
	assertNoKitInstalled(t, l.BinDir)
}

func TestInstall_ZeroByte(t *testing.T) {
	// A valid gzip stream that decompresses to nothing.
	srv := serveGzip(t, gzipBytes(t, []byte{}))
	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	_, err := l.install(context.Background())
	if err == nil {
		t.Fatal("expected error on zero-byte download, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("empty")) {
		t.Errorf("error should explain the binary is empty: %v", err)
	}
	// The broken (empty) binary must NOT have been installed.
	assertNoKitInstalled(t, l.BinDir)
}

func TestInstall_AtomicNoClobber(t *testing.T) {
	good := []byte("the existing good kit binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	dest := filepath.Join(l.BinDir, "kit")
	if err := os.WriteFile(dest, good, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := l.install(context.Background()); err == nil {
		t.Fatal("expected install to fail (500)")
	}
	// The existing good kit must be byte-for-byte intact.
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("existing kit was removed: %v", err)
	}
	if !bytes.Equal(got, good) {
		t.Errorf("existing kit was clobbered: got %q want %q", got, good)
	}
	assertNoTempLeftovers(t, l.BinDir)
}

func TestInstall_UnsupportedPlatformNoNetwork(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer srv.Close()

	l := testLauncher(t)
	l.GOOS = "windows"
	l.BaseURL = srv.URL + "/"
	_, err := l.install(context.Background())
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("expected ErrUnsupportedPlatform, got %v", err)
	}
	if hit {
		t.Error("unsupported platform must not make a network request")
	}
}

func TestInstall_CodesignFailureSurfaced(t *testing.T) {
	srv := serveGzip(t, gzipBytes(t, []byte("fake kit")))
	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"
	l.Codesign = func(string) error { return errors.New("gatekeeper would block this") }
	_, err := l.install(context.Background())
	if err == nil {
		t.Fatal("expected codesign failure to surface, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("codesigning")) {
		t.Errorf("error should mention codesigning: %v", err)
	}
	// A binary that failed signing must not be left installed.
	assertNoKitInstalled(t, l.BinDir)
}

// --- Run: handoff, forwarding, install-on-missing --------------------------

// execSpy records the argv/env it was handed and returns errToReturn.
type execSpy struct {
	argv0       string
	argv        []string
	env         []string
	called      bool
	errToReturn error
}

func (s *execSpy) fn(argv0 string, argv []string, env []string) error {
	s.called = true
	s.argv0 = argv0
	s.argv = argv
	s.env = env
	return s.errToReturn
}

func TestRun_ForwardsArgsVerbatim(t *testing.T) {
	l := testLauncher(t)
	managed := filepath.Join(l.BinDir, "kit")
	writeExecutable(t, managed)
	spy := &execSpy{}
	l.Exec = spy.fn

	args := []string{"--kitflag", "x", "--", "pos", "-z"}
	code, err := l.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if code != 0 {
		t.Errorf("expected 0 on successful handoff, got %d", code)
	}
	if !spy.called {
		t.Fatal("Exec was never called")
	}
	wantArgv := append([]string{managed}, args...)
	if fmt.Sprint(spy.argv) != fmt.Sprint(wantArgv) {
		t.Errorf("argv = %v, want %v (argv[0] must be the binary, rest verbatim)", spy.argv, wantArgv)
	}
	if spy.argv0 != managed {
		t.Errorf("argv0 = %q, want %q", spy.argv0, managed)
	}
}

func TestRun_ExecFailureSurfaced(t *testing.T) {
	l := testLauncher(t)
	writeExecutable(t, filepath.Join(l.BinDir, "kit"))
	l.Exec = (&execSpy{errToReturn: errors.New("exec: no such file")}).fn

	code, err := l.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when Exec fails, got nil")
	}
	if code == 0 {
		t.Error("a failed exec must yield a non-zero exit code")
	}
}

func TestRun_InstallsWhenMissing(t *testing.T) {
	srv := serveGzip(t, gzipBytes(t, []byte("#!/bin/sh\nexit 0\n")))
	l := testLauncher(t) // empty BinDir, LookPath finds nothing
	l.BaseURL = srv.URL + "/"
	spy := &execSpy{}
	l.Exec = spy.fn

	code, err := l.Run(context.Background(), []string{"--foo"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if code != 0 {
		t.Errorf("expected 0, got %d", code)
	}
	if !spy.called {
		t.Fatal("Exec was not called after install")
	}
	if spy.argv0 != filepath.Join(l.BinDir, "kit") {
		t.Errorf("handed off to %q, want freshly-installed %q", spy.argv0, filepath.Join(l.BinDir, "kit"))
	}
	if _, err := os.Stat(filepath.Join(l.BinDir, "kit")); err != nil {
		t.Errorf("kit was not installed: %v", err)
	}
}

func TestRun_NotExecutableSurfaced(t *testing.T) {
	l := testLauncher(t)
	// Present but not executable → needs repair, must not silently reinstall.
	if err := os.WriteFile(filepath.Join(l.BinDir, "kit"), []byte("kit"), 0o644); err != nil {
		t.Fatal(err)
	}
	installCalled := false
	l.Exec = func(string, []string, []string) error { installCalled = true; return nil }
	code, err := l.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected ErrKitNotExecutable to surface from Run")
	}
	if !errors.Is(err, ErrKitNotExecutable) {
		t.Errorf("expected ErrKitNotExecutable, got %v", err)
	}
	if code == 0 {
		t.Error("needs-repair must be a non-zero exit")
	}
	if installCalled {
		t.Error("must not hand off when the binary needs repair")
	}
}

// --- The non-silence guarantee --------------------------------------------

// TestRun_NoFailureIsSilent injects each failure mode and asserts BOTH a
// non-zero exit code AND a non-empty error message — the direct test of
// the "no silent errors" requirement.
func TestRun_NoFailureIsSilent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, l *Launcher)
	}{
		{
			name: "unsupported-platform",
			setup: func(t *testing.T, l *Launcher) {
				l.GOOS = "windows"
			},
		},
		{
			name: "http-404",
			setup: func(t *testing.T, l *Launcher) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.NotFound(w, r)
				}))
				t.Cleanup(srv.Close)
				l.BaseURL = srv.URL + "/"
			},
		},
		{
			name: "http-500",
			setup: func(t *testing.T, l *Launcher) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "boom", 500)
				}))
				t.Cleanup(srv.Close)
				l.BaseURL = srv.URL + "/"
			},
		},
		{
			name: "network-failure",
			setup: func(t *testing.T, l *Launcher) {
				l.Client = &http.Client{Transport: errRoundTripper{}}
				l.BaseURL = "http://example.invalid/"
			},
		},
		{
			name: "corrupt-gzip",
			setup: func(t *testing.T, l *Launcher) {
				srv := serveGzip(t, []byte("not gzip"))
				l.BaseURL = srv.URL + "/"
			},
		},
		{
			name: "zero-byte",
			setup: func(t *testing.T, l *Launcher) {
				srv := serveGzip(t, gzipBytes(t, []byte{}))
				l.BaseURL = srv.URL + "/"
			},
		},
		{
			name: "exec-failure",
			setup: func(t *testing.T, l *Launcher) {
				writeExecutable(t, filepath.Join(l.BinDir, "kit"))
				l.Exec = func(string, []string, []string) error { return errors.New("exec failed") }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLauncher(t)
			tc.setup(t, l)
			code, err := l.Run(context.Background(), nil)
			if code == 0 {
				t.Errorf("%s: exit code is 0 — a failure was reported as success", tc.name)
			}
			if err == nil || err.Error() == "" {
				t.Errorf("%s: error is empty — the failure is silent", tc.name)
			}
		})
	}
}

// --- Refresh ---------------------------------------------------------------

func TestRefresh_ReplacesExistingKit(t *testing.T) {
	old := []byte("old kit binary")
	new := []byte("new kit binary")
	srv := serveGzip(t, gzipBytes(t, new))

	l := testLauncher(t)
	l.BaseURL = srv.URL + "/"

	// Pre-install an existing kit so Refresh must overwrite it.
	dest := filepath.Join(l.BinDir, "kit")
	if err := os.WriteFile(dest, old, 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := l.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}
	if path != dest {
		t.Errorf("Refresh returned %q, want %q", path, dest)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, new) {
		t.Errorf("Refresh did not replace kit: got %q, want %q", got, new)
	}
}

func TestRefresh_PropagatesError(t *testing.T) {
	l := testLauncher(t)
	l.Client = &http.Client{Transport: errRoundTripper{}}
	l.BaseURL = "http://example.invalid/"

	_, err := l.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected error on network failure, got nil")
	}
}

// --- assertion helpers -----------------------------------------------------

func assertNoKitInstalled(t *testing.T, binDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(binDir, "kit")); err == nil {
		t.Error("a broken/failed download must not leave an installed kit")
	}
	assertNoTempLeftovers(t, binDir)
}

func assertNoTempLeftovers(t *testing.T, binDir string) {
	t.Helper()
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(e.Name()) >= 4 && e.Name()[:4] == ".kit" {
			t.Errorf("temp download file left behind: %s", e.Name())
		}
	}
}
