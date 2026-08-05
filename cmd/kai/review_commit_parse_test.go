package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/finding"
)

// The reviewer is told to finish with EXACTLY this block. It mostly does — and
// when it doesn't, the verdict used to be dropped on the floor. Every case below
// was reachable against the old parser and produced MatchUnknown, which the
// console renders as "no contract resolved for this change" even though the
// reviewer had, in fact, resolved it.
func TestRCParseReviewOutput_VerdictShapes(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantMatch finding.Match
	}{
		{"canonical", "INTENT_MATCH: verified", finding.MatchVerified},
		{"bold key and value", "**INTENT_MATCH:** verified", finding.MatchVerified},
		{"backticked key", "`INTENT_MATCH:` verified", finding.MatchVerified},
		{"trailing period", "INTENT_MATCH: verified.", finding.MatchVerified},
		{"trailing justification", "INTENT_MATCH: verified — no material gaps", finding.MatchVerified},
		{"prompt's own gloss", "INTENT_MATCH: matches", finding.MatchVerified},
		{"lowercase key", "intent_match: verified", finding.MatchVerified},
		{"partial", "INTENT_MATCH: partial", finding.MatchPartial},
		{"diverges", "INTENT_MATCH: diverges", finding.MatchDiverges},
		// A justification is the natural way to write a non-verified verdict, and
		// "matches" is the natural verb for it. Reading past the first word turns
		// both of these into unknown — the dropped verdict this parser exists to
		// prevent.
		{"justification naming another verdict", "INTENT_MATCH: diverges — the code no longer matches the stated intent", finding.MatchDiverges},
		{"partial with justification", "INTENT_MATCH: partial — the refactor matches the described behavior but the tests were not updated", finding.MatchPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "FINDINGS:\n" + tt.line + "\nNOTE: looks fine"
			_, match, _ := rcParseReviewOutput(raw)
			if match != tt.wantMatch {
				t.Errorf("match = %q, want %q (line %q)", match, tt.wantMatch, tt.line)
			}
		})
	}
}

// An unreadable or absent verdict must stay unknown. The caller logs the raw
// closing text whenever it is, which is what tells a human which of the two
// happened — silently recording a verdict nobody stated is the defect.
func TestRCParseReviewOutput_UnreadableVerdictStaysUnknown(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no verdict line at all", "FINDINGS:\n- [bug] a.go:1 — thing\nNOTE: hmm"},
		{"unrecognized value", "INTENT_MATCH: probably-fine\nNOTE: hmm"},
		{"empty value", "INTENT_MATCH:\nNOTE: hmm"},
		{"empty output", ""},
		// A negated verdict is the reviewer saying the opposite of the word it
		// contains. Reading past the first word records "verified" for a change
		// the reviewer just said was not verified.
		{"negated", "INTENT_MATCH: not verified"},
		{"negated, modal", "INTENT_MATCH: cannot be verified"},
		// Two verdict lines that disagree. Recording the last one makes the result
		// depend on emission order; the model quoting the block back as an example
		// is enough to trigger it.
		{"two lines disagree", "INTENT_MATCH: verified\nINTENT_MATCH: diverges"},
		{"two lines disagree, reversed", "INTENT_MATCH: diverges\nINTENT_MATCH: verified"},
		{"quoted example after the real verdict", "INTENT_MATCH: verified\n`INTENT_MATCH: diverges`"},
		{"bolded example after the real verdict", "INTENT_MATCH: verified\n**INTENT_MATCH: diverges**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, match, _ := rcParseReviewOutput(tt.raw)
			if match != finding.MatchUnknown {
				t.Errorf("match = %q, want unknown", match)
			}
		})
	}
}

