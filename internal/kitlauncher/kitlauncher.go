// Package kitlauncher resolves, self-installs, and hands off to the
// managed `kit` TUI binary that backs `kai code`.
//
// This is the Go re-implementation of install.sh's install_kit() flow,
// with one deliberate inversion: install_kit swallows every failure
// (curl/wget/gunzip/chmod all `return 0`) because it must never block
// `kai`'s install over a kit hiccup. Here the contract is the opposite —
// `kai code` exists *to* run kit, so every failure path returns an error
// that surfaces to the user. Nothing is silent.
//
// Every interaction with the outside world (network, filesystem, process
// exec, host platform) is a seam on Launcher so the whole flow is
// testable without real downloads or a real process replacement. See
// kitlauncher_test.go.
package kitlauncher

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Sentinel errors classify resolveKitPath / install outcomes so callers
// (and tests) can branch on cause rather than string-matching.
var (
	// ErrKitNotFound means no kit binary was found in the managed dir or
	// on PATH. This is the signal that triggers a self-install — it is a
	// typed not-found, never an empty path that would exec nothing.
	ErrKitNotFound = errors.New("kit binary not found")

	// ErrKitNotExecutable means a kit binary exists in the managed dir but
	// is missing its executable bit. Surfaced as "needs repair" rather than
	// silently skipped or blindly reinstalled.
	ErrKitNotExecutable = errors.New("kit binary found but not executable")

	// ErrUnsupportedPlatform means kit is not published for this GOOS/GOARCH.
	// Returned before any network call so an unsupported platform is an
	// explicit error, not a swallowed 404.
	ErrUnsupportedPlatform = errors.New("kit is not available for this platform")
)

// kitBinaryName is the on-disk name of the managed binary (~/.kai/bin/kit).
// install.sh installs the gunzipped asset under this name.
const kitBinaryName = "kit"

// Launcher resolves, installs, and hands off to the kit binary. Construct
// the production instance with Default(); tests construct one directly and
// override the seams.
type Launcher struct {
	// BaseURL is the download root, with a trailing slash. The asset name
	// (kit-{os}-{arch}.gz) is appended to it.
	BaseURL string
	// Client performs the download. A non-nil timeout is what turns a hung
	// connection into a surfaced error rather than a silent hang.
	Client *http.Client
	// BinDir is the managed bin directory (default ~/.kai/bin). The kit
	// binary is resolved here first and installed here.
	BinDir string
	// GOOS / GOARCH select the download asset. Default to the host's, but
	// overridable so platform-mapping can be tested without cross-building.
	GOOS   string
	GOARCH string
	// Exec replaces the current process with kit. Defaults to syscall.Exec;
	// it returns only on failure (a successful exec never returns).
	Exec func(argv0 string, argv []string, env []string) error
	// BeforeExec, if set, runs immediately before the handoff replaces this
	// process — the last chance to flush state (e.g. telemetry) that the
	// replacement would otherwise drop. No-op when nil.
	BeforeExec func()
	// Codesign ad-hoc signs a freshly downloaded binary so macOS Gatekeeper
	// doesn't block it. Default is a no-op off darwin.
	Codesign func(path string) error
	// LookPath resolves a binary on PATH. Defaults to exec.LookPath.
	LookPath func(file string) (string, error)
	// Environ supplies the environment passed to kit. Defaults to os.Environ.
	Environ func() []string

	// Stderr receives the launcher's own progress lines (install/download
	// status). kit inherits the real stdio directly through the syscall.Exec
	// handoff, so the launcher doesn't plumb Stdin/Stdout itself.
	Stderr io.Writer
}

// Default returns a Launcher wired to the real download endpoint,
// filesystem, host platform, and syscall.Exec handoff.
func Default() *Launcher {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// No resolvable home (some CI/cron/restricted accounts). Fall back to
		// an absolute temp dir so BinDir is never the relative ".kai/bin" —
		// which would download kit into the cwd, litter project trees, and
		// re-resolve to a different place on every run from a new directory.
		home = os.TempDir()
	}
	// Mirror install.sh's `${KAI_INSTALL_DIR:-$HOME/.kai/bin}` exactly, so a
	// user who relocated the install (e.g. KAI_INSTALL_DIR=/usr/local/bin) is
	// resolved there instead of having a second, divergent kit silently
	// downloaded to ~/.kai/bin and shadowing it.
	binDir := os.Getenv("KAI_INSTALL_DIR")
	if binDir == "" {
		binDir = filepath.Join(home, ".kai", "bin")
	}
	return &Launcher{
		BaseURL:  "https://app.kaicontext.com/dl/",
		Client:   &http.Client{Timeout: 60 * time.Second},
		BinDir:   binDir,
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Exec:     syscall.Exec,
		Codesign: adhocCodesign,
		LookPath: exec.LookPath,
		Environ:  os.Environ,
		Stderr:   os.Stderr,
	}
}

// Refresh re-downloads and installs the latest kit binary unconditionally,
// replacing whatever managed copy exists. Called by `kai update` so kit stays
// current alongside kai. Returns the installed path on success.
func (l *Launcher) Refresh(ctx context.Context) (string, error) {
	return l.install(ctx)
}

