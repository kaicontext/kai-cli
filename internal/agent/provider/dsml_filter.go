package provider

import (
	"strings"

	"kai/internal/agent/message"
)

// DeepSeek-V4-Pro routes some tool-call attempts through the *content*
// channel instead of OpenAI's `tool_calls` array, leaking its native
// training-format delimiters as plain text. Example seen in the TUI:
//
//	<｜DSML｜tool_calls｜>
//	<｜DSML｜invoke name="view"｜>
//	<｜DSML｜parameter name="file_path"｜>kai-cli/...</｜DSML｜parameter｜>
//	</｜DSML｜invoke｜>
//	</｜DSML｜tool_calls｜>
//
// The wire format can't carry those — the parser sees them as opaque
// assistant text and they render verbatim in scrollback. We strip
// them at the provider boundary so nothing downstream needs to know.
//
// The bar `｜` is U+FF5C (fullwidth vertical line), 3 bytes in UTF-8;
// the literals below must stay fullwidth — that's what the model
// emits, and ASCII `|` will not match.
//
// Tag shapes we recognize:
//   - opener: `<｜DSML｜<anything>>` — increments depth.
//   - closer: `</｜DSML｜<anything>>` — decrements depth.
//
// When depth > 0 every byte is suppressed (including inner text and
// nested tags). When depth returns to 0 we resume emitting normally.

const (
	dsmlOpenPrefix  = "<｜DSML｜"
	dsmlClosePrefix = "</｜DSML｜"
)

// stripDSMLLeak removes DSML blocks from s using the same state
// machine as the streaming filter, so non-streaming and streaming
// produce identical output. Safe on text that contains no markers.
func stripDSMLLeak(s string) string {
	if s == "" || !strings.Contains(s, dsmlOpenPrefix) {
		return s
	}
	f := &dsmlFilter{}
	return f.Feed(s) + f.Flush()
}

// scrubDSMLParts walks resp.Parts in place and strips DSML markers
// from every TextContent. Called after streaming has assembled the
// final accumulator text, so conversation history matches what the
// user saw on screen.
func scrubDSMLParts(resp *Response) {
	if resp == nil {
		return
	}
	for i, p := range resp.Parts {
		if tc, ok := p.(message.TextContent); ok {
			cleaned := stripDSMLLeak(tc.Text)
			if cleaned != tc.Text {
				resp.Parts[i] = message.TextContent{Text: cleaned}
			}
		}
	}
}

// dsmlFilter is the streaming sanitizer. Feed it text deltas as they
// arrive; it returns the prefix that's safe to forward now and holds
// back any tail that could still grow into a DSML opener/closer or
// complete an in-flight tag. Use one instance per SSE stream.
//
// Hold cases (returned in a later Feed/Flush):
//   - `<` that's a prefix of `<｜DSML｜` or `</｜DSML｜`.
//   - A confirmed tag whose closing `>` hasn't arrived yet.
//
// At Flush() time, anything still inside an open block (depth > 0)
// is dropped — a truncated leak is still leak text we don't want
// to render.
type dsmlFilter struct {
	pending strings.Builder // bytes not yet safe to commit
	depth   int             // number of open DSML blocks
}

// Feed appends chunk and returns the bytes safe to emit now. The
// unreturned remainder stays in f.pending for the next Feed/Flush.
func (f *dsmlFilter) Feed(chunk string) string {
	if chunk == "" && f.pending.Len() == 0 {
		return ""
	}
	f.pending.WriteString(chunk)
	buf := f.pending.String()
	f.pending.Reset()

	var out strings.Builder
	i := 0
	for i < len(buf) {
		// Find the next `<` — that's the only character that can
		// start a tag we care about. Bytes before it are either
		// safe (depth 0) or dropped (depth > 0).
		j := strings.IndexByte(buf[i:], '<')
		if j < 0 {
			// No more potential tags in this buffer.
			if f.depth == 0 {
				out.WriteString(buf[i:])
			}
			return out.String()
		}
		if f.depth == 0 {
			out.WriteString(buf[i : i+j])
		}
		i += j
		rest := buf[i:]

		isOpener := strings.HasPrefix(rest, dsmlOpenPrefix)
		isCloser := strings.HasPrefix(rest, dsmlClosePrefix)

		if !isOpener && !isCloser {
			// Could still be a partial prefix of either marker —
			// if so, hold and wait for more bytes.
			if strings.HasPrefix(dsmlOpenPrefix, rest) || strings.HasPrefix(dsmlClosePrefix, rest) {
				f.pending.WriteString(rest)
				return out.String()
			}
			// Not a DSML marker at all. At depth 0 emit the `<`;
			// at depth > 0 just drop it. Advance one byte.
			if f.depth == 0 {
				out.WriteByte('<')
			}
			i++
			continue
		}

		// Confirmed an opener or closer prefix; find the tag's
		// closing `>`. If not present yet, hold the partial tag.
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			f.pending.WriteString(rest)
			return out.String()
		}
		if isCloser {
			if f.depth > 0 {
				f.depth--
			}
			// stray closer at depth 0: silently consumed.
		} else {
			f.depth++
		}
		i += end + 1
	}
	return out.String()
}

// Flush returns any held bytes at stream end. If we're still inside a
// block when the stream ends, drop the held bytes — a truncated DSML
// block is still leak text we don't want rendered.
func (f *dsmlFilter) Flush() string {
	if f.depth > 0 {
		f.pending.Reset()
		return ""
	}
	out := f.pending.String()
	f.pending.Reset()
	return out
}
