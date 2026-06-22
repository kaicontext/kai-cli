package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CapExceededError is returned when kailab rejects a request
// because the user is over their per-user daily cost cap. The
// runner's retry classifier treats this as non-transient; the
// TUI uses the structured fields (cap amount, reset time, BYOM
// hint) to render an informative message instead of a generic
// 429 stack.
//
// Wire format mirrors kailab's spec §3 cap-exceeded body:
//
//	{
//	  "error": {
//	    "type": "daily_cost_cap_exceeded",
//	    "message": "Daily usage cap reached ($10.00). Resets at midnight UTC.",
//	    "daily_cost_usd": 10.23,
//	    "daily_cap_usd":  10.00,
//	    "resets_at":      "2026-05-05T00:00:00Z",
//	    "pricing_note":   null,
//	    "byom_hint":      "..."
//	  }
//	}
//
// We expose the parsed fields rather than re-formatting in the
// CLI so downstream callers (TUI, logs, future GUIs) can render
// the same data however they want.
type CapExceededError struct {
	// Message is kailab's human-readable line, e.g.
	// "Daily usage cap reached ($10.00). Resets at midnight UTC."
	// Safe to display verbatim — kailab generates it.
	Message string

	// DailyCostUSD / DailyCapUSD are the two halves of the
	// trailer-style "X.XX / Y.YY" usage display.
	DailyCostUSD float64
	DailyCapUSD  float64

	// ResetsAt is when the cap clears (next UTC midnight).
	// Zero-valued time when kailab sent something unparseable —
	// callers should fall back to the verbatim Message in that
	// case rather than displaying a misleading "0001-01-01".
	ResetsAt time.Time

	// PricingNote is non-empty when the model that triggered
	// the cap was priced via the unknown-model fallback rate.
	// Surfacing it lets the user understand "my cap blew up
	// faster than expected because we don't have pricing for
	// claude-mystery-9000."
	PricingNote string

	// BYOMHint is kailab's instructional copy for switching to a
	// personal Anthropic key. Intentionally instructional rather
	// than a copy-paste shell block — see spec §3 for the
	// rationale.
	BYOMHint string
}

func (e *CapExceededError) Error() string {
	if e.Message != "" {
		return "kailab: " + e.Message
	}
	return "kailab: daily cost cap exceeded"
}

// IsCapExceeded reports whether err (or any wrapped cause) is a
// CapExceededError. Use this in the runner / TUI rather than a
// type assertion so the check survives error-wrapping at any
// layer in between.
func IsCapExceeded(err error) bool {
	var ce *CapExceededError
	return errors.As(err, &ce)
}

// AsCapExceeded extracts the typed error from err so callers can
// read the structured fields without having to manage the type
// assertion themselves.
func AsCapExceeded(err error) (*CapExceededError, bool) {
	var ce *CapExceededError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// parseCapExceeded converts the wire JSON to CapExceededError.
// On a malformed body we still return a CapExceededError so the
// caller's IsCapExceeded check fires — the body is the only way
// kailab signals which 429 this is, but the kai-cap-exceeded
// header (which the caller already checked) is the contractual
// signal. Best-effort field parsing keeps us functional through
// minor schema drift.
func parseCapExceeded(body []byte) *CapExceededError {
	var raw struct {
		Error struct {
			Message      string  `json:"message"`
			DailyCostUSD float64 `json:"daily_cost_usd"`
			DailyCapUSD  float64 `json:"daily_cap_usd"`
			ResetsAt     string  `json:"resets_at"`
			PricingNote  string  `json:"pricing_note"`
			BYOMHint     string  `json:"byom_hint"`
		} `json:"error"`
	}
	out := &CapExceededError{}
	if err := json.Unmarshal(body, &raw); err == nil {
		out.Message = raw.Error.Message
		out.DailyCostUSD = raw.Error.DailyCostUSD
		out.DailyCapUSD = raw.Error.DailyCapUSD
		out.PricingNote = raw.Error.PricingNote
		out.BYOMHint = raw.Error.BYOMHint
		if t, perr := time.Parse(time.RFC3339, raw.Error.ResetsAt); perr == nil {
			out.ResetsAt = t
		}
	}
	if out.Message == "" {
		out.Message = "Daily usage cap reached. Resets at midnight UTC."
	}
	return out
}

// FormatHumanMessage renders the multi-line message the TUI
// shows when a request is blocked. Kept on the type so all CLI
// surfaces (TUI trailer, /status command, future error pages)
// produce the same wording.
func (e *CapExceededError) FormatHumanMessage() string {
	header := fmt.Sprintf("Daily kailab usage cap reached ($%.2f / $%.2f).",
		e.DailyCostUSD, e.DailyCapUSD)
	when := "Resets at midnight UTC."
	if !e.ResetsAt.IsZero() {
		until := time.Until(e.ResetsAt).Round(time.Minute)
		if until > 0 {
			when = fmt.Sprintf("Resets at midnight UTC (in ~%s).", humanDuration(until))
		}
	}
	hint := e.BYOMHint
	if hint == "" {
		hint = "To continue without limits, set ANTHROPIC_API_KEY in your shell and run `KAI_PROVIDER=anthropic kai`."
	}
	out := header + "\n" + when + "\n\n" + hint
	if e.PricingNote != "" {
		out += "\n\n(Note: " + e.PricingNote + ")"
	}
	return out
}

// humanDuration renders 4h23m / 47m / 23s — coarser than
// time.Duration's default String() so the trailer stays compact.
func humanDuration(d time.Duration) string {
	if d >= time.Hour {
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
