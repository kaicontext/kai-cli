package contract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	c, err := s.Create("rate-limit retry on sync client", SourceKit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected a slug id")
	}
	if c.Status != VerdictDrifting {
		t.Errorf("new contract should be drifting, got %q", c.Status)
	}
	if c.Source != SourceKit {
		t.Errorf("source = %q", c.Source)
	}

	// Reload and verify persistence.
	got, err := s.Get(c.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Statement != "rate-limit retry on sync client" {
		t.Errorf("statement round-trip: %q", got.Statement)
	}
	// Provenance opens with the contract's creating intent.
	if len(got.Provenance) != 1 || got.Provenance[0].Kind != EventIntent || !got.Provenance[0].Applied {
		t.Fatalf("expected opening intent event, got %+v", got.Provenance)
	}

	// Fold a refinement into provenance + statement, save, reload.
	got.AddEvent(Event{Ref: "p3", Text: "add backoff too", Kind: EventRefinement, TS: 1})
	got.Statement = "rate-limit retry with backoff on sync client"
	got.Status = VerdictCleanUnconfirmed
	if err := s.Save(got); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, _ := s.Get(c.ID)
	if len(reloaded.Provenance) != 2 {
		t.Fatalf("expected opening intent + refinement, got %+v", reloaded.Provenance)
	}
	if ref := reloaded.Provenance[1]; ref.Kind != EventRefinement || !ref.Applied {
		t.Fatalf("refinement should be recorded + applied, got %+v", ref)
	}
	if reloaded.Status != VerdictCleanUnconfirmed {
		t.Errorf("status not persisted: %q", reloaded.Status)
	}

	// A question event must NOT apply to the contract.
	reloaded.AddEvent(Event{Ref: "p4", Text: "what does it do now?", Kind: EventQuestion, TS: 2})
	if reloaded.Provenance[2].Applied {
		t.Error("question event must not apply to the contract")
	}
}

func TestStore_SlugUniqueness(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, _ := s.Create("paginate the workspace list", SourceTraced)
	b, _ := s.Create("paginate the workspace list", SourceTraced)
	if a.ID == b.ID {
		t.Fatalf("duplicate statements must get distinct ids, both = %q", a.ID)
	}
}

func TestStore_ListAndDrop(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c1, _ := s.Create("red reset button on settings page", SourceKit)
	s.Create("paginate the workspace list", SourceKit)

	all, err := s.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v, n=%d", err, len(all))
	}

	if err := s.Drop(c1.ID); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if got, _ := s.Get(c1.ID); got != nil {
		t.Error("contract should be gone after drop")
	}
	if err := s.Drop("nonexistent"); err == nil {
		t.Error("drop of missing contract should error")
	}
}

// The store must use the shared db.sqlite (what kit also opens), not a private file.
func TestStore_UsesSharedDB(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()
	s.Create("x", SourceKit)
	if _, err := os.Stat(filepath.Join(dir, "db.sqlite")); err != nil {
		t.Fatalf("expected shared db.sqlite at %s: %v", dir, err)
	}
}
