package tools

import "testing"

func TestRewritePipeAlternation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", "foo"},
		{"a|b", "a OR b"},
		{"sidebar|Sidebar", "sidebar OR Sidebar"},
		{"contextBridge|exposeInMainWorld", "contextBridge OR exposeInMainWorld"},
		{"  a | b | c  ", "a OR b OR c"},
		{"a||b", "a OR b"}, // empty parts dropped
		{`"a|b"`, `"a|b"`}, // quoted phrase left alone
		{"", ""},
	}
	for _, c := range cases {
		got := rewritePipeAlternation(c.in)
		if got != c.want {
			t.Errorf("rewritePipeAlternation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