// Nothing outside a real INTENT_MATCH line may produce a verdict. Every case here
// contains a verdict synonym in ordinary prose — "matches" is a common English
// word, and an "Intent:" recap heading is a natural thing for a reviewer to
// write. Reading any of them fabricates a verdict AND suppresses the warning that
// would have exposed it.
func TestRCParseReviewOutput_ProseNeverBecomesAVerdict(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"note mentioning intent mid-sentence",
			"NOTE: Confirmed the author's intent: the refactor matches the described behavior with no regressions."},
		{"finding about intent-named code",
			"FINDINGS:\n- [bug] intent_flag.go:5 — the verified flag is never reset\nNOTE: hmm"},
		{"prose naming intent and a verdict",
			"The change is intentional - it matches the original spec.\nNOTE: fine"},
		{"intent recap heading",
			"Intent: harden the parser so drifted formatting still matches a known verdict.\nFINDINGS:\n- [bug] a.go:1 — x"},
		{"drifted key is not a verdict",
			"INTENT MATCH: verified"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, match, _ := rcParseReviewOutput(tt.raw)
			if match != finding.MatchUnknown {
				t.Errorf("match = %q, want unknown — a verdict was fabricated from non-verdict text", match)
			}
		})
	}
}

// A recap line the reviewer wrote BEFORE its real verdict must never override it.
func TestRCParseReviewOutput_ProseDoesNotOverrideTheStatedVerdict(t *testing.T) {
	raw := strings.Join([]string{
		"Intent: the change matches the described refactor.",
		"",
		"FINDINGS:",
		"- [bug] a.go:1 — thing",
		"INTENT_MATCH: partial",
		"NOTE: gaps",
	}, "\n")
	_, match, _ := rcParseReviewOutput(raw)
	if match != finding.MatchPartial {
		t.Errorf("match = %q, want partial — the reviewer's own verdict was overridden", match)
	}
}

// A reviewer that quotes an example FINDINGS block while explaining itself must
// not have those bullets stored as defects. Observed in a real run: 3 findings
// recorded, 2 of them quoted from this command's own test fixtures.
func TestRCParseReviewOutput_QuotedFindingsBlockDoesNotLeak(t *testing.T) {
	raw := strings.Join([]string{
		"The test file contains fixtures like:",
		"FINDINGS:",
		"- [bug] a.go:1 — thing",
		"- [bug] a.go:1 — thing",
		"",
		"FINDINGS:",
		"- [correctness] review_commit.go:373 — the real defect",
		"INTENT_MATCH: partial",
		"NOTE: ok",
	}, "\n")
	risks, match, _ := rcParseReviewOutput(raw)
	want := []string{"[correctness] review_commit.go:373 — the real defect"}
	if !slices.Equal(risks, want) {
		t.Errorf("risks = %q, want %q — quoted example bullets leaked in", risks, want)
	}
	if match != finding.MatchPartial {
		t.Errorf("match = %q, want partial", match)
	}
}

// The FINDINGS list ends at the first non-bullet line. A closing block the model
// spelled differently used to leave the parser inside FINDINGS, so its trailing
// prose was stored as a defect with Verified: true.
func TestRCParseReviewOutput_FindingsListEndsAtNonBullet(t *testing.T) {
	raw := strings.Join([]string{
		"FINDINGS:",
		"- [bug] a.go:1 — x",
		"INTENT MATCH: verified",
		"- I also confirmed the callers were updated",
		"NOTE: hm",
	}, "\n")
	risks, _, _ := rcParseReviewOutput(raw)
	if len(risks) != 1 {
		t.Errorf("risks = %v, want exactly the one real defect", risks)
	}
}

