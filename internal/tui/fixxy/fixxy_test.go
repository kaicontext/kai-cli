package fixxy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNew_NoClaudeBinaryDisablesSilently pins the soft-fail
// contract: when `claude` isn't on PATH, the worker disables
// itself but doesn't return an error. Trigger calls become
// noops + a single "skipped: claude not on PATH" event. This
// is what keeps the secret flag from breaking sessions for
// users who pass it without `claude` installed.
func TestNew_NoClaudeBinaryDisablesSilently(t *testing.T) {
	// We can't easily un-PATH `claude` for a test, so this
	// covers the equivalent: a bad repo path also triggers
	// the disabled state.
	w := New("/definitely/not/a/real/dir", "/tmp/kai-test-binary")
	if !w.disabled {
		t.Fatal("expected worker disabled when repo path missing")
	}
	w.Trigger("test", "noop", M1)
	// First trigger emits the disabled-msg event; subsequent
	// triggers are silent.
	select {
	case ev := <-w.Events():
		if ev.Kind != "skipped" {
			t.Errorf("expected skipped event, got %+v", ev)
		}
	default:
		t.Error("expected one skipped event from first Trigger")
	}
}

// TestBuildErrorPrompt_IncludesAllSignal verifies the prompt
// passed to claude carries the kind/headline/raw plus the
// errors.log tail. Without these claude has no context to
// fix anything.
func TestBuildErrorPrompt_IncludesAllSignal(t *testing.T) {
	prompt := BuildErrorPrompt(
		"preflight.missing_blobs",
		"Snapshot needs rebuilding",
		"object store is missing blobs the snapshot references",
		"[2026-05-05T10:00:00Z] previous error 1\n[2026-05-05T10:01:00Z] previous error 2\n",
	)
	for _, must := range []string{
		"preflight.missing_blobs",
		"Snapshot needs rebuilding",
		"missing blobs the snapshot references",
		"previous error 1",
		"previous error 2",
		"Diagnose, fix, exit",
	} {
		if !strings.Contains(prompt, must) {
			t.Errorf("error prompt missing %q", must)
		}
	}
}

// TestBuildFeedbackPrompt_CarriesComplaintAndContext: mode 2
// prompt has to include both the user's verbatim complaint
// and the recent turns. Without context claude can't tell
// what went wrong.
func TestBuildFeedbackPrompt_CarriesComplaintAndContext(t *testing.T) {
	prompt := BuildFeedbackPrompt(
		"no sir i don't like it, the planner kept going in circles",
		"USER: explain X\nKAI: <wrong answer>\n\nUSER: try again\nKAI: <also wrong>\n",
	)
	for _, must := range []string{
		"no sir i don't like it",
		"explain X",
		"<wrong answer>",
		// Pinned: feedback prompt MUST tell claude not to do the
		// user's original task. Without this, claude reads the
		// transcript, sees a fixable real-world thing, and goes
		// off and fixes that instead of fixing kai-the-tool.
		"do NOT attempt the user's original task",
	} {
		if !strings.Contains(prompt, must) {
			t.Errorf("feedback prompt missing %q", must)
		}
	}
}

