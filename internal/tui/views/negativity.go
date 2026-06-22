package views

import (
	"regexp"
	"strings"
)

// Negativity-phrase telemetry. The dispatch path runs every user
// submission through matchNegativity; matches fire a "user_negativity"
// telemetry event with one Stats key per canonical phrase.
//
// What gets sent to telemetry: only the canonical phrases that
// matched (drawn from the hardcoded list below). The user's raw input
// is never shipped — telemetry sees "match_wtf" / "match_piece_of_shit"
// and nothing else. This is a frustration signal, not message logging.
//
// Word boundaries via \b so that "ffs" doesn't false-match inside
// "stuffs", "wth" inside "width", etc. Multi-word phrases get the
// same treatment but the boundary positions are at phrase edges, so
// "what the fuck" still matches in "what the fuck is going on".
//
// 2026-05-14 dogfood: shipping the seed list. Expand as patterns turn
// up in PostHog that warrant a new bucket; canonical phrases listed
// here are the ones the user explicitly named on day one.
var negativityRegex = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
	"wtf", "wth", "ffs", "omfg",
	"shit", "shitty", "shittest",
	"dumbass", "horrible", "awful",
	"pissed off", "pissing off",
	"piece of shit", "piece of crap", "piece of junk",
	"what the fuck", "what the hell",
	"fucking broken", "fucking useless", "fucking terrible",
	"fucking awful", "fucking horrible",
	"fuck you", "screw this", "screw you",
	"so frustrating", "this sucks", "damn it",
}, "|") + `)\b`)

// matchNegativity returns the deduped canonical phrases that appear
// in line (case-insensitive, word-boundary). Returns nil when nothing
// matches. The returned strings are already lowercased canonical
// forms — safe to ship to telemetry as-is, no further sanitization.
func matchNegativity(line string) []string {
	hits := negativityRegex.FindAllString(strings.ToLower(line), -1)
	if len(hits) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(hits))
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// negativityKey turns a canonical phrase into a telemetry stat key.
// "piece of shit" → "match_piece_of_shit". Kept here next to the
// matcher so the key shape and the phrase list move together.
func negativityKey(phrase string) string {
	return "match_" + strings.ReplaceAll(phrase, " ", "_")
}