// The accepted vocabulary is closed, and every entry must appear VERBATIM in the
// prompt this command sends or in the finding.Match enum. This test pins both
// directions so the set cannot grow by guesswork — it grows only when the
// verdictRead logging shows a real run producing a spelling we rejected.
func TestRCVerdictSynonyms_EveryEntryIsAttestedInThePrompt(t *testing.T) {
	want := map[string]finding.Match{
		"verified": finding.MatchVerified,
		"matches":  finding.MatchVerified,
		"partial":  finding.MatchPartial,
		"diverges": finding.MatchDiverges,
	}
	if len(rcVerdictSynonyms) != len(want) {
		t.Errorf("accepted vocabulary has %d entries, want %d — a new entry needs the prompt or the enum behind it",
			len(rcVerdictSynonyms), len(want))
	}
	for k, v := range want {
		if got, ok := rcVerdictSynonyms[k]; !ok || got != v {
			t.Errorf("rcVerdictSynonyms[%q] = %q,%v; want %q", k, got, ok, v)
		}
		// The authority itself: the spelling must be a WORD in the prompt the model
		// reads, or be the enum value. Substring matching would accept any fragment
		// of the ~3 KB prompt — "verif", "part" and "diverg" all appear in it — so
		// the check has to be whole-word to mean anything.
		if !slices.Contains(rcVerdictWords(rcReviewSystem), k) && string(v) != k {
			t.Errorf("%q is accepted but appears nowhere in the review prompt — it has no authority", k)
		}
	}
	// Plausible but unattested. "match" and "diverge" are here deliberately:
	// kai-engine's judgeIntent accepts them, but no prompt in this codebase emits
	// them, so they are that parser's guesses and must not be inherited as fact.
	for _, w := range []string{"match", "diverge", "partially", "divergent", "diverged", "verify", "ok", "pass", "yes"} {
		if _, ok := rcVerdictSynonyms[w]; ok {
			t.Errorf("%q is accepted but unattested — add it to the prompt first, or wait for a logged run that produces it", w)
		}
	}
}

// Prose must never invent a verdict. "a confidently wrong verified poisons the
// whole proposition" — salvage only looks at lines mentioning INTENT.
func TestRCSalvageVerdict_DoesNotInventFromProse(t *testing.T) {
	raw := "The tests partially cover this.\nThe implementation matches the docs.\nNOTE: fine"
	_, match, _ := rcParseReviewOutput(raw)
	if match != finding.MatchUnknown {
		t.Errorf("match=%q — prose must not produce a verdict", match)
	}
}

// Findings and note extraction must survive the decoration handling.
func TestRCParseReviewOutput_FindingsAndNote(t *testing.T) {
	raw := strings.Join([]string{
		"FINDINGS:",
		"- [security] api/auth.go:42 — compares secrets with == instead of a constant-time compare",
		"- [correctness] db/user.go:10 — scans a nullable column into a non-nullable string",
		"**INTENT_MATCH:** partial",
		"**NOTE:** mostly good, two gaps",
	}, "\n")
	risks, match, note := rcParseReviewOutput(raw)
	if len(risks) != 2 {
		t.Fatalf("risks = %d, want 2: %v", len(risks), risks)
	}
	if !strings.Contains(risks[0], "constant-time") {
		t.Errorf("first risk mangled: %q", risks[0])
	}
	if match != finding.MatchPartial {
		t.Errorf("match=%q, want partial", match)
	}
	if note != "mostly good, two gaps" {
		t.Errorf("note = %q", note)
	}
}

// A finding the model wrapped in bold is still a finding. Dropping it produced a
// clean-looking review with a real defect missing from it, and nothing anywhere
// said so.
func TestRCParseReviewOutput_DecoratedFindingsSurvive(t *testing.T) {
	raw := strings.Join([]string{
		"FINDINGS:",
		"**- [security] a.go:1 — compares a secret with == instead of a constant-time compare**",
		"- [correctness] b.go:2 — off-by-one in the refill math",
		"INTENT_MATCH: verified",
	}, "\n")
	risks, match, _ := rcParseReviewOutput(raw)
	if len(risks) != 2 {
		t.Fatalf("risks = %d, want 2 — a decorated finding was dropped: %v", len(risks), risks)
	}
	if !strings.Contains(risks[0], "[security]") || strings.Contains(risks[0], "*") {
		t.Errorf("decorated finding not unwrapped cleanly: %q", risks[0])
	}
	if match != finding.MatchVerified {
		t.Errorf("match = %q, want verified", match)
	}
}

