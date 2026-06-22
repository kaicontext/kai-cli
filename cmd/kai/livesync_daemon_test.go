package main

import (
	"os"
	"testing"
)

// TestAcquireLiveRunLock_SingleInstance verifies the flock guard: while one
// holder has the lock, a second acquire is refused; after the first releases,
// a new acquire succeeds. This is what prevents orphan daemons from piling up.
func TestAcquireLiveRunLock_SingleInstance(t *testing.T) {
	dir := t.TempDir()

	f1, ok1 := acquireLiveRunLock(dir)
	if !ok1 || f1 == nil {
		t.Fatalf("first acquire should succeed, got ok=%v file=%v", ok1, f1)
	}

	// Second acquire while the first is held must be refused.
	f2, ok2 := acquireLiveRunLock(dir)
	if ok2 {
		t.Fatalf("second acquire should be refused while first is held")
	}
	if f2 != nil {
		f2.Close()
	}

	// Release the first; a fresh acquire should now succeed.
	if err := f1.Close(); err != nil {
		t.Fatalf("closing first lock: %v", err)
	}
	f3, ok3 := acquireLiveRunLock(dir)
	if !ok3 || f3 == nil {
		t.Fatalf("acquire after release should succeed, got ok=%v", ok3)
	}
	_ = f3.Close()
	_ = os.Remove(dir + "/livesync.lock")
}