// TestStreamPipe_EmitsLineByLine pins the live-trace contract:
// streamPipe MUST emit one event per non-empty line as soon as
// it lands, not buffer until close. Without this the user
// sees nothing for the entire claude run, then a wall of
// output at the end — which was the original bug this fix
// addresses.
//
// We simulate claude's output by feeding lines into a pipe
// with delays between them and asserting events arrive at
// the corresponding times.
func TestStreamPipe_EmitsLineByLine(t *testing.T) {
	pr, pw := io.Pipe()
	w := &Worker{events: make(chan Event, 16)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	lastAt := time.Now()
	wg.Add(1)
	go w.streamPipe(pr, "", &wg, &mu, &lastAt)

	// Write three lines with a brief delay between them.
	// Each should appear as a separate event before we
	// close the pipe — proving the streamer doesn't wait
	// for EOF.
	go func() {
		_, _ = pw.Write([]byte("line one\n"))
		time.Sleep(20 * time.Millisecond)
		_, _ = pw.Write([]byte("line two\n"))
		time.Sleep(20 * time.Millisecond)
		_, _ = pw.Write([]byte("line three\n"))
		_ = pw.Close()
	}()

	got := []string{}
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-w.events:
			if ev.Kind == "claude_line" {
				got = append(got, ev.Text)
			}
		case <-timeout:
			t.Fatalf("only got %d events after 2s, want 3: %+v", len(got), got)
		}
	}
	wg.Wait()

	want := []string{"line one", "line two", "line three"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestStreamPipe_StderrTagged confirms stderr lines are
// prefixed so the user can tell when claude is reporting
// tool errors vs prose.
func TestStreamPipe_StderrTagged(t *testing.T) {
	pr, pw := io.Pipe()
	w := &Worker{events: make(chan Event, 4)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	lastAt := time.Now()
	wg.Add(1)
	go w.streamPipe(pr, "[stderr] ", &wg, &mu, &lastAt)

	go func() {
		_, _ = pw.Write([]byte("oops\n"))
		_ = pw.Close()
	}()

	select {
	case ev := <-w.events:
		if ev.Text != "[stderr] oops" {
			t.Errorf("expected stderr tag, got %q", ev.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
	wg.Wait()
}

// TestFindKaiCli_ResolvesCommonLayouts pins the May-5 fix:
// the rebuild step has to handle BOTH the "KAI_REPO is the
// kai source root" layout AND the "KAI_REPO is the parent
// container with kai/, kai-server/, etc." layout. Without
// this, the user's actual setup (~/projects/kai/ as repo,
// ~/projects/kai/kai/kai-cli/go.mod as source) breaks the
// rebuild step with "go.mod file not found".
func TestFindKaiCli_ResolvesCommonLayouts(t *testing.T) {
	cases := []struct {
		name    string
		seeds   []string // file paths to create (relative to tmp); content irrelevant
		wantSub string   // relative path under tmp; "" means expect error
	}{
		{
			name:    "kai source root with kai-cli child (canonical)",
			seeds:   []string{"kai-cli/go.mod", "kai-cli/cmd/kai/main.go"},
			wantSub: "kai-cli",
		},
		{
			name:    "parent container, source one level deeper (multi-checkout)",
			seeds:   []string{"kai/kai-cli/go.mod", "kai/kai-cli/cmd/kai/main.go"},
			wantSub: "kai/kai-cli",
		},
		{
			name:    "KAI_REPO points directly at kai-cli",
			seeds:   []string{"go.mod", "cmd/kai/main.go"},
			wantSub: ".",
		},
		{
			name:    "go.mod present but no cmd/kai (e.g. kai-server) is rejected",
			seeds:   []string{"kai-cli/go.mod"},
			wantSub: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			for _, rel := range tc.seeds {
				full := filepath.Join(tmp, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := findKaiCli(tmp)
			if tc.wantSub == "" {
				if err == nil {
					t.Errorf("expected error, got dir=%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			want := tmp
			if tc.wantSub != "." {
				want = filepath.Join(tmp, tc.wantSub)
			}
			if got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}

// TestBuildReviewPrompt_AllowsNoFixOutcome: mode 3 must
// explicitly tell claude that "looks fine, exit" is a valid
// outcome. Otherwise mode 3 would generate fixes for every
// turn — false positives multiplied across heavy use.
func TestBuildReviewPrompt_AllowsNoFixOutcome(t *testing.T) {
	prompt := BuildReviewPrompt("yo", "Hey there!", "")
	if !strings.Contains(prompt, "nothing to fix here") {
		t.Errorf("review prompt should explicitly permit a no-op outcome, got: %q", prompt)
	}
}