// Decoration INSIDE a note is content, not markup to be deleted. Stripping every
// * and ` rewrote the reviewer's assessment on its way to the console: `a*b`
// silently became "ab", a different token entirely.
func TestRCParseReviewOutput_NotePreservesInlineCode(t *testing.T) {
	tests := []struct {
		name, line, want string
	}{
		{"inline code mid-note",
			"NOTE: check `a*b` overflow, uses `os.Getenv` internally",
			"check `a*b` overflow, uses `os.Getenv` internally"},
		// The wrapper is the token the LINE opened with. An undecorated note that
		// merely STARTS with inline code has no wrapper to remove.
		{"note starts with inline code",
			"NOTE: `os.Getenv` is read twice per request",
			"`os.Getenv` is read twice per request"},
		// ...and a bold key does not make a trailing backtick into a wrapper.
		{"bold key, note ends with inline code",
			"**NOTE:** the fix matches `handleFoo`",
			"the fix matches `handleFoo`"},
		{"whole line bold",
			"**NOTE: mostly good, two gaps**",
			"mostly good, two gaps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, note := rcParseReviewOutput("INTENT_MATCH: partial\n" + tt.line)
			if note != tt.want {
				t.Errorf("note = %q, want %q", note, tt.want)
			}
		})
	}
}

// The same rule for findings: decoration inside a defect statement is content.
func TestRCParseReviewOutput_FindingsPreserveInlineCode(t *testing.T) {
	raw := strings.Join([]string{
		"FINDINGS:",
		"- `hmac.Equal` should be used instead of ==",
		"**- [security] a.go:1 — uses `==` on `hmac`**",
		"INTENT_MATCH: verified",
	}, "\n")
	risks, _, _ := rcParseReviewOutput(raw)
	want := []string{
		"`hmac.Equal` should be used instead of ==",
		"[security] a.go:1 — uses `==` on `hmac`",
	}
	if !slices.Equal(risks, want) {
		t.Errorf("risks = %q, want %q", risks, want)
	}
}

func TestRCTail(t *testing.T) {
	if got := rcTail("abcdef", 3); got != "def" {
		t.Errorf("rcTail = %q, want %q", got, "def")
	}
	if got := rcTail("abc", 10); got != "abc" {
		t.Errorf("rcTail = %q, want %q", got, "abc")
	}
	// Must not split a multi-byte rune. "aéb" is 4 bytes (é is two), so a 2-byte
	// tail starts on é's continuation byte and has to advance to the next
	// boundary; a 3-byte tail is already aligned and keeps the é.
	if got := rcTail("aéb", 2); got != "b" {
		t.Errorf("rcTail = %q, want %q — split a rune", got, "b")
	}
	if got := rcTail("aéb", 3); got != "éb" {
		t.Errorf("rcTail = %q, want %q", got, "éb")
	}
	// A computed width can go non-positive; slicing on it would panic.
	if got := rcTail("abc", 0); got != "" {
		t.Errorf("rcTail(_, 0) = %q, want empty", got)
	}
	if got := rcTail("abc", -1); got != "" {
		t.Errorf("rcTail(_, -1) = %q, want empty", got)
	}
}

// snake_case identifiers in the note must survive — an earlier draft stripped _
// as decoration and turned review_findings.go into reviewfindings.go.
func TestRCParseReviewOutput_NotePreservesUnderscores(t *testing.T) {
	_, _, note := rcParseReviewOutput("INTENT_MATCH: verified\nNOTE: verify_ingest_secret in review_findings.go is correct")
	if !strings.Contains(note, "verify_ingest_secret") || !strings.Contains(note, "review_findings.go") {
		t.Errorf("note lost underscores: %q", note)
	}
}
