package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/authorship"
	"kai/internal/kaipath"
	spawnpkg "kai/pkg/spawn"
	"kai/pkg/synclog"
)

// `kai ui` — v0 dashboard. Localhost-only, polls JSON endpoints,
// embeds a single-page vanilla-JS UI. Not a daemon, not a tray icon.
// Reads the spawn registry + each spawned dir's sync-log JSONL.

//go:embed ui/index.html
var uiHTML embed.FS

var (
	uiPort       int
	uiNoBrowser  bool
	uiApp        bool
	uiPalette    = []string{"#ef4444", "#3b82f6", "#22c55e", "#a855f7", "#f97316", "#14b8a6", "#eab308", "#ec4899"}
	syncLogLimit = 200
)

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the local Kai dashboard in your browser",
	Long: `Starts a local HTTP server (127.0.0.1 only) and opens the dashboard
in your default browser. Shows live status of every spawned workspace
and a real-time feed of sync events.

The server runs in the foreground; Ctrl+C exits.`,
	RunE: runUI,
}

func init() {
	uiCmd.Flags().IntVar(&uiPort, "port", 0, "Port to listen on (0 = random free port)")
	uiCmd.Flags().BoolVar(&uiNoBrowser, "no-browser", false, "Don't auto-open a browser")
	uiCmd.Flags().BoolVar(&uiApp, "app", false, "Open in a Chromium-based browser's app mode (borderless single window, dock icon)")
}

func runUI(cmd *cobra.Command, args []string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents", handleAgents)
	mux.HandleFunc("/api/events", handleEvents)
	mux.HandleFunc("/api/header", handleHeader)
	mux.HandleFunc("/", serveIndex)

	addr := fmt.Sprintf("127.0.0.1:%d", uiPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	fmt.Printf("Kai dashboard → %s\n", url)
	fmt.Println("Ctrl+C to exit")

	if !uiNoBrowser {
		go func() {
			time.Sleep(200 * time.Millisecond)
			openBrowser(url)
		}()
	}
	return http.Serve(ln, mux)
}

// ---------------------------------------------------------------------------
// Handlers

type agentDTO struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Path        string `json:"path"`
	Workspace   string `json:"workspace"`
	SyncMode    string `json:"sync_mode"`
	SourceRepo  string `json:"source_repo,omitempty"` // e.g. "kaicontext/kai"
	Checkpoints int    `json:"checkpoints"`
	UptimeSec   int64  `json:"uptime_sec"`
	LastFile    string `json:"last_file,omitempty"`
	LastEventTs int64  `json:"last_event_ts,omitempty"`
	Sparkline   []int  `json:"sparkline"`
	TaskHint    string `json:"task_hint,omitempty"`
}

func handleAgents(w http.ResponseWriter, r *http.Request) {
	entries, err := spawnpkg.List()
	if err != nil {
		writeJSON(w, []agentDTO{})
		return
	}
	out := make([]agentDTO, 0, len(entries))
	colorIdx := 0
	for _, e := range entries {
		if _, err := os.Stat(e.Path); err != nil {
			continue
		}
		kdPath := kaipath.Resolve(e.Path)
		dto := agentDTO{
			Name:       displayAgentName(e.Agent, e.WorkspaceName),
			Color:      uiPalette[colorIdx%len(uiPalette)],
			Path:       e.Path,
			Workspace:  e.WorkspaceName,
			SyncMode:   e.SyncMode,
			SourceRepo: e.RepoChannel,
		}
		colorIdx++
		dto.Checkpoints = countCheckpointFiles(kdPath)
		if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
			dto.UptimeSec = int64(time.Since(t).Seconds())
		}
		lastFile, lastTs, sparks := summarizeActivity(kdPath, time.Now())
		dto.LastFile = lastFile
		dto.LastEventTs = lastTs
		dto.Sparkline = sparks
		// task_hint: no source today; left blank for v0. Wire to
		// changeset intent or workspace description in v1.
		out = append(out, dto)
	}
	writeJSON(w, out)
}

