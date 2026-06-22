//go:build !darwin && !linux

package spawn

// On other platforms we never report CoW. Detect will return ResolvedFull
// (or fail loudly when --copy-strategy=cow is requested explicitly).
func detectCoW(path string) (bool, string) {
	return false, "unsupported"
}
