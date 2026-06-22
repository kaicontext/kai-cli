// Package clipboard copies text to the user's clipboard, prioritizing
// terminal-relayed copy (OSC 52) over host-side helpers.
//
// Why OSC 52 first: kai is often used over SSH, where pbcopy/xclip
// would write to the *remote* host's clipboard, not the user's.
// Modern terminals (iTerm2, kitty, WezTerm, Alacritty, recent tmux)
// implement the OSC 52 escape sequence to relay clipboard writes back
// to the local machine. Falling back to host helpers covers terminals
// that don't (default macOS Terminal.app, plain xterm).
package clipboard

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Copy writes s to the clipboard. It tries OSC 52 first (works over
// SSH if the terminal supports it), then falls back to the host's
// platform helper. Returns nil on best-effort success — OSC 52 has no
// reply channel, so we can't actually confirm the terminal honored it.
func Copy(s string) error {
	emitOSC52(s)
	return systemCopy(s)
}

// emitOSC52 writes the OSC 52 sequence to stderr. Format:
// `ESC ] 52 ; c ; <base64> BEL`. "c" is the system clipboard. Stderr
// (not stdout) because bubbletea owns stdout for its renderer.
func emitOSC52(s string) {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	fmt.Fprintf(os.Stderr, "\x1b]52;c;%s\x07", enc)
}

// systemCopy shells out to the platform clipboard helper. Silent
// no-op if no helper is on PATH — the OSC 52 emit above is the
// primary path, this is just a safety net.
func systemCopy(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		// Wayland first, then X11 — xclip silently succeeds inside
		// Wayland sessions but lands in a hidden Xwayland clipboard.
		for _, bin := range []string{"wl-copy", "xclip", "xsel"} {
			if _, err := exec.LookPath(bin); err != nil {
				continue
			}
			switch bin {
			case "xclip":
				cmd = exec.Command(bin, "-selection", "clipboard")
			case "xsel":
				cmd = exec.Command(bin, "--clipboard", "--input")
			default:
				cmd = exec.Command(bin)
			}
			break
		}
	case "windows":
		cmd = exec.Command("clip")
	}
	if cmd == nil {
		return nil
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
