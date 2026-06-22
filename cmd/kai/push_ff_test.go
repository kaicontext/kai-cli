package main

import "testing"

// Regression tests for the F-13 fast-forward guard: kai push must refuse to
// overwrite a remote snap.latest/cs.latest that advanced since we last synced,
// instead of silently clobbering another user's snapshot.
func TestIsNonFastForwardPush(t *testing.T) {
	base := []byte("BASE") // the snapshot two users cloned from
	s1 := []byte("S1")     // user 1's snapshot (lands on the remote first)
	s2 := []byte("S2")     // user 2's snapshot (the contended push)
	empty := []byte(nil)   // ref absent on the remote

	tests := []struct {
		name    string
		remote  []byte // ref's current value on the remote
		tracked []byte // value we last pulled/pushed (remote/origin/*)
		newer   []byte // value we are about to push
		wantNFF bool   // true => must be rejected (non-fast-forward)
	}{
		{
			name:    "first push of a brand-new ref is allowed",
			remote:  empty,
			tracked: empty,
			newer:   s1,
			wantNFF: false,
		},
		{
			name:    "re-pushing the value already on the remote is a no-op, allowed",
			remote:  s1,
			tracked: s1,
			newer:   s1,
			wantNFF: false,
		},
		{
			name:    "true fast-forward: remote is exactly what we last synced",
			remote:  base,
			tracked: base,
			newer:   s1,
			wantNFF: false,
		},
		{
			name:    "F-13 contended push: remote advanced past our base -> reject",
			remote:  s1,   // user 1 already pushed
			tracked: base, // we only ever synced the base
			newer:   s2,   // our divergent snapshot
			wantNFF: true,
		},
		{
			name:    "remote present but we never synced it (no tracking ref) -> reject",
			remote:  s1,
			tracked: empty,
			newer:   s2,
			wantNFF: true,
		},
		{
			name:    "remote diverged from tracked, but we happen to push exactly the remote head -> allowed",
			remote:  s1,
			tracked: base,
			newer:   s1,
			wantNFF: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isNonFastForwardPush(tc.remote, tc.tracked, tc.newer)
			if got != tc.wantNFF {
				t.Errorf("isNonFastForwardPush(remote=%q, tracked=%q, new=%q) = %v, want %v",
					tc.remote, tc.tracked, tc.newer, got, tc.wantNFF)
			}
		})
	}
}
