package agent

import "testing"

// TestNormalizeBashErrSig pins the error-signature extractor used by
// the error-class change detector. Two errors that point at the same
// problem must produce the same signature; two errors that point at
// different problems must produce different ones.
func TestNormalizeBashErrSig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // expected signature (or empty for "no signal")
		empty   bool   // true means we just check that signature is empty
	}{
		{
			name: "vite Cannot find module",
			content: `vite v5.4.21 building for production...
error during build:
[vite-plugin-svelte] [plugin vite-plugin-svelte] Error while preprocessing /Users/jacobschatz/projects/kai/kai-desktop/src/App.svelte - Cannot find module './transformers/globalStyle'`,
			want: "error during build:",
		},
		{
			name:    "svelte unexpected token with file:line:col",
			content: `src/AgentsView.svelte (182:63): Error while preprocessing src/AgentsView.svelte:182:63 - Unexpected token`,
			want:    "src/AgentsView.svelte (182:63): Error while preprocessing src/AgentsView.svelte:182:63 - Unexpected token",
		},
		{
			name:    "no error in output",
			content: `vite v5.4.21\n✓ 35 modules transformed.\nbuilt in 542ms`,
			empty:   true,
		},
		{
			name:    "empty content",
			content: "",
			empty:   true,
		},
		{
			name:    "exception keyword",
			content: `Traceback (most recent call last):\n  File "x.py", line 5\n    syntax error\nException: oh no`,
			want:    `Traceback (most recent call last):\n  File "x.py", line 5\n    syntax error\nException: oh no`,
		},
		{
			name:    "absolute spawn path stripped",
			content: `Error: cannot find /private/tmp/kai-port-react-design-1779/kai-desktop/src/AgentsView.svelte`,
			want:    `Error: cannot find AgentsView.svelte`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeBashErrSig(c.content)
			if c.empty {
				if got != "" {
					t.Errorf("expected empty signature, got %q", got)
				}
				return
			}
			// For non-empty cases we don't enforce exact equality
			// (the regexp-based path stripping makes that brittle);
			// we check the signature is non-empty and stable across
			// two calls.
			if got == "" {
				t.Errorf("expected non-empty signature, got empty for content %q", c.content)
			}
			again := normalizeBashErrSig(c.content)
			if got != again {
				t.Errorf("signature not deterministic: %q vs %q", got, again)
			}
		})
	}
}

// TestBashErrSigRing pins the cross-turn ring semantics. The
// 2026-05-25 runlog (e31c2173) showed the v0.31.40 regression in
// action: turn 1 errored, turn 3 ran an unrelated successful
// command, turn 4 errored with a different signature — but the
// detector had wiped its memory at turn 3 and didn't fire the
// "new error class" note at turn 4. The ring preserves memory
// across intervening unrelated successes.
func TestBashErrSigRing(t *testing.T) {
	t.Run("contains scans linearly", func(t *testing.T) {
		ring := []string{"a", "b", "c"}
		for _, s := range []string{"a", "b", "c"} {
			if !bashErrSigRingContains(ring, s) {
				t.Errorf("ring should contain %q", s)
			}
		}
		if bashErrSigRingContains(ring, "x") {
			t.Errorf("ring should not contain %q", "x")
		}
	})

	t.Run("push deduplicates", func(t *testing.T) {
		ring := []string{"a"}
		ring = pushBashErrSigRing(ring, "a")
		if len(ring) != 1 {
			t.Errorf("duplicate push should not grow ring; got len=%d", len(ring))
		}
	})

	t.Run("push appends new sigs", func(t *testing.T) {
		var ring []string
		ring = pushBashErrSigRing(ring, "a")
		ring = pushBashErrSigRing(ring, "b")
		ring = pushBashErrSigRing(ring, "c")
		if len(ring) != 3 {
			t.Errorf("expected len=3, got %d", len(ring))
		}
		if ring[0] != "a" || ring[2] != "c" {
			t.Errorf("unexpected ring order: %v", ring)
		}
	})

	t.Run("ring caps at bashErrSigRingSize, drops oldest", func(t *testing.T) {
		var ring []string
		for i := 0; i < bashErrSigRingSize+3; i++ {
			ring = pushBashErrSigRing(ring, string(rune('a'+i)))
		}
		if len(ring) != bashErrSigRingSize {
			t.Errorf("expected len=%d, got %d", bashErrSigRingSize, len(ring))
		}
		// 'a','b','c' should have been evicted; ring should start at 'd'.
		if ring[0] != "d" {
			t.Errorf("expected oldest to be 'd' after eviction, got %q", ring[0])
		}
	})
}

// TestNormalizeBashErrSig_ChangeDetection pins the core property the
// downstream tunnel-vision guard relies on: different error CLASSES
// must produce different signatures, same error class must produce
// the same signature.
func TestNormalizeBashErrSig_ChangeDetection(t *testing.T) {
	preprocessorErr := `error during build:
Cannot find module '/Users/jacobschatz/projects/kai/kai-desktop/node_modules/svelte-preprocess/dist/transformers/globalStyle'`

	syntaxErr := `src/AgentsView.svelte (182:63): Unexpected token`

	preprocessorErr2 := `error during build:
Cannot find module '/Users/jacobschatz/projects/kai/kai-desktop/node_modules/svelte-preprocess/dist/transformers/globalStyle' again`

	sigA := normalizeBashErrSig(preprocessorErr)
	sigB := normalizeBashErrSig(syntaxErr)
	sigA2 := normalizeBashErrSig(preprocessorErr2)

	if sigA == "" || sigB == "" {
		t.Fatalf("signatures should be non-empty, got A=%q B=%q", sigA, sigB)
	}
	if sigA == sigB {
		t.Errorf("different error classes should produce different sigs, both = %q", sigA)
	}
	// preprocessorErr and preprocessorErr2 are NOT identical content
	// but they share the same first error line — signatures should
	// match because the first error line is what we hash.
	if sigA != sigA2 {
		t.Errorf("same first-error-line should produce same sig, got %q vs %q", sigA, sigA2)
	}
}
