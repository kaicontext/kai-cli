package projects

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-engine/graph"
	"kai/internal/safetygate"
)

// Open populates DB and GateConfig on every initialized project in
// the Set. Pinned-but-uninitialized projects (the user listed a
// sibling that doesn't have `.kai` yet) are left with nil DB and a
// default GateConfig — tools that route to them will skip
// graph-backed work for those paths.
//
// Callers must Close the Set to release the DB handles. On any open
// failure the partial state is rolled back (already-opened DBs are
// closed) so the caller never holds a half-populated Set.
func (s *Set) Open() error {
	if s == nil {
		return nil
	}
	opened := make([]*graph.DB, 0, len(s.projects))
	for _, p := range s.projects {
		if p.KaiDir == "" {
			// Defensive: Discover always sets this. A blank KaiDir
			// here means the project was hand-built by a test or
			// callers; default to the safetygate baseline and skip
			// graph open.
			p.GateConfig = safetygate.DefaultConfig()
			continue
		}
		// Gate config: load is best-effort. A missing file falls back
		// to defaults; a malformed file is an error worth surfacing
		// (the user would otherwise wonder why their gate rules
		// silently stopped applying).
		cfg, err := safetygate.LoadConfig(p.KaiDir)
		if err != nil {
			rollback(opened)
			return fmt.Errorf("loading gate config for %s: %w", p.Name, err)
		}
		p.GateConfig = cfg

		// Graph DB: only open if db.sqlite exists. A pinned but
		// uninitialized project has KaiDir set (kaipath always
		// resolves) but no DB on disk — leave DB nil so the tool
		// layer can skip graph queries cleanly.
		dbPath := filepath.Join(p.KaiDir, "db.sqlite")
		objPath := filepath.Join(p.KaiDir, "objects")
		db, err := graph.Open(dbPath, objPath)
		if err != nil {
			// "no such file" is the expected case for an
			// uninitialized pinned project; bubble up everything else
			// since opening should generally succeed when the file
			// exists.
			if isNotExistErr(err) {
				continue
			}
			rollback(opened)
			return fmt.Errorf("opening graph for %s: %w", p.Name, err)
		}
		p.DB = db
		opened = append(opened, db)
	}
	return nil
}

func rollback(dbs []*graph.DB) {
	for _, db := range dbs {
		_ = db.Close()
	}
}

// isNotExistErr matches both os.ErrNotExist and the SQLite "unable to
// open" message that graph.Open wraps. We can't import sqlite-error
// types without pulling the driver in here, so a substring check is
// the pragmatic option for the one case we care about.
func isNotExistErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sub := range []string{"no such file", "unable to open database", "does not exist"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}
