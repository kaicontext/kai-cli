//go:build darwin

package spawn

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// detectCoW returns (true, "apfs") on APFS, (false, fstype) otherwise.
func detectCoW(path string) (bool, string) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, "unknown"
	}
	// Fstypename is a fixed-size [16]byte; trim NULs.
	name := string(bytes.TrimRight(st.Fstypename[:], "\x00"))
	return name == "apfs", name
}
