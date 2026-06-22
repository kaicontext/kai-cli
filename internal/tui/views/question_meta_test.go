package views

import "testing"

// TestIsQuestion_MetaDiscussionOverridesActionVerb covers the
// 2026-05-15 dogfood case: a long meta-discussion prompt
// containing action verbs as content ("update version numbers",
// "write an MD file", "make sure X stays simple") routed to the
// planner because the action-verb check tripped. The meta-
// discussion override now catches it: long prompts ending with
// "?" and containing meta markers ("the agent", "is there a way",
// etc.) are classified as questions even when action verbs are
// present.
func TestIsQuestion_MetaDiscussionOverridesActionVerb(t *testing.T) {
	cases := map[string]bool{
		// The actual prompt that triggered the 6m26s planner explosion.
		"Theres a lot of times you as the agent need to do a simple task, like update version numbers, or write an MD file. But at the moment you turn it into a big deal and make a mess. Writing a design doc shouldn't spawn multiple agents and take 10 minutes to execute. The planning takes a long time and that's ok, but is there a way to make sure simple tasks stay simple?": true,

		// Other meta-discussion patterns we want routed to chat.
		"The agent seems to spend too long exploring. Why does this happen on every task and how do we make it stop?": true,
		"You as the agent keep introducing testify when the file uses t.Errorf. Why does that happen? Can we change it?": true,
		"Is there a way to make the planner skip exploration when the request is a simple single-file edit?":             true,

		// Negatives: a SHORT prompt with action verb is still a request,
		// even if it contains "the agent". The 120-char floor on
		// isMetaDiscussion prevents over-routing on these.
		"the agent should add a /health endpoint": false,
		"make the planner faster":                 false,
	}
	for in, want := range cases {
		if got := isQuestion(in); got != want {
			t.Errorf("isQuestion(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestIsMetaDiscussion_Markers covers the helper directly so the
// marker list can't be silently shrunk in a future refactor.
func TestIsMetaDiscussion_Markers(t *testing.T) {
	longEnough := func(s string) string {
		for len(s) < 90 {
			s += " more context to make this realistically long"
		}
		return s
	}

	markers := []string{
		"the agent did X", "the planner spent Y",
		"the system loops here", "the tui takes forever",
		"the worker thrashes a lot", "the model returned no text",
		"you as the agent need to handle this",
		"is there a way to skip exploration",
		"any way to make tasks shorter",
		"is it possible to disable this",
		"how come planning is slow",
		"do we know why builds fail",
		"what controls the agent count",
	}
	for _, m := range markers {
		if !isMetaDiscussion(longEnough(m)) {
			t.Errorf("isMetaDiscussion missed marker %q", m)
		}
	}

	// Short prompts (under 120 chars) MUST NOT trigger — they're
	// almost always real imperatives.
	short := "is there a way to add a flag"
	if isMetaDiscussion(short) {
		t.Errorf("short prompt should not be meta-discussion: %q", short)
	}
}
