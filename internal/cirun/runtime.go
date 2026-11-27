package cirun

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ContainerRuntime is the Docker-compatible daemon act will talk to.
type ContainerRuntime struct {
	Name string // "Docker", "OrbStack", "Colima", "Podman", or "DOCKER_HOST"
	Host string // value for DOCKER_HOST; "" means the client's default
}

// ErrNoRuntime is returned when no container runtime answers.
var ErrNoRuntime = errors.New("no container runtime found: start Docker Desktop, OrbStack, Colima or Podman, or set DOCKER_HOST")

// DetectRuntime finds a running container runtime: DOCKER_HOST if set, else
// the first default socket that accepts a connection.
func DetectRuntime() (*ContainerRuntime, error) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if p, ok := strings.CutPrefix(h, "unix://"); ok && !dialable(p) {
			return nil, errors.New("DOCKER_HOST is " + h + " but nothing is listening there")
		}
		return &ContainerRuntime{Name: "DOCKER_HOST", Host: h}, nil
	}
	if runtime.GOOS == "windows" {
		// Docker Desktop's named pipe; the Docker client finds it on its own.
		return &ContainerRuntime{Name: "Docker"}, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []struct{ name, path string }{
		{"Docker", "/var/run/docker.sock"},
		{"Docker", filepath.Join(home, ".docker", "run", "docker.sock")},
		{"OrbStack", filepath.Join(home, ".orbstack", "run", "docker.sock")},
		{"Colima", filepath.Join(home, ".colima", "default", "docker.sock")},
		{"Podman", filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock")},
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		candidates = append(candidates,
			struct{ name, path string }{"Docker", filepath.Join(x, "docker.sock")},
			struct{ name, path string }{"Podman", filepath.Join(x, "podman", "podman.sock")})
	}
	for i, c := range candidates {
		if dialable(c.path) {
			rt := &ContainerRuntime{Name: c.name}
			if i > 0 { // not the client's default path
				rt.Host = "unix://" + c.path
			}
			return rt, nil
		}
	}
	return nil, ErrNoRuntime
}

func dialable(sock string) bool {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
