package orchestrator

import "testing"

func TestScopeAwareReadStreak(t *testing.T) {
	cases := []struct {
		files int
		soft  int
		hard  int
	}{
		{1, 2, 5},
		{2, 3, 7},
		{4, 3, 7},
		{5, 0, 0},  // falls back to runner defaults
		{10, 0, 0}, // ditto
		{0, 0, 0},  // unknown scope → defaults
	}
	for _, c := range cases {
		soft, hard := scopeAwareReadStreak(c.files)
		if soft != c.soft || hard != c.hard {
			t.Errorf("files=%d: got (%d, %d), want (%d, %d)", c.files, soft, hard, c.soft, c.hard)
		}
	}
}