// Run resolves kit (installing it on first use), then hands off to it with
// args forwarded verbatim by replacing this process via syscall.Exec. On a
// successful handoff this never returns. On failure it returns a non-zero
// code and a non-nil error describing it — never (0, nil), so no failure is
// silent.
func (l *Launcher) Run(ctx context.Context, args []string) (int, error) {
	path, err := l.resolveKitPath()
	if err != nil {
		if !errors.Is(err, ErrKitNotFound) {
			// Not-executable / directory / other classification problem.
			// Surface it; don't blindly reinstall over a possibly-fine file.
			return 1, err
		}
		fmt.Fprintln(l.Stderr, "kai code: kit not found — downloading it now (one-time)…")
		path, err = l.install(ctx)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(l.Stderr, "kai code: installed kit to %s\n", path)
	}

	// Hand off: replace this process with kit. argv[0] is the binary path;
	// the user's trailing args follow verbatim. On a successful exec this
	// never returns, so run any BeforeExec flush first.
	argv := append([]string{path}, args...)
	if l.BeforeExec != nil {
		l.BeforeExec()
	}
	if err := l.Exec(path, argv, l.Environ()); err != nil {
		return 1, fmt.Errorf("launching kit (%s): %w", path, err)
	}
	// Unreachable on a successful exec — the process has been replaced.
	return 0, nil
}

// resolveKitPath finds the kit binary. Precedence: the managed
// ~/.kai/bin/kit first (kai owns it there), then PATH. A managed binary
// that exists but isn't executable is reported as ErrKitNotExecutable
// ("needs repair"), never silently skipped. A total miss is the typed
// ErrKitNotFound with an empty path, which is the install trigger.
func (l *Launcher) resolveKitPath() (string, error) {
	managed := filepath.Join(l.BinDir, kitBinaryName)
	if info, err := os.Stat(managed); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory, expected the kit binary", managed)
		}
		// Check the owner execute bit specifically: kai owns this managed
		// binary, so it runs it as the owner. A file executable only by
		// group/other (e.g. mode 0070 from an odd umask) would pass an
		// any-bit (&0o111) test yet still fail at syscall.Exec with a cryptic
		// EACCES — route it to the actionable repair message instead.
		if info.Mode().Perm()&0o100 == 0 {
			return managed, fmt.Errorf("%w: %s (run `chmod +x %s` or reinstall)", ErrKitNotExecutable, managed, managed)
		}
		return managed, nil
	}
	if p, err := l.LookPath(kitBinaryName); err == nil {
		return p, nil
	}
	return "", ErrKitNotFound
}

// assetName maps GOOS/GOARCH to the published asset name. An unsupported
// platform is an explicit error returned *before* any network call, so it
// can never be confused with a 404 for a supported platform.
func (l *Launcher) assetName() (string, error) {
	switch l.GOOS {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, l.GOOS, l.GOARCH)
	}
	switch l.GOARCH {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("%w: %s/%s", ErrUnsupportedPlatform, l.GOOS, l.GOARCH)
	}
	return fmt.Sprintf("kit-%s-%s.gz", l.GOOS, l.GOARCH), nil
}

// install downloads, decompresses, validates, signs, and atomically
// installs kit into BinDir, returning the installed path. Mirrors
// runUpdate's download→gunzip→chmod→rename shape (main.go) but every
// failure returns an error — none is swallowed — and the install is
// atomic: a mid-failure can never clobber an existing good kit.
func (l *Launcher) install(ctx context.Context) (string, error) {
	asset, err := l.assetName()
	if err != nil {
		return "", err
	}
	url := l.BaseURL + asset

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading kit from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading kit from %s: server returned %s", url, resp.Status)
	}

	if err := os.MkdirAll(l.BinDir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", l.BinDir, err)
	}

	// Download into a temp file in the SAME directory so the final rename
	// is atomic (one filesystem) and never partially overwrites an existing
	// kit. The temp file is removed on any failure before the rename.
	tmp, err := os.CreateTemp(l.BinDir, ".kit-download-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file in %s: %w", l.BinDir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("decompressing kit download: %w", err)
	}
	n, err := io.Copy(tmp, gz)
	gz.Close()
	if err != nil {
		// Truncated / corrupt gzip stream lands here as unexpected EOF.
		return "", fmt.Errorf("writing kit binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("flushing kit binary: %w", err)
	}
	// A zero-byte (or empty-gzip) download must never be chmod-+x'd and
	// left behind as a broken binary.
	if n == 0 {
		return "", fmt.Errorf("downloaded kit is empty (0 bytes) — refusing to install a broken binary")
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return "", fmt.Errorf("making kit executable: %w", err)
	}
	if err := l.Codesign(tmpPath); err != nil {
		return "", fmt.Errorf("codesigning kit: %w", err)
	}

	dest := filepath.Join(l.BinDir, kitBinaryName)
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", fmt.Errorf("installing kit to %s: %w", dest, err)
	}
	committed = true
	return dest, nil
}

// adhocCodesign ad-hoc signs a freshly downloaded binary on macOS so
// Gatekeeper doesn't refuse to run it. No-op on other platforms. A
// signing failure is returned, not swallowed — an unsigned binary that
// Gatekeeper later blocks would otherwise be an invisible failure.
func adhocCodesign(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--sign", "-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ad-hoc codesign of %s failed: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
