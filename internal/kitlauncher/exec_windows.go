//go:build windows

package kitlauncher

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// kitBinaryName is the managed kit's file name inside BinDir.
const kitBinaryName = "kit.exe"

// execHandoff is the Windows stand-in for exec(2), which does not exist
// there: run kit as a child with our stdio, then exit with its status so
// the caller still sees "kai code" end the way kit did. Ctrl-C reaches
// both processes through the shared console; we ignore it so kit alone
// decides what an interrupt means. Returns only when kit could not be
// started (the exec contract: a successful handoff never returns).
func execHandoff(argv0 string, argv []string, env []string) error {
	cmd := exec.Command(argv0, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signal.Ignore(os.Interrupt)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
