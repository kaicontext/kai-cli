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

	// ErrKitStale means a managed kit binary exists and is executable, but its
	// recorded version (the kit.version sidecar) does not match the version
	// this kai is pinned to (Launcher.ExpectedVersion). It is the trigger to
	// re-install the pinned build over the old one — distinct from
	// ErrKitNotFound (nothing installed) so the launcher can say "updating"
	// rather than "downloading (one-time)". Only ever returned for the managed
	// binary; a kit found on PATH is the user's to version.
	ErrKitStale = errors.New("managed kit binary is a different version than this kai pins")
)

// kitBinaryName is the on-disk name of the managed binary (~/.kai/bin/kit).
// install.sh installs the gunzipped asset under this name.
const kitBinaryName = "kit"

// versionFileName is the sidecar next to the managed binary
// (~/.kai/bin/kit.version) recording which kit version is installed there.
// Written at install time = the version we downloaded; read on resolve to
// detect a stale pin. Deliberately NOT prefixed ".kit" so it isn't mistaken
// for a leftover ".kit-download-*" temp file. Absent (e.g. a kit placed by
// the old install.sh, which wrote no sidecar) reads as "" → treated as stale
// under a pin, so the first pinned `kai code` upgrades it once.
const versionFileName = kitBinaryName + ".version"

// PinnedKitVersion is the kit build this kai is pinned to. EMPTY = unpinned:
// `kai code` fetches the rolling-latest asset (kit-{os}-{arch}.gz) and never
// version-checks — the pre-pin behavior, byte-for-byte. Set it to a published
// kit version (e.g. "0.34.0") to make `kai code` fetch and stay on exactly
// that build, re-installing whenever the managed copy drifts.
//
// ACTIVATION (do all three together, or leave empty):
//  1. The /dl endpoint must serve version-qualified assets:
//     app.kaicontext.com/dl/kit-{os}-{arch}-{version}.gz (kailab-control maps
//     the version to that tag's kai-tui release asset). Today it serves only
//     the unversioned rolling-latest.
//  2. A kit release for this version must exist (kai-tui cuts one on a v* tag).
//  3. Bump this constant, ideally in lockstep with each kai release so kit and
//     kai move together.
//
// Until (1) and (2) hold, leaving this empty is correct — a non-empty pin here
// would make `kai code` request an asset the proxy can't serve yet (404).
const PinnedKitVersion = ""

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
	// ExpectedVersion pins the managed kit to a specific build. When
	// non-empty the launcher (a) downloads the version-qualified asset
	// (kit-{os}-{arch}-{ver}.gz) and (b) re-installs whenever the managed
	// copy's recorded version differs — so `kai code` always runs this exact
	// kit. Empty disables pinning entirely (rolling-latest, no version check),
	// which is the default via PinnedKitVersion. Never applied to a kit found
	// on PATH — only the managed binary kai owns.
	ExpectedVersion string
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
		BaseURL:         "https://app.kaicontext.com/dl/",
		Client:          &http.Client{Timeout: 60 * time.Second},
		BinDir:          binDir,
		ExpectedVersion: PinnedKitVersion,
		GOOS:            runtime.GOOS,
		GOARCH:          runtime.GOARCH,
		Exec:            syscall.Exec,
		Codesign:        adhocCodesign,
		LookPath:        exec.LookPath,
		Environ:         os.Environ,
		Stderr:          os.Stderr,
	}
}

// Run resolves kit (installing it on first use), then hands off to it with
// args forwarded verbatim by replacing this process via syscall.Exec. On a
// successful handoff this never returns. On failure it returns a non-zero
// code and a non-nil error describing it — never (0, nil), so no failure is
// silent.
func (l *Launcher) Run(ctx context.Context, args []string) (int, error) {
	path, err := l.resolveKitPath()
	if err != nil {
		switch {
		case errors.Is(err, ErrKitNotFound):
			fmt.Fprintln(l.Stderr, "kai code: kit not found — downloading it now (one-time)…")
		case errors.Is(err, ErrKitStale):
			// A managed kit is present but pinned to a different version —
			// re-install the pinned build over it (atomic; never clobbers on
			// failure). This is the "always run the right kit" path.
			fmt.Fprintf(l.Stderr, "kai code: updating kit to %s…\n", l.ExpectedVersion)
		default:
			// Not-executable / directory / other classification problem.
			// Surface it; don't blindly reinstall over a possibly-fine file.
			return 1, err
		}
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
		// Pin check (only when this kai pins a version): if the managed copy's
		// recorded version differs from the pin, report it stale so Run
		// re-installs the pinned build. Reported only for the managed binary;
		// a kit on PATH below is the user's own and is never second-guessed.
		if l.ExpectedVersion != "" && l.installedVersion() != l.ExpectedVersion {
			return managed, ErrKitStale
		}
		return managed, nil
	}
	if p, err := l.LookPath(kitBinaryName); err == nil {
		return p, nil
	}
	return "", ErrKitNotFound
}

// installedVersion reads the kit.version sidecar next to the managed binary.
// Returns "" when absent or unreadable — which, under a pin, reads as stale
// and triggers a one-time upgrade (e.g. a kit placed by the old install.sh
// that wrote no sidecar).
func (l *Launcher) installedVersion() string {
	b, err := os.ReadFile(filepath.Join(l.BinDir, versionFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// assetName maps GOOS/GOARCH (and the pin, if any) to the published asset
// name. An unsupported platform is an explicit error returned *before* any
// network call, so it can never be confused with a 404 for a supported
// platform. When ExpectedVersion is set, the version-qualified asset
// (kit-{os}-{arch}-{ver}.gz) is requested so the download is exactly the
// pinned build; otherwise the unversioned rolling-latest asset.
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
	if l.ExpectedVersion != "" {
		return fmt.Sprintf("kit-%s-%s-%s.gz", l.GOOS, l.GOARCH, l.ExpectedVersion), nil
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

	// Record which version we just installed so the next resolve can detect a
	// stale pin. Only under a pin (unpinned installs track rolling-latest and
	// have no version to record). Best-effort: the kit binary is already in
	// place and runnable, so a sidecar-write hiccup must not fail the install —
	// it would just cause one redundant re-download next launch. We surface a
	// warning rather than swallow it, consistent with the package's no-silent
	// rule, but still return success.
	if l.ExpectedVersion != "" {
		vf := filepath.Join(l.BinDir, versionFileName)
		if err := os.WriteFile(vf, []byte(l.ExpectedVersion+"\n"), 0o644); err != nil {
			fmt.Fprintf(l.Stderr, "kai code: warning: recording kit version failed (%v); kit installed but may re-download next run\n", err)
		}
	}
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
