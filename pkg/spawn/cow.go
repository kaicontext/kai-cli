package spawn

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Strategy is how the second-and-later spawned workspaces get cloned
// from the first. The CoW variants need filesystem support; full is
// always available.
type Strategy string

const (
	StrategyAuto Strategy = "auto" // CoW if available, fall back to full
	StrategyCoW  Strategy = "cow"  // require CoW; error if unavailable
	StrategyFull Strategy = "full" // always full copy
)

// Resolved is what Detect picked, after evaluating Auto against the
// target filesystem. It's what Copy actually executes.
type Resolved string

const (
	ResolvedAPFSClone Resolved = "apfs-clone" // darwin: cp -c
	ResolvedReflink   Resolved = "reflink"    // linux btrfs/xfs: cp --reflink=auto
	ResolvedFull      Resolved = "full"       // plain cp -R
)

// Detect chooses the actual copy mechanism for `targetParent` given
// the user's requested strategy. Returns (Resolved, error). For
// StrategyCoW, returns an error if the target FS doesn't support CoW.
//
// IMPORTANT: detection runs on the target parent dir, not the source.
// `cp -c` on darwin silently degrades to a regular copy when the
// destination is a different (non-APFS) filesystem from the source,
// so the destination's FS is the one that matters.
func Detect(targetParent string, requested Strategy) (Resolved, error) {
	if requested == StrategyFull {
		return ResolvedFull, nil
	}
	cow, fsName := detectCoW(targetParent)
	switch {
	case cow && runtime.GOOS == "darwin":
		return ResolvedAPFSClone, nil
	case cow && runtime.GOOS == "linux":
		return ResolvedReflink, nil
	case requested == StrategyCoW:
		return "", fmt.Errorf("--copy-strategy=cow but target filesystem (%s) doesn't support copy-on-write", fsName)
	default:
		return ResolvedFull, nil
	}
}

// Copy clones src to dst using the resolved strategy. dst must not yet
// exist (cp will create it). Both paths must be absolute.
func Copy(src, dst string, r Resolved) error {
	var cmd *exec.Cmd
	switch r {
	case ResolvedAPFSClone:
		cmd = exec.Command("cp", "-c", "-R", src, dst)
	case ResolvedReflink:
		cmd = exec.Command("cp", "--reflink=auto", "-R", src, dst)
	case ResolvedFull:
		cmd = exec.Command("cp", "-R", src, dst)
	default:
		return fmt.Errorf("unknown resolved strategy: %s", r)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp failed (%s → %s): %w: %s", src, dst, err, string(out))
	}
	return nil
}
