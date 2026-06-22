//go:build linux

package spawn

import (
	"golang.org/x/sys/unix"
)

// Magic numbers from <linux/magic.h>. Filesystems with reflink/CoW
// support that we care about: btrfs, xfs (when mkfs'd with reflink=1).
// XFS without reflink will fail `cp --reflink=auto` silently and copy
// normally — fine for us.
const (
	btrfsSuperMagic uint32 = 0x9123683E
	xfsSuperMagic   uint32 = 0x58465342
)

func detectCoW(path string) (bool, string) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, "unknown"
	}
	switch uint32(st.Type) {
	case btrfsSuperMagic:
		return true, "btrfs"
	case xfsSuperMagic:
		return true, "xfs"
	default:
		return false, "other"
	}
}
