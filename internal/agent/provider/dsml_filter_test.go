package provider

import (
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/message"
)

// feedAll drives a dsmlFilter with the given chunks, returning the
// concatenation of safe output across all Feed calls plus a final
// Flush. Mirrors how runOpenAISSE drains the filter.
func feedAll(chunks []string) string {
	f := &dsmlFilter{}
	var got strings.Builder
	for _, c := range chunks {
		got.WriteString(f.Feed(c))
	}
	got.WriteString(f.Flush())
	return got.String()
}

func TestDSMLFilter_NoMarkerPassthrough(t *testing.T) {
	cases := []string{
		"",
		"plain ascii text",
		"text with < ascii angle brackets > intact",
		"unicode: 日本語 — em dash — fullwidth ｜ bar alone",
		"a < b && c > d is valid prose",
	}
	for _, in := range cases {
		got := feedAll([]string{in})
		if got != in {
			t.Errorf("passthrough mismatch\n in:  %q\n got: %q", in, got)
		}
	}
}

func TestDSMLFilter_StripsCompleteBlock(t *testing.T) {
	in := "before <｜DSML｜tool_calls｜>guts</｜DSML｜tool_calls｜> after"
	want := "before  after"
	if got := feedAll([]string{in}); got != want {
		t.Errorf("complete-block strip\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_StripsStrayOpener(t *testing.T) {
	// Opener with no closer → drop the held bytes on flush.
	in := "before <｜DSML｜invoke name=\"view\">"
	want := "before "
	if got := feedAll([]string{in}); got != want {
		t.Errorf("stray opener\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_OpenerSplitAcrossChunks(t *testing.T) {
	// Opener "<｜DSML｜" split byte-by-byte across many deltas: the
	// filter must hold each unsafe prefix and never flush partial.
	pieces := []string{"hello ", "<", "｜", "D", "S", "M", "L", "｜", "guts｜>tail</｜DSML｜x｜>!"}
	got := feedAll(pieces)
	want := "hello !"
	if got != want {
		t.Errorf("split opener\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_CloserSplitAcrossChunks(t *testing.T) {
	// Inside-block; the closing `｜>` arrives split across chunks.
	pieces := []string{"x <｜DSML｜a｜>inner</｜DSML｜b", "｜", ">", " y"}
	got := feedAll(pieces)
	want := "x  y"
	if got != want {
		t.Errorf("split closer\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_FalseAlarmPrefix(t *testing.T) {
	// `<` followed by bytes that disprove the opener — should flush.
	pieces := []string{"plain ", "<", "div>not dsml</div>"}
	got := feedAll(pieces)
	want := "plain <div>not dsml</div>"
	if got != want {
		t.Errorf("false-alarm prefix\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_MultipleBlocks(t *testing.T) {
	in := "A<｜DSML｜x｜>1</｜DSML｜x｜>B<｜DSML｜y｜>2</｜DSML｜y｜>C"
	want := "ABC"
	if got := feedAll([]string{in}); got != want {
		t.Errorf("multi-block\n got:  %q\n want: %q", got, want)
	}
}

func TestDSMLFilter_TruncatedBlockDropsHeld(t *testing.T) {
	// Inside a block at stream end → held bytes are dropped, not
	// surfaced as garbled text.
	pieces := []string{"prefix <｜DSML｜tool_calls｜>partial content with no closer"}
	got := feedAll(pieces)
	want := "prefix "
	if got != want {
		t.Errorf("truncated block\n got:  %q\n want: %q", got, want)
	}
}

func TestStripDSMLLeak_Regex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"clean text", "clean text"},
		{"a<｜DSML｜tool_calls｜>x</｜DSML｜tool_calls｜>b", "ab"},
		// Stray opener with no closer: everything after the opener
		// is dropped (we can't know where the leak ends).
		{"a<｜DSML｜invoke name=\"v\"｜>b", "a"},
		{"a<｜DSML｜x｜>1</｜DSML｜x｜>b<｜DSML｜y｜>2</｜DSML｜y｜>c", "abc"},
	}
	for _, c := range cases {
		if got := stripDSMLLeak(c.in); got != c.want {
			t.Errorf("stripDSMLLeak(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScrubDSMLParts(t *testing.T) {
	resp := &Response{
		Parts: []message.ContentPart{
			message.TextContent{Text: "before <｜DSML｜tool_calls｜>x</｜DSML｜tool_calls｜> after"},
			message.ToolCall{ID: "t1", Name: "view", Input: "{}", Type: "tool_use", Finished: true},
			message.TextContent{Text: "clean"},
		},
	}
	scrubDSMLParts(resp)

	tc0, ok := resp.Parts[0].(message.TextContent)
	if !ok || tc0.Text != "before  after" {
		t.Errorf("part 0 not scrubbed: %#v", resp.Parts[0])
	}
	if _, ok := resp.Parts[1].(message.ToolCall); !ok {
		t.Errorf("part 1 (tool call) should be untouched: %#v", resp.Parts[1])
	}
	tc2, ok := resp.Parts[2].(message.TextContent)
	if !ok || tc2.Text != "clean" {
		t.Errorf("part 2 should be untouched: %#v", resp.Parts[2])
	}
}
