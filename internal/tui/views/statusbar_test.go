package views

import (
	"testing"

	"kai/api/graph"
)

// TestStatusBar_GateCountIsRefreshAuthoritative locks down the
// status-bar half of the gate-count mismatch: the held counter must
// reflect ONLY the DB-grounded GateRefreshedMsg. A mid-stream "gate"
// chat-activity event must not move it — an optimistic increment that
// lands after the authoritative refresh leaves the bar stuck (the bar
// showed "1 held" while `/gate` — a fresh ListHeld — showed nothing).
func TestStatusBar_GateCountIsRefreshAuthoritative(t *testing.T) {
	var s StatusBar

	// A refresh carrying one held item sets the count.
	s = s.Update(GateRefreshedMsg{items: []*graph.Node{{}}})
	if s.gateHeld != 1 {
		t.Fatalf("GateRefreshedMsg should set gateHeld=1, got %d", s.gateHeld)
	}

	// A 'gate' chat-activity event must NOT change the count — only
	// the DB-grounded refresh is authoritative.
	s = s.Update(ChatActivityMsg{Event: ChatActivityEvent{Kind: "gate", GateVerdict: "review"}})
	if s.gateHeld != 1 {
		t.Errorf("gate chat-activity event must not move the counter, got %d", s.gateHeld)
	}

	// Once the held item is resolved a refresh zeroes the count, and a
	// later stale 'gate' event must leave it at 0 — not bump it back.
	s = s.Update(GateRefreshedMsg{items: nil})
	s = s.Update(ChatActivityMsg{Event: ChatActivityEvent{Kind: "gate", GateVerdict: "block"}})
	if s.gateHeld != 0 {
		t.Errorf("stale gate event after a zeroing refresh must keep gateHeld=0, got %d", s.gateHeld)
	}
}

// TestStatusBar_AgentCountStillTracks confirms removing the gate
// optimistic-increment didn't disturb the agent_start/agent_end
// counting that shares the same ChatActivityMsg switch.
func TestStatusBar_AgentCountStillTracks(t *testing.T) {
	var s StatusBar
	s = s.Update(ChatActivityMsg{Event: ChatActivityEvent{Kind: "agent_start"}})
	s = s.Update(ChatActivityMsg{Event: ChatActivityEvent{Kind: "agent_start"}})
	if s.agentsActive != 2 {
		t.Fatalf("two agent_start events should give agentsActive=2, got %d", s.agentsActive)
	}
	s = s.Update(ChatActivityMsg{Event: ChatActivityEvent{Kind: "agent_end"}})
	if s.agentsActive != 1 {
		t.Errorf("agent_end should decrement to 1, got %d", s.agentsActive)
	}
}
