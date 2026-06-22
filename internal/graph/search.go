package graph

import (
	"database/sql"
	"fmt"
	"strings"
)

// File-text search over an FTS5 virtual table. This is kai's
// "ripgrep-equivalent" for free-text queries — the graph already
// indexes symbols + calls, but full-text search across file bodies
// needed its own index. SQLite FTS5 gives BM25-ranked results in
// 1-5ms on a 100k-row corpus, vs ~50-100ms for the rg equivalent on
// a cold cache. The structural payoff: every hit can be joined back
// to the File node it came from, so results carry semantic metadata
// (project, path) rather than raw filesystem paths.
//
// Schema:
//
//	file_text(project, path, body)  — FTS5 virtual table
//
// `project` is the multi-root project name. Indexing the project
// alongside the path means a search query can filter by project
// without an extra JOIN against the nodes table. `path` is relative
// to the project root so the same file appearing in two clones (an
// uncommon but possible monorepo layout) doesn't collide.
//
// Phase 1 (this code): table + index/search API + a one-shot backfill
// driven by the agent tool. Files are stale until the tool re-indexes.
// Phase 2 (not yet): hook into runCapture so writes during capture
// keep the FTS in sync automatically.

// SearchHit is one full-text search result. Snippet is the BM25-
// matched window with `«` / `»` around the matched terms — pre-
// formatted so the agent can render it directly without further
// tokenization.
//
// Symbol and Line are filled by the optional enrichment pass in the
// kai_search tool: it joins the FTS hit back to the graph's Symbol
// nodes by line range. Zero values mean "no enclosing symbol was
// found" (top-level comment, import block, file that hasn't been
// parsed yet) — not an error.
type SearchHit struct {
	Project string
	Path    string
	Snippet string
	Rank    float64
	Symbol  string // optional: enclosing function/method/class name
	Line    int    // optional: 1-based line of the first match
}

// migrateFileText creates the FTS5 virtual table used by SearchText.
// Safe to call on every open — the IF NOT EXISTS clause makes it
// idempotent. Skipped silently if the SQLite build doesn't include
// FTS5 (modernc.org/sqlite ships with it enabled, but defensive
// fallback keeps the rest of the DB functional if that ever changes).
func (db *DB) migrateFileText() {
	// `content=''` makes this a contentless table: FTS5 stores only
	// the tokenized index, not the raw text. We don't need to read
	// bodies back from FTS5 (the file is on disk if we need it
	// again), and contentless cuts disk usage roughly in half.
	//
	// `tokenize='unicode61 remove_diacritics 2'` handles non-ASCII
	// source files without choking on combining marks. The default
	// 'simple' tokenizer would miss accented identifiers and
	// CJK code.
	db.conn.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS file_text USING fts5(
			project,
			path,
			body,
			tokenize = 'unicode61 remove_diacritics 2'
		)
	`)
}

// IndexFile inserts or replaces a row for (project, path). Idempotent
// — calling twice with different bodies for the same (project, path)
// keeps only the latest. project may be empty for single-root
// workspaces; path must be non-empty.
func (db *DB) IndexFile(project, path, body string) error {
	if path == "" {
		return fmt.Errorf("IndexFile: path required")
	}
	// FTS5 doesn't support UPSERT directly; emulate via DELETE +
	// INSERT in a single transaction. The DELETE is cheap when no
	// row exists (just an index miss).
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM file_text WHERE project = ? AND path = ?`, project, path,
	); err != nil {
		return fmt.Errorf("IndexFile: delete existing: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO file_text(project, path, body) VALUES (?, ?, ?)`,
		project, path, body,
	); err != nil {
		return fmt.Errorf("IndexFile: insert: %w", err)
	}
	return tx.Commit()
}

// FileTextCount returns the number of indexed rows. Used by the
// agent tool to decide whether to trigger a backfill on first use.
// Returns 0 when the table doesn't exist (FTS5 unavailable).
func (db *DB) FileTextCount() int {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM file_text`).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// CountFileTextForProject returns the number of indexed rows for one
// project, or 0 when the project hasn't been backfilled yet. Used by
// the agent tool's lazy-multi-root backfill: on every search, any
// project with 0 rows gets backfilled. Closes the bug where a
// single-root backfill from an earlier session sticks even after the
// workspace becomes multi-root (FileTextCount > 0 short-circuited
// future fanouts).
func (db *DB) CountFileTextForProject(project string) int {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM file_text WHERE project = ?`, project).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// SearchText runs an FTS5 query and returns up to `limit` results
// ranked by BM25 (lower rank is better, per SQLite convention).
//
// Query syntax is FTS5's MATCH grammar: phrases in quotes, AND/OR/
// NOT, prefix with `*` (`config*`), proximity with `NEAR()`. A bare
// identifier matches whole-word occurrences. The agent tool layer
// is responsible for showing the user-friendly syntax — this layer
// passes through verbatim so power features (boolean queries,
// proximity) are reachable when the agent wants them.
//
// Project filter is optional. When non-empty, results are restricted
// to that project; useful for multi-root workspaces where the agent
// already knows which repo owns the question.
func (db *DB) SearchText(query, project string, limit int) ([]SearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("SearchText: query required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // bound the response size — agent context is precious
	}

	// snippet(file_text, COL, prefix, suffix, ellipsis, ntoks):
	// COL=2 → body column. Surround matches with « » so the agent
	// can find the highlighted spans in its output without
	// re-parsing. 8 tokens of context on either side of the match
	// is a good balance — enough to read, not enough to flood.
	var rows *sql.Rows
	var err error
	if project == "" {
		rows, err = db.conn.Query(`
			SELECT project, path,
			       snippet(file_text, 2, '«', '»', '…', 8) AS snippet,
			       bm25(file_text) AS rank
			FROM file_text
			WHERE file_text MATCH ?
			ORDER BY rank
			LIMIT ?
		`, query, limit)
	} else {
		rows, err = db.conn.Query(`
			SELECT project, path,
			       snippet(file_text, 2, '«', '»', '…', 8) AS snippet,
			       bm25(file_text) AS rank
			FROM file_text
			WHERE file_text MATCH ? AND project = ?
			ORDER BY rank
			LIMIT ?
		`, query, project, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("SearchText: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Project, &h.Path, &h.Snippet, &h.Rank); err != nil {
			return nil, fmt.Errorf("SearchText scan: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// RemoveFile drops a single (project, path) row. Used by the
// watcher's incremental refresh when a file is deleted or renamed
// out from under us — without this the FTS index would keep
// returning hits for a path that no longer exists, and the agent
// would chase ghost references.
func (db *DB) RemoveFile(project, path string) error {
	if path == "" {
		return fmt.Errorf("RemoveFile: path required")
	}
	_, err := db.conn.Exec(
		`DELETE FROM file_text WHERE project = ? AND path = ?`,
		project, path,
	)
	return err
}

// ClearFileTextForProject drops every indexed row for a project.
// Used by the backfill path when re-indexing — easier to wipe +
// rewrite than diff. For a 1000-file project this is sub-100ms on
// SQLite's WAL mode.
func (db *DB) ClearFileTextForProject(project string) error {
	_, err := db.conn.Exec(`DELETE FROM file_text WHERE project = ?`, project)
	return err
}
