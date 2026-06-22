package contract

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists contracts in the shared kai db.sqlite, alongside the graph
// and activity stores. It opens its own connection (WAL-mode) so the kai CLI
// and kit can read/write the same contracts concurrently.
type Store struct {
	db *sql.DB
}

func nowMs() int64 { return time.Now().UnixMilli() }

// Open opens and migrates the contract store rooted at kaiDir (the resolved
// .kai / .git/kai directory).
func Open(kaiDir string) (*Store, error) {
	conn, err := sql.Open("sqlite", filepath.Join(kaiDir, "db.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("opening contract store: %w", err)
	}
	conn.Exec("PRAGMA journal_mode=WAL")
	conn.Exec("PRAGMA busy_timeout=5000")
	s := &Store{db: conn}
	if err := s.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS contracts (
  id         TEXT PRIMARY KEY,
  json       TEXT NOT NULL,
  status     TEXT NOT NULL,
  updated_at INTEGER NOT NULL
)`); err != nil {
		return err
	}
	// verify_state holds the daemon's latest global deterministic result — the
	// tree-wide structural verdict that backs `no-intent` and the daemon
	// liveness line in `kai status`.
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS verify_state (
  key        TEXT PRIMARY KEY,
  json       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
)`)
	return err
}

const structuralKey = "structural"

// SaveStructural records the latest tree-wide deterministic CheckResult.
func (s *Store) SaveStructural(cr CheckResult) error {
	b, err := json.Marshal(cr)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO verify_state (key, json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
		structuralKey, string(b), nowMs())
	return err
}

// GetStructural returns the latest tree-wide deterministic CheckResult, with
// ok=false if the daemon has never run.
func (s *Store) GetStructural() (CheckResult, bool, error) {
	var js string
	err := s.db.QueryRow(`SELECT json FROM verify_state WHERE key = ?`, structuralKey).Scan(&js)
	if err == sql.ErrNoRows {
		return CheckResult{}, false, nil
	}
	if err != nil {
		return CheckResult{}, false, err
	}
	var cr CheckResult
	if err := json.Unmarshal([]byte(js), &cr); err != nil {
		return CheckResult{}, false, err
	}
	return cr, true, nil
}

// Close releases the store's connection.
func (s *Store) Close() error { return s.db.Close() }

// Create opens a new contract for the given statement and source, allocating a
// stable human-readable slug id. A fresh contract is drifting (never verified).
func (s *Store) Create(statement string, source Source) (*Contract, error) {
	id, err := s.uniqueSlug(statement)
	if err != nil {
		return nil, err
	}
	now := nowMs()
	c := &Contract{
		ID:         id,
		Statement:  strings.TrimSpace(statement),
		Status:     VerdictDrifting,
		Source:     source,
		Provenance: []Event{},
		Residue:    []ResidueItem{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	// Every contract's provenance opens with the intent that created it.
	c.AddEvent(Event{Ref: "i1", Text: c.Statement, Kind: EventIntent, TS: now})
	if err := s.Save(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Save upserts a contract, stamping UpdatedAt.
func (s *Store) Save(c *Contract) error {
	c.UpdatedAt = nowMs()
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling contract: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO contracts (id, json, status, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET json = excluded.json, status = excluded.status, updated_at = excluded.updated_at`,
		c.ID, string(b), string(c.Status), c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("saving contract: %w", err)
	}
	return nil
}

// Get returns the contract with the given id, or (nil, nil) if absent.
func (s *Store) Get(id string) (*Contract, error) {
	var js string
	err := s.db.QueryRow(`SELECT json FROM contracts WHERE id = ?`, id).Scan(&js)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying contract: %w", err)
	}
	return decode(js)
}

// List returns all contracts, most recently updated first.
func (s *Store) List() ([]*Contract, error) {
	rows, err := s.db.Query(`SELECT json FROM contracts ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing contracts: %w", err)
	}
	defer rows.Close()
	var out []*Contract
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return nil, err
		}
		c, err := decode(js)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Drop removes a contract. Returns an error if it doesn't exist.
func (s *Store) Drop(id string) error {
	res, err := s.db.Exec(`DELETE FROM contracts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dropping contract: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("contract %q not found", id)
	}
	return nil
}

func decode(js string) (*Contract, error) {
	var c Contract
	if err := json.Unmarshal([]byte(js), &c); err != nil {
		return nil, fmt.Errorf("decoding contract: %w", err)
	}
	return &c, nil
}

var slugSep = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a statement into a short kebab-case id (first few meaningful
// words). The exact form isn't load-bearing — uniqueness is handled separately.
func slugify(statement string) string {
	s := slugSep.ReplaceAllString(strings.ToLower(statement), "-")
	s = strings.Trim(s, "-")
	var kept []string
	for _, p := range strings.Split(s, "-") {
		if p == "" || stopwords[p] {
			continue
		}
		kept = append(kept, p)
		if len(kept) >= 4 {
			break
		}
	}
	if len(kept) == 0 {
		return "contract"
	}
	return strings.Join(kept, "-")
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "on": true, "in": true,
	"of": true, "for": true, "and": true, "or": true, "it": true, "is": true,
}

// uniqueSlug returns a slug not already in use, suffixing -2, -3, ... on collision.
func (s *Store) uniqueSlug(statement string) (string, error) {
	base := slugify(statement)
	id := base
	for i := 2; i < 10000; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM contracts WHERE id = ?`, id).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return id, nil
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("could not allocate a unique slug for %q", statement)
}
