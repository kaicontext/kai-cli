package invariant

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// addMemberRule mirrors the old-kai invariant: a handler that adds a
// member must recompute the org's caps.
var addMemberRule = Rule{Trigger: "AddMember", Require: "recomputeCapsForOrg"}

const membersSrc = `package api

// goodHandler adds a member and recomputes caps — satisfies the rule.
func goodHandler() {
	h.db.AddMember(orgID, userID, role)
	h.recomputeCapsForOrg(orgID)
}

// badHandler adds a member but never recomputes — the violation.
func badHandler() {
	h.db.AddMember(orgID, userID, role)
	writeJSON(w, 201, resp)
}

// AddMember is the definition itself; it must not flag itself.
func (db *DB) AddMember(orgID, userID, role string) error {
	return db.exec(query, userID, orgID, role)
}

func unrelated() {
	doSomethingElse()
}
`

func TestCheck_FlagsMissingRequire(t *testing.T) {
	root := writeTree(t, map[string]string{"internal/api/orgs.go": membersSrc})
	vios, err := Check(root, []Rule{addMemberRule})
	if err != nil {
		t.Fatal(err)
	}
	if len(vios) != 1 {
		t.Fatalf("expected exactly 1 violation, got %d: %+v", len(vios), vios)
	}
	if vios[0].Func != "badHandler" {
		t.Errorf("expected badHandler flagged, got %q", vios[0].Func)
	}
	if vios[0].File != "internal/api/orgs.go" || vios[0].Line == 0 {
		t.Errorf("expected a real file:line, got %s:%d", vios[0].File, vios[0].Line)
	}
}

func TestCheck_SuppressComment(t *testing.T) {
	src := `package api

// seedBulk loads fixtures and intentionally skips the recompute.
// kit:invariant-ok bulk seed, caps recomputed once at the end
func seedBulk() {
	h.db.AddMember(a, b, c)
}
`
	root := writeTree(t, map[string]string{"seed.go": src})
	vios, err := Check(root, []Rule{addMemberRule})
	if err != nil {
		t.Fatal(err)
	}
	if len(vios) != 0 {
		t.Errorf("suppress marker must opt the function out, got %+v", vios)
	}
}

func TestCheck_SkipsTestFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"orgs_test.go": "package api\n\nfunc TestX() { h.db.AddMember(a, b, c) }\n",
	})
	vios, err := Check(root, []Rule{addMemberRule})
	if err != nil {
		t.Fatal(err)
	}
	if len(vios) != 0 {
		t.Errorf("_test.go files must be skipped, got %+v", vios)
	}
}

// TestCheck_FlagsHandlerSharingTriggerName is the regression guard for
// the real old-kai case: the HTTP handler is itself named AddMember and
// calls the DB AddMember without recomputing — it must still be flagged
// (the checker keys on calls, not on the function's own name).
func TestCheck_FlagsHandlerSharingTriggerName(t *testing.T) {
	src := `package api

func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request) {
	h.db.AddMember(orgID, userID, role)
	writeJSON(w, 201, resp)
}
`
	root := writeTree(t, map[string]string{"api/orgs.go": src})
	vios, err := Check(root, []Rule{addMemberRule})
	if err != nil {
		t.Fatal(err)
	}
	if len(vios) != 1 || vios[0].Func != "AddMember" {
		t.Fatalf("a handler sharing the trigger name must be flagged, got %+v", vios)
	}
}

func TestCheck_NoRulesNoViolations(t *testing.T) {
	root := writeTree(t, map[string]string{"orgs.go": membersSrc})
	vios, err := Check(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vios) != 0 {
		t.Errorf("no rules → no violations, got %+v", vios)
	}
}
