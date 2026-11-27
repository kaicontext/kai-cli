package cirun

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ActVersion is the nektos/act release kai ci run is tested against. Bump it
// together with actSHA256.
const ActVersion = "0.2.98"

// actSHA256 pins the release archives of ActVersion, keyed by asset name.
// The values are the digests GitHub publishes for the v0.2.98 release assets.
var actSHA256 = map[string]string{
	"act_Darwin_arm64.tar.gz":  "14477d7e076b12a885c54301beff2b524a905e4238a2b3ec9b3d1fd0efdc100f",
	"act_Darwin_x86_64.tar.gz": "a0885179705d0f3f5d27201cefa0309ac78e3fccda447ecdfb96877b66b565b0",
	"act_Linux_arm64.tar.gz":   "a30ba1a1582cb93810c77f743415cac33c16f60fc288116a8e0943144b6a6a4f",
	"act_Linux_x86_64.tar.gz":  "d3344bd0810c04b000d2444030b08c90ab9fbf905bbe2c3e26298be00c620d65",
	"act_Windows_arm64.zip":    "c67cd28ccd1c9a9238614306a8f49c2037b7df7aa01b5d890ad671d43cbdc503",
	"act_Windows_x86_64.zip":   "d0d868ee2b33bb710f4df82d69345ce3a10d2f9fc7bbee9522bf36a15779ada0",
}

// actBaseURL is where release archives are fetched from; tests point it at a
// local server.
var actBaseURL = "https://github.com/nektos/act/releases/download"

const actMaxArchive = 128 << 20

// ActTool is a resolved act binary.
type ActTool struct {
	Path    string
	Version string // "" when it couldn't be read
	Source  string // "flag", "path" or "managed"
	Warning string // set when Version differs from ActVersion
}

func actAsset(goos, goarch string) (string, error) {
	osName := map[string]string{"darwin": "Darwin", "linux": "Linux", "windows": "Windows"}[goos]
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	name := "act_" + osName + "_" + arch + ext
	if osName == "" || arch == "" || actSHA256[name] == "" {
		return "", fmt.Errorf("kai has no pinned act build for %s/%s; install act %s yourself and pass --act-path", goos, goarch, ActVersion)
	}
	return name, nil
}

func actBinName() string {
	if runtime.GOOS == "windows" {
		return "act.exe"
	}
	return "act"
}

// ManagedActPath is where kai keeps its own copy of act.
func ManagedActPath(kaiHome string) string {
	return filepath.Join(kaiHome, "tools", "act", ActVersion, actBinName())
}

// ResolveAct finds the act binary to use: --act-path, then an act on PATH,
// then kai's managed copy, downloading it on first use. notify is told before
// a download starts.
func ResolveAct(ctx context.Context, actPath, kaiHome string, notify func(string)) (*ActTool, error) {
	tool := &ActTool{}
	switch {
	case actPath != "":
		// act is started from another directory, so a relative path has to
		// be pinned to the file that was checked here.
		abs, err := filepath.Abs(actPath)
		if err != nil {
			return nil, fmt.Errorf("--act-path: %w", err)
		}
		if fi, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("--act-path: %w", err)
		} else if fi.IsDir() {
			return nil, fmt.Errorf("--act-path: %s is a directory", abs)
		}
		tool.Path, tool.Source = abs, "flag"
	default:
		if p, err := exec.LookPath("act"); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			tool.Path, tool.Source = p, "path"
			break
		}
		p, err := ensureManagedAct(ctx, kaiHome, notify)
		if err != nil {
			return nil, err
		}
		tool.Path, tool.Source = p, "managed"
	}
	tool.Version = actVersion(ctx, tool.Path)
	if tool.Source != "managed" && tool.Version != ActVersion {
		got := tool.Version
		if got == "" {
			got = "an unknown version"
		}
		tool.Warning = fmt.Sprintf("using act %s at %s; kai is tested against %s", got, tool.Path, ActVersion)
	}
	return tool, nil
}

// ResolveActManaged returns kai's own copy of act, downloading it if it
// isn't there yet, without looking at PATH.
func ResolveActManaged(ctx context.Context, kaiHome string) (*ActTool, error) {
	p, err := ensureManagedAct(ctx, kaiHome, nil)
	if err != nil {
		return nil, err
	}
	return &ActTool{Path: p, Source: "managed", Version: actVersion(ctx, p)}, nil
}

func ensureManagedAct(ctx context.Context, kaiHome string, notify func(string)) (string, error) {
	p := ManagedActPath(kaiHome)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if notify != nil {
		notify(fmt.Sprintf("Downloading act %s to %s", ActVersion, filepath.Dir(p)))
	}
	if err := downloadAct(ctx, p); err != nil {
		return "", fmt.Errorf("downloading act %s: %w", ActVersion, err)
	}
	return p, nil
}

var actVersionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

func actVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, "--version")
	c.WaitDelay = childWaitDelay
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return actVersionRE.FindString(string(out))
}

// downloadAct fetches the pinned archive, checks it against actSHA256, and
// installs the binary at dest. Nothing is left at dest unless the digest
// matched.
func downloadAct(ctx context.Context, dest string) error {
	asset, err := actAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	url := fmt.Sprintf("%s/v%s/%s", actBaseURL, ActVersion, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, actMaxArchive+1))
	if err != nil {
		return err
	}
	if len(archive) > actMaxArchive {
		return fmt.Errorf("%s is larger than %d bytes", asset, actMaxArchive)
	}
	sum := sha256.Sum256(archive)
	if got, want := hex.EncodeToString(sum[:]), actSHA256[asset]; got != want {
		return fmt.Errorf("%s: sha256 is %s, expected %s; refusing to install it", asset, got, want)
	}
	bin, err := extractAct(asset, archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".act-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func extractAct(asset string, archive []byte) ([]byte, error) {
	want := "act"
	if strings.HasSuffix(asset, ".zip") {
		want = "act.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, actMaxArchive))
		}
		return nil, fmt.Errorf("%s has no %s in it", asset, want)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s in it", asset, want)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(io.LimitReader(tr, actMaxArchive))
		}
	}
}
