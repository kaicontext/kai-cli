package views

import (
	"kai/api/safetygate"
	"kai/api/util"
)

// gateHeldIDs returns the user-facing 12-char hex IDs of every
// snapshot the safety gate is currently holding. Used by the
// REPL's autocomplete to populate `/gate approve <here>`,
// `/gate reject <here>`, and `/gate show <here>`.
//
// Best-effort: returns nil on any error so the autocomplete
// silently degrades to no-suggestions instead of breaking the
// keystroke. The user can always paste an ID by hand.
func gateHeldIDs(s *PlannerServices) []string {
	if s == nil || s.DB == nil {
		return nil
	}
	held, err := safetygate.ListHeld(s.DB)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(held))
	for _, n := range held {
		hex := util.BytesToHex(n.ID)
		if len(hex) > 12 {
			hex = hex[:12]
		}
		out = append(out, hex)
	}
	return out
}
