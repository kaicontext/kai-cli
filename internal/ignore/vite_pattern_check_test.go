package ignore

import "testing"

func TestLoadDefaults_ViteTimestampPattern(t *testing.T) {
	m := NewMatcher(".")
	m.LoadDefaults()
	cases := []struct {
		path  string
		match bool
	}{
		{"vite.config.mjs.timestamp-1779719033227-02f5a0952b2198.mjs", true},
		{"vite.config.js.timestamp-1234567890-abc.js", true},
		{"vite.config.ts.timestamp-9999.mjs", true},
		{"src/vite.config.mjs.timestamp-1.mjs", true},
		{"vite.config.mjs", false},
		{"vite.config.ts", false},
		{"AgentsView.svelte", false},
		{"some-other-timestamp-file.mjs", false},
	}
	for _, c := range cases {
		got := m.Match(c.path, false)
		if got != c.match {
			t.Errorf("Match(%q) = %v, want %v", c.path, got, c.match)
		}
	}
}
