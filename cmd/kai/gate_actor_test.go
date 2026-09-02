package main

import (
	"testing"

	"github.com/kaicontext/kai-engine/workspace"
)

// The single most corrupting mistake this tally can make: filing an
// AGENT approval as a human one. Jeff resolves holds by shelling out to
// `kai gate approve` — the same command a person runs — so the only
// thing separating them is the environment kit exports.
func TestGateActorDistinguishesAgentFromHuman(t *testing.T) {
	t.Setenv("KAI_GATE_ACTOR", "")
	if got := gateActor(); got != workspace.ActorHuman {
		t.Errorf("bare terminal = %q, want %q", got, workspace.ActorHuman)
	}

	t.Setenv("KAI_GATE_ACTOR", workspace.ActorAgent)
	if got := gateActor(); got != workspace.ActorAgent {
		t.Errorf("agent env = %q, want %q — every agent approval would file as human", got, workspace.ActorAgent)
	}

	t.Setenv("KAI_GATE_ACTOR", workspace.ActorBackstop)
	if got := gateActor(); got != workspace.ActorBackstop {
		t.Errorf("backstop env = %q, want %q", got, workspace.ActorBackstop)
	}

	// Whitespace is not an actor.
	t.Setenv("KAI_GATE_ACTOR", "   ")
	if got := gateActor(); got != workspace.ActorHuman {
		t.Errorf("blank env = %q, want the human default", got)
	}
}
