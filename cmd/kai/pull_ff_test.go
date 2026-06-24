package main

import "testing"

// Regression tests for the F-9 fast-forward guard: kai pull must let a behind
// clone catch up to a remote that merely advanced (a clean fast-forward),
// instead of aborting with a false "you have unpushed local snapshots that will
// become orphaned" / "diverged" error. It must still abort on a real divergence
// (local snapshots beyond what we last synced).
func TestIsFastForwardPull(t *testing.T) {
	base := []byte("BASE")   // the snapshot the clone was created from
	ahead := []byte("AHEAD") // the remote head after the origin moved on
	s2 := []byte("S2")       // a local, unpushed snapshot
	empty := []byte(nil)     // ref absent

	tests := []struct {
		name    string
		local   []byte // our current snap.latest
		tracked []byte // value we last pulled/pushed (remote/origin/*)
		remote  []byte // ref's current value on the remote
		wantFF  bool   // true => safe to fast-forward (don't abort)
	}{
		{
			name:    "F-9 behind clone: local is exactly what we last synced -> fast-forward",
			local:   base,
			tracked: base,
			remote:  ahead,
			wantFF:  true,
		},
		{
			name:    "no local ref at all -> any remote head is a fast-forward",
			local:   empty,
			tracked: empty,
			remote:  ahead,
			wantFF:  true,
		},
		{
			name:    "real divergence: we captured locally past what we synced -> abort",
			local:   s2,
			tracked: base,
			remote:  ahead,
			wantFF:  false,
		},
		{
			name:    "local moved off the tracking ref but we never synced (no tracking ref) -> abort",
			local:   s2,
			tracked: empty,
			remote:  ahead,
			wantFF:  false,
		},
		{
			name:    "behind clone catching up to a remote we have never tracked -> abort (can't prove safe)",
			local:   base,
			tracked: empty,
			remote:  ahead,
			wantFF:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isFastForwardPull(tc.local, tc.tracked, tc.remote)
			if got != tc.wantFF {
				t.Errorf("isFastForwardPull(local=%q, tracked=%q, remote=%q) = %v, want %v",
					tc.local, tc.tracked, tc.remote, got, tc.wantFF)
			}
		})
	}
}