type eventDTO struct {
	Type      string `json:"type"`     // checkpoint | push | recv | merge | conflict | skip
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Color     string `json:"color"`
	File      string `json:"file,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Detail    string `json:"detail,omitempty"`
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	entries, err := spawnpkg.List()
	if err != nil {
		writeJSON(w, []eventDTO{})
		return
	}
	all := make([]eventDTO, 0, 64)
	colorIdx := 0
	for _, e := range entries {
		color := uiPalette[colorIdx%len(uiPalette)]
		colorIdx++
		if _, err := os.Stat(e.Path); err != nil {
			continue
		}
		kdPath := kaipath.Resolve(e.Path)
		displayName := displayAgentName(e.Agent, e.WorkspaceName)
		// Sync events (peer push/recv/merge/conflict).
		for _, ev := range readRecentSyncLog(kdPath, 50) {
			all = append(all, eventDTO{
				Type:      ev.Event,
				Agent:     ev.Agent,
				AgentName: displayName,
				Color:     color,
				File:      ev.File,
				Timestamp: ev.Timestamp,
				Detail:    ev.Detail,
			})
		}
		// Local checkpoint events (this agent's authored edits).
		if cps, err := authorship.ReadPendingCheckpoints(kdPath); err == nil {
			for _, cp := range cps {
				all = append(all, eventDTO{
					Type:      "checkpoint",
					Agent:     cp.Agent,
					AgentName: displayName,
					Color:     color,
					File:      cp.File,
					Timestamp: cp.Timestamp,
				})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp > all[j].Timestamp })
	if len(all) > syncLogLimit {
		all = all[:syncLogLimit]
	}
	writeJSON(w, all)
}

type headerDTO struct {
	AgentCount int      `json:"agent_count"`
	RepoCount  int      `json:"repo_count"`
	Repos      []string `json:"repos"`            // distinct source repos, sorted
	SoleRepo   string   `json:"sole_repo,omitempty"` // set when RepoCount == 1
}

// handleHeader summarizes the spawned-workspace registry: how many
// agents are live and which source repos they came from. The dashboard
// is global across the machine — it doesn't care about cwd.
func handleHeader(w http.ResponseWriter, r *http.Request) {
	entries, _ := spawnpkg.List()
	seen := map[string]bool{}
	repos := []string{}
	live := 0
	for _, e := range entries {
		if _, err := os.Stat(e.Path); err != nil {
			continue
		}
		live++
		if e.RepoChannel != "" && !seen[e.RepoChannel] {
			seen[e.RepoChannel] = true
			repos = append(repos, e.RepoChannel)
		}
	}
	sort.Strings(repos)
	dto := headerDTO{AgentCount: live, RepoCount: len(repos), Repos: repos}
	if len(repos) == 1 {
		dto.SoleRepo = repos[0]
	}
	writeJSON(w, dto)
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := uiHTML.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// ---------------------------------------------------------------------------
// Helpers

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func displayAgentName(agent, ws string) string {
	if agent != "" {
		return agent
	}
	return ws
}

func countCheckpointFiles(kdPath string) int {
	root := filepath.Join(kdPath, "checkpoints")
	count := 0
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// readRecentSyncLog returns the most recent N entries from today's
// sync-log file, newest first.
func readRecentSyncLog(kdPath string, max int) []synclog.SyncLogEntry {
	logDir := filepath.Join(kdPath, "sync-log")
	entries := []synclog.SyncLogEntry{}
	dir, err := os.ReadDir(logDir)
	if err != nil {
		return entries
	}
	// Sort filenames descending to read newest first.
	files := make([]string, 0, len(dir))
	for _, d := range dir {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, d.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	for _, fname := range files {
		data, err := os.ReadFile(filepath.Join(logDir, fname))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		// Walk newest-first within a file.
		for i := len(lines) - 1; i >= 0; i-- {
			if lines[i] == "" {
				continue
			}
			var e synclog.SyncLogEntry
			if json.Unmarshal([]byte(lines[i]), &e) != nil {
				continue
			}
			entries = append(entries, e)
			if len(entries) >= max {
				return entries
			}
		}
	}
	return entries
}

// summarizeActivity returns the most recent file the agent touched, its
// timestamp, and a 20-bucket sparkline of activity over the last 5
// minutes. Pulls from BOTH the sync-log (peer push/recv events) AND the
// local checkpoint files (authorship records), so the dashboard reflects
// agent activity even when live-sync hasn't fired any peer events.
func summarizeActivity(kdPath string, now time.Time) (string, int64, []int) {
	const buckets = 20
	const windowSec = 300
	bucketSec := int64(windowSec / buckets)
	hist := make([]int, buckets)
	cutoff := now.Add(-time.Duration(windowSec) * time.Second).UnixMilli()

	type tsFile struct {
		ts   int64
		file string
	}
	all := []tsFile{}

	for _, e := range readRecentSyncLog(kdPath, 500) {
		all = append(all, tsFile{ts: e.Timestamp, file: e.File})
	}
	if cps, err := authorship.ReadPendingCheckpoints(kdPath); err == nil {
		for _, cp := range cps {
			all = append(all, tsFile{ts: cp.Timestamp, file: cp.File})
		}
	}

	var lastFile string
	var lastTs int64
	for _, x := range all {
		if x.ts > lastTs {
			lastTs = x.ts
			if x.file != "" {
				lastFile = x.file
			}
		}
		if x.ts < cutoff {
			continue
		}
		idx := int((now.UnixMilli()-x.ts)/(bucketSec*1000)) % buckets
		if idx < 0 || idx >= buckets {
			continue
		}
		hist[buckets-1-idx]++
	}
	return lastFile, lastTs, hist
}

func openBrowser(url string) {
	if uiApp {
		if openAppMode(url) {
			return
		}
		fmt.Fprintln(os.Stderr, "kai ui: --app requested but no Chromium-based browser found, falling back to default browser")
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("cmd", "/c", "start", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}

// openAppMode launches the URL in a Chromium-based browser's "app mode"
// (--app=<url>) — borderless single window, no tabs/address bar, gets
// its own dock icon. Tries Chrome → Edge → Brave → Arc → Vivaldi →
// Chromium until one launches. Returns true on success.
func openAppMode(url string) bool {
	type candidate struct {
		appName string   // for `open -a` on darwin
		bin     []string // executables to try via PATH on linux/windows
	}
	candidates := []candidate{
		{"Google Chrome", []string{"google-chrome", "chrome", "chrome.exe"}},
		{"Microsoft Edge", []string{"microsoft-edge", "msedge", "msedge.exe"}},
		{"Brave Browser", []string{"brave-browser", "brave", "brave.exe"}},
		{"Arc", nil},
		{"Vivaldi", []string{"vivaldi", "vivaldi.exe"}},
		{"Chromium", []string{"chromium", "chromium-browser"}},
	}
	for _, c := range candidates {
		if runtime.GOOS == "darwin" && c.appName != "" {
			if appExistsDarwin(c.appName) {
				cmd := exec.Command("open", "-na", c.appName, "--args", "--app="+url)
				if err := cmd.Start(); err == nil {
					return true
				}
			}
			continue
		}
		for _, b := range c.bin {
			if path, err := exec.LookPath(b); err == nil {
				cmd := exec.Command(path, "--app="+url)
				if err := cmd.Start(); err == nil {
					return true
				}
			}
		}
	}
	return false
}

// appExistsDarwin checks whether a macOS .app bundle is installed in
// any of the standard locations. We use a direct filesystem check
// rather than `mdfind` because Spotlight indexing can be paused or
// missing on dev machines, returning false negatives even for apps
// that are clearly installed.
func appExistsDarwin(appName string) bool {
	bundle := appName + ".app"
	roots := []string{"/Applications", "/System/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, bundle)); err == nil {
			return true
		}
	}
	return false
}
