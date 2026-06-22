package tools

import "testing"

// TestByteInspectionPardon covers the 2026-05-25 chat-debug log
// pathology: model wanted `xxd file | grep pattern` to verify byte-
// level content when view's output was ambiguous. The chain walker
// flagged `grep` (a shadowed tool) and rejected the whole command.
// Now segments downstream of xxd / hexdump / od / file are pardoned —
// they're filtering byte output, not substituting for kai tools.
func TestByteInspectionPardon(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantTok string // empty = expect no shadow, otherwise the flagged token
	}{
		{
			name: "xxd alone passes",
			cmd:  "xxd /path/to/file",
		},
		{
			name: "xxd piped to grep — grep pardoned",
			cmd:  "xxd /path/to/file | grep pattern",
		},
		{
			name: "xxd piped to head — head pardoned",
			cmd:  "xxd /path/to/file | head -30",
		},
		{
			name: "hexdump piped to tail — tail pardoned",
			cmd:  "hexdump -C /path/to/file | tail -20",
		},
		{
			name: "od piped to grep — grep pardoned",
			cmd:  "od -c /path/to/file | grep -A2 each",
		},
		{
			name:    "cat alone still flagged (no byte tool in chain)",
			cmd:     "cat /path/to/file",
			wantTok: "cat",
		},
		{
			name:    "cat piped to grep — cat caught first, no pardon",
			cmd:     "cat /path/to/file | grep pattern",
			wantTok: "cat",
		},
		{
			name:    "grep alone — flagged",
			cmd:     "grep -r pattern .",
			wantTok: "grep",
		},
		{
			name:    "head before xxd — head flagged (byte tool comes too late)",
			cmd:     "head -100 /path/to/file | xxd",
			wantTok: "head",
		},
		{
			name: "chained byte inspection across &&",
			cmd:  "cd /tmp && xxd file | grep pattern",
			// cd is unflagged; xxd pardons grep
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := firstShadowedTokenInChain(c.cmd)
			if got != c.wantTok {
				t.Errorf("firstShadowedTokenInChain(%q) = %q, want %q", c.cmd, got, c.wantTok)
			}
		})
	}
}
