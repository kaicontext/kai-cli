package verify

import (
	"testing"

	"kai/internal/contract"
)

func TestParseSemantic(t *testing.T) {
	raw := `VERDICT: yes
NOTE: the diff adds a retry loop on 429 as the intent describes
RESIDUE:
- confirm the max-retry count (3) matches what you wanted`
	sem, residue := ParseSemantic(raw)
	if sem.Matches == nil || !*sem.Matches {
		t.Fatalf("expected matches=true, got %v", sem.Matches)
	}
	if sem.Note == "" {
		t.Error("expected a note")
	}
	if len(residue) != 1 {
		t.Fatalf("expected 1 residue question, got %v", residue)
	}
}

func TestParseSemantic_UnsureIsNil(t *testing.T) {
	sem, _ := ParseSemantic("VERDICT: unsure\nNOTE: the retry change isn't in this diff")
	if sem.Matches != nil {
		t.Fatalf("unsure must map to nil (not established), got %v", *sem.Matches)
	}
	sem2, _ := ParseSemantic("VERDICT: no\nNOTE: diff adds caching, not retry")
	if sem2.Matches == nil || *sem2.Matches {
		t.Fatalf("no must map to false, got %v", sem2.Matches)
	}
}

func boolp2(b bool) *bool { return &b }

func TestSemanticVerdict(t *testing.T) {
	pass := contract.CheckResult{TestsPass: boolp2(true)}
	fail := contract.CheckResult{TestsPass: boolp2(false)}

	// broken structural beats any semantic opinion
	if v := SemanticVerdict(fail, contract.SemanticResult{Matches: boolp2(true)}); v != contract.VerdictBroken {
		t.Errorf("failing tests must stay broken even with semantic match, got %v", v)
	}
	// structural clean + semantic match → verified (the only path to verified)
	if v := SemanticVerdict(pass, contract.SemanticResult{Matches: boolp2(true)}); v != contract.VerdictVerified {
		t.Errorf("expected verified, got %v", v)
	}
	// semantic unsure → stays clean_unconfirmed, never verified
	if v := SemanticVerdict(pass, contract.SemanticResult{Matches: nil}); v != contract.VerdictCleanUnconfirmed {
		t.Errorf("unsure must stay clean_unconfirmed, got %v", v)
	}
	// semantic no → clean_unconfirmed (surfaced via residue, not a false verified)
	if v := SemanticVerdict(pass, contract.SemanticResult{Matches: boolp2(false)}); v != contract.VerdictCleanUnconfirmed {
		t.Errorf("semantic-no must not be verified, got %v", v)
	}
}
