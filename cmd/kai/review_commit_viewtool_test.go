package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/projects"
	"github.com/kaicontext/kai-engine/tools"
)

// The citation parser against the REAL file tool, not a hand-written imitation
// of its output. Whatever kai_view returns for an offset spelled any way the
// tool accepts, the parser must map the same rows to the same file lines —
// and a citation of "file line 1" must reach the file's first line, never the
// tool-call header. Reproduces the case where `"offset": "0.0"` made the parser
// fall back to row numbering and file line 1 resolved to the header.
func rcRealView(t *testing.T, dir, input string) string {
	t.Helper()
	ft := &tools.FileTools{Set: projects.Single(dir)}
	resp, err := ft.View().Run(context.Background(), tools.ToolCall{ID: "v", Name: "kai_view", Input: input})
	if err != nil {
		t.Fatalf("view %s: %v", input, err)
	}
	if resp.IsError {
		t.Fatalf("view %s refused: %s", input, resp.Content)
	}
	return resp.Content
}

func TestKaiViewOffsetSpellingsMatchTheRealTool(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	for i := 1; i <= 30; i++ {
		body.WriteString("line " + itoa2(i) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		offset    string // the JSON as the model wrote it; "" = key absent
		wantFirst int
	}{
		{"", 1}, {"0", 1}, {"0.0", 1}, {`"0.0"`, 1}, {"null", 1}, {`""`, 1}, {`" 0 "`, 1}, {"-5", 1}, {`"-5"`, 1},
		{"3", 4}, {`"3"`, 4}, {"3.9", 4}, {`" 3 "`, 4}, {`"1e1"`, 11},
	} {
		input := `{"file_path":"f.txt","limit":5`
		if tc.offset != "" {
			input += `,"offset":` + tc.offset
		}
		input += "}"
		t.Run(input, func(t *testing.T) {
			content := rcRealView(t, dir, input)
			src := rcToolSource("kai_view", input, content)
			if src.Coord != rcCoordFile {
				t.Fatalf("not file-addressed (%s: %s) for tool output:\n%s", src.Coord, src.Why, content)
			}
			if src.First != tc.wantFirst || len(src.Rows) != 5 || src.Rows[0] != "line "+itoa2(tc.wantFirst) {
				t.Fatalf("mapping: first=%d rows=%d row0=%q; want first %d", src.First, len(src.Rows), src.Rows[0], tc.wantFirst)
			}
			// The first returned file line is citable and is the file's line,
			// never the header; the line before the slice is not citable.
			sources := []rcSource{rcPromptSource("p"), src}
			got, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 2, LineStart: tc.wantFirst, LineEnd: tc.wantFirst})
			if !ok || got != "line "+itoa2(tc.wantFirst) {
				t.Fatalf("citation of file line %d: %q %v", tc.wantFirst, got, ok)
			}
			if tc.wantFirst > 1 {
				if _, _, ok := rcExtractCitation(sources, rcCheckEvidence{Source: 2, LineStart: tc.wantFirst - 1, LineEnd: tc.wantFirst - 1}); ok {
					t.Fatalf("line %d before the slice was citable", tc.wantFirst-1)
				}
			}
		})
	}
	// An offset the tool refuses yields an error response and therefore no
	// source at all — the parser is never asked. Confirm the tool's behavior so
	// the parser's own rejection list stays aligned with it.
	ft := &tools.FileTools{Set: projects.Single(dir)}
	resp, _ := ft.View().Run(context.Background(), tools.ToolCall{ID: "v", Name: "kai_view", Input: `{"file_path":"f.txt","offset":"abc"}`})
	if !resp.IsError {
		t.Fatalf("the tool accepted offset \"abc\": %s", resp.Content)
	}
	if src := rcToolSource("kai_view", `{"file_path":"f.txt","offset":"abc"}`, "1: line 1\n"); src.Coord != rcCoordNone {
		t.Fatalf("parser accepted an offset the tool refuses: %+v", src)
	}
}

// The real tool on a slice past the end and on a truncated slice.
func TestKaiViewRealToolEdgesAreMappedOrUnmapped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Past the end: the tool returns a notice, not rows → unmapped, uncitable.
	content := rcRealView(t, dir, `{"file_path":"g.txt","offset":40}`)
	src := rcToolSource("kai_view", `{"file_path":"g.txt","offset":40}`, content)
	if src.Coord != rcCoordNone {
		t.Fatalf("past-the-end result mapped: %+v\n%s", src, content)
	}
	// Truncated: two rows returned of four (the trailing newline makes a
	// phantom empty fourth); the trailer names line 3 but it is not citable.
	content = rcRealView(t, dir, `{"file_path":"g.txt","offset":0,"limit":2}`)
	src = rcToolSource("kai_view", `{"file_path":"g.txt","offset":0,"limit":2}`, content)
	if src.Coord != rcCoordFile || src.First != 1 || len(src.Rows) != 2 || !strings.Contains(content, "(truncated;") {
		t.Fatalf("truncated slice: %+v\n%s", src, content)
	}
	if _, _, ok := rcExtractCitation([]rcSource{src}, rcCheckEvidence{Source: 1, LineStart: 3, LineEnd: 3}); ok {
		t.Fatal("a line the tool did not return was citable")
	}
	// The response the tool actually produced, kept on the record for review.
	raw, _ := json.Marshal(map[string]any{"input": `{"file_path":"g.txt","offset":0,"limit":2}`, "content": content})
	t.Logf("real view output: %s", raw)
}

func itoa2(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
