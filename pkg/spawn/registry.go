// Package spawn implements `kai spawn` / `kai despawn` / `kai spawn list`:
// orchestration over kai checkout + ws create + CoW copy + git init that
// stands up N disposable, sync-connected workspaces from one snapshot.
//
// The registry tracks spawned workspaces. Each spawned dir is its own
// independently-`kai init`'d repo, so there is no central `.kai/` that
// knows about its siblings — the registry at ~/.kai/spawned.json plays
// that role.
package spawn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Entry is one spawned workspace.
type Entry struct {
	Path           string `json:"path"`
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceName  string `json:"workspace_name"`
	Agent          string `json:"agent,omitempty"`
	SourceSnapshot string `json:"source_snapshot"`
	SourceRepo     string `json:"source_repo"`
	RepoChannel    string `json:"repo_channel,omitempty"`
	RemoteName     string `json:"remote_name,omitempty"`
	SyncMode       string `json:"sync_mode"`
	CopySource     string `json:"copy_source,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// Registry is the on-disk shape of ~/.kai/spawned.json.
type Registry struct {
	Spawned []Entry `json:"spawned"`
}

// RegistryPath returns the absolute path to the registry file
// ($HOME/.kai/spawned.json).
func RegistryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kai", "spawned.json"), nil
}

// withLock opens the registry file with an exclusive flock and runs fn.
// The file is created if missing. Lock is released when fn returns.
func withLock(fn func(path string) error) error {
	path, err := RegistryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating registry dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("opening registry: %w", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking registry: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn(path)
}

// Load returns the current registry contents. Empty registry if the
// file is missing or empty.
func Load() (*Registry, error) {
	path, err := RegistryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return &Registry{}, nil
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}
	return &r, nil
}

// save writes the registry atomically (write-tmp + rename) inside an
// already-held lock. Caller must hold the flock.
func save(path string, r *Registry) error {
	sort.Slice(r.Spawned, func(i, j int) bool {
		return r.Spawned[i].CreatedAt < r.Spawned[j].CreatedAt
	})
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Add appends entries under an exclusive lock.
func Add(entries ...Entry) error {
	return withLock(func(path string) error {
		r, err := loadFromPath(path)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		for _, e := range entries {
			if e.CreatedAt == "" {
				e.CreatedAt = now
			}
			r.Spawned = append(r.Spawned, e)
		}
		return save(path, r)
	})
}

// RemoveByPath drops the entry whose Path matches.
func RemoveByPath(p string) error {
	return withLock(func(path string) error {
		r, err := loadFromPath(path)
		if err != nil {
			return err
		}
		out := r.Spawned[:0]
		for _, e := range r.Spawned {
			if e.Path != p {
				out = append(out, e)
			}
		}
		r.Spawned = out
		return save(path, r)
	})
}

// List returns a snapshot of registry entries, dropping any whose dir
// no longer exists. Stale entries are also removed from the on-disk
// registry under an exclusive lock so the file doesn't accumulate
// dead paths.
func List() ([]Entry, error) {
	r, err := Load()
	if err != nil {
		return nil, err
	}
	live := make([]Entry, 0, len(r.Spawned))
	stalePaths := []string{}
	for _, e := range r.Spawned {
		if _, err := os.Stat(e.Path); err == nil {
			live = append(live, e)
		} else {
			stalePaths = append(stalePaths, e.Path)
		}
	}
	if len(stalePaths) > 0 {
		// Best-effort cleanup; ignore errors so a List() never fails
		// just because the registry couldn't be rewritten.
		_ = withLock(func(path string) error {
			cur, err := loadFromPath(path)
			if err != nil {
				return err
			}
			out := cur.Spawned[:0]
			for _, e := range cur.Spawned {
				stale := false
				for _, p := range stalePaths {
					if e.Path == p {
						stale = true
						break
					}
				}
				if !stale {
					out = append(out, e)
				}
			}
			cur.Spawned = out
			return save(path, cur)
		})
	}
	return live, nil
}

// loadFromPath reads the registry from a known path (used inside withLock
// so we don't reopen the file).
func loadFromPath(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return &Registry{}, nil
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}
	return &r, nil
}
