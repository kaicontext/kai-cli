// Package ref provides human-friendly references to nodes (short IDs, named refs, selectors, slugs).
package ref

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kai/internal/graph"
	"kai/internal/util"
)

// Kind represents the type of a resolvable entity.
type Kind string

const (
	KindSnapshot  Kind = "Snapshot"
	KindChangeSet Kind = "ChangeSet"
	KindWorkspace Kind = "Workspace"
	KindReview    Kind = "Review"
	KindFile      Kind = "File"
	KindSymbol    Kind = "Symbol"
	KindModule    Kind = "Module"
)

// Ref represents a named reference to a node.
type Ref struct {
	Name       string
	TargetID   []byte
	TargetKind Kind
	CreatedAt  int64
	UpdatedAt  int64
	Meta       map[string]string // optional metadata (git commit, message, author, etc.)
}

// Slug represents a human-readable identifier for a node.
type Slug struct {
	TargetID []byte
	Slug     string
}

// LogEntry represents a sequenced entry for a node kind.
type LogEntry struct {
	Kind      Kind
	Seq       int64
	ID        []byte
	CreatedAt int64
}

// ResolveResult contains the result of resolving an input.
type ResolveResult struct {
	ID   []byte
	Kind Kind
}

// AmbiguityError indicates multiple nodes match a prefix.
type AmbiguityError struct {
	Prefix     string
	Candidates []ResolveResult
}

func (e *AmbiguityError) Error() string {
	var parts []string
	for _, c := range e.Candidates {
		parts = append(parts, fmt.Sprintf("%s (%s)", util.BytesToHex(c.ID)[:16], c.Kind))
	}
	return fmt.Sprintf("ambiguous prefix '%s' matches:\n  %s\nprovide more characters or use a ref", e.Prefix, strings.Join(parts, "\n  "))
}

// NotFoundError indicates no node was found.
type NotFoundError struct {
	Input string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("not found: %s", e.Input)
}

// Resolver handles ID resolution from various input formats.
type Resolver struct {
	db *graph.DB
}

// NewResolver creates a new resolver.
func NewResolver(db *graph.DB) *Resolver {
	return &Resolver{db: db}
}

// selectorPattern matches @kind:selector syntax
var selectorPattern = regexp.MustCompile(`^@(snap|cs|ws|snapshot|changeset|workspace):(.+)$`)

// relativePattern matches ~N suffix for relative navigation
var relativePattern = regexp.MustCompile(`^(.+)~(\d+)$`)

// Resolve resolves an input string to a node ID.
// Input can be:
// - Full 64-char hex ID
// - Short hex prefix (8+ chars)
// - Ref name (snap.main, cs.latest, etc.)
// - Slug (snap_2024-12-02_001)
// - Selector (@snap:last, @cs:last~2, @ws:name:head)
//
// If wantKind is provided, only matches of that kind are returned.
func (r *Resolver) Resolve(input string, wantKind *Kind) (*ResolveResult, error) {
	// 1. Check if it's a full hex ID (64 chars)
	if len(input) == 64 && isHex(input) {
		id, err := util.HexToBytes(input)
		if err != nil {
			return nil, err
		}
		node, err := r.db.GetNode(id)
		if err != nil {
			return nil, err
		}
		if node == nil {
			return nil, &NotFoundError{Input: input}
		}
		kind := Kind(node.Kind)
		if wantKind != nil && kind != *wantKind {
			return nil, fmt.Errorf("expected %s but found %s", *wantKind, kind)
		}
		return &ResolveResult{ID: id, Kind: kind}, nil
	}

	// 2. Check if it's a slug
	result, err := r.resolveSlug(input, wantKind)
	if err == nil && result != nil {
		return result, nil
	}

	// 3. Check if it's a ref name
	result, err = r.resolveRef(input, wantKind)
	if err == nil && result != nil {
		return result, nil
	}

	// 4. Check if it's a selector (@snap:last, etc.)
	if strings.HasPrefix(input, "@") {
		return r.resolveSelector(input, wantKind)
	}

	// 5. Treat as short hex prefix
	if len(input) >= 8 && isHex(input) {
		return r.resolveShortID(input, wantKind)
	}

	return nil, &NotFoundError{Input: input}
}

// resolveShortID resolves a short hex prefix to a full ID.
func (r *Resolver) resolveShortID(prefix string, wantKind *Kind) (*ResolveResult, error) {
	prefix = strings.ToLower(prefix)

	query := `SELECT id, kind FROM nodes WHERE hex(id) LIKE ? || '%' COLLATE NOCASE`
	args := []interface{}{prefix}

	if wantKind != nil {
		query += ` AND kind = ?`
		args = append(args, string(*wantKind))
	}

	query += ` LIMIT 11`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying nodes: %w", err)
	}
	defer rows.Close()

	var candidates []ResolveResult
	for rows.Next() {
		var id []byte
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		candidates = append(candidates, ResolveResult{ID: id, Kind: Kind(kind)})
	}

	if len(candidates) == 0 {
		return nil, &NotFoundError{Input: prefix}
	}
	if len(candidates) == 1 {
		return &candidates[0], nil
	}
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	return nil, &AmbiguityError{Prefix: prefix, Candidates: candidates}
}

// resolveRef resolves a named reference.
func (r *Resolver) resolveRef(name string, wantKind *Kind) (*ResolveResult, error) {
	query := `SELECT target_id, target_kind FROM refs WHERE name = ?`
	args := []interface{}{name}

	var id []byte
	var kind string
	err := r.db.QueryRow(query, args...).Scan(&id, &kind)
	if err == sql.ErrNoRows {
		return nil, nil // Not found, try other methods
	}
	if err != nil {
		return nil, fmt.Errorf("querying ref: %w", err)
	}

	k := Kind(kind)
	if wantKind != nil && k != *wantKind {
		return nil, fmt.Errorf("ref '%s' points to %s, expected %s", name, k, *wantKind)
	}

	return &ResolveResult{ID: id, Kind: k}, nil
}

// resolveSlug resolves a human-readable slug.
func (r *Resolver) resolveSlug(slug string, wantKind *Kind) (*ResolveResult, error) {
	query := `SELECT s.target_id, n.kind FROM slugs s JOIN nodes n ON s.target_id = n.id WHERE s.slug = ?`
	args := []interface{}{slug}

	var id []byte
	var kind string
	err := r.db.QueryRow(query, args...).Scan(&id, &kind)
	if err == sql.ErrNoRows {
		return nil, nil // Not found, try other methods
	}
	if err != nil {
		return nil, fmt.Errorf("querying slug: %w", err)
	}

	k := Kind(kind)
	if wantKind != nil && k != *wantKind {
		return nil, fmt.Errorf("slug '%s' points to %s, expected %s", slug, k, *wantKind)
	}

	return &ResolveResult{ID: id, Kind: k}, nil
}

// resolveSelector resolves a selector expression.
// Formats:
// - @snap:last, @snap:prev
// - @cs:last, @cs:prev, @cs:last~2
// - @ws:name:head, @ws:name:base
func (r *Resolver) resolveSelector(input string, wantKind *Kind) (*ResolveResult, error) {
	// Check for relative navigation (~N)
	base := input
	offset := 0
	if m := relativePattern.FindStringSubmatch(input); m != nil {
		base = m[1]
		offset, _ = strconv.Atoi(m[2])
	}

	// Parse the selector
	m := selectorPattern.FindStringSubmatch(base)
	if m == nil {
		return nil, fmt.Errorf("invalid selector format: %s", input)
	}

	kindStr := strings.ToLower(m[1])
	selector := m[2]

	var kind Kind
	switch kindStr {
	case "snap", "snapshot":
		kind = KindSnapshot
	case "cs", "changeset":
		kind = KindChangeSet
	case "ws", "workspace":
		kind = KindWorkspace
	default:
		return nil, fmt.Errorf("unknown kind in selector: %s", kindStr)
	}

	if wantKind != nil && kind != *wantKind {
		return nil, fmt.Errorf("selector is for %s, expected %s", kind, *wantKind)
	}

	// Handle workspace selectors specially
	if kind == KindWorkspace {
		return r.resolveWorkspaceSelector(selector)
	}

	// Handle :last, :prev, :working for snapshots and changesets
	switch selector {
	case "last":
		return r.resolveLatest(kind, offset)
	case "prev":
		return r.resolveLatest(kind, offset+1)
	case "working":
		// @snap:working resolves to the snap.working ref (ephemeral working snapshot)
		if kind != KindSnapshot {
			return nil, fmt.Errorf("@%s:working only valid for snapshots", kindStr)
		}
		return r.resolveRef("snap.working", &kind)
	default:
		// Try resolving as a ref name (e.g. @snap:20260314T090755.729 or @snap:snap.20260314T090755.729)
		// First try the selector as-is (already a full ref name like snap.20260314T...)
		result, err := r.resolveRef(selector, &kind)
		if err == nil && result != nil {
			return result, nil
		}
		// Then try prefixing with the kind (e.g. "20260314T..." -> "snap.20260314T...")
		result, err = r.resolveRef(kindStr+"."+selector, &kind)
		if err == nil && result != nil {
			return result, nil
		}
		return nil, fmt.Errorf("unknown selector: %s (try 'last', 'prev', 'working', or a ref name)", selector)
	}
}

// resolveLatest resolves to the Nth latest node of a kind.
func (r *Resolver) resolveLatest(kind Kind, offset int) (*ResolveResult, error) {
	// First try using the logs table for accurate sequencing
	query := `SELECT id FROM logs WHERE kind = ? ORDER BY seq DESC LIMIT 1 OFFSET ?`
	var id []byte
	err := r.db.QueryRow(query, string(kind), offset).Scan(&id)
	if err == nil {
		return &ResolveResult{ID: id, Kind: kind}, nil
	}

	// Fall back to created_at ordering
	query = `SELECT id FROM nodes WHERE kind = ? ORDER BY created_at DESC, id ASC LIMIT 1 OFFSET ?`
	err = r.db.QueryRow(query, string(kind), offset).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, &NotFoundError{Input: fmt.Sprintf("@%s:last~%d", strings.ToLower(string(kind[:4])), offset)}
	}
	if err != nil {
		return nil, fmt.Errorf("querying latest %s: %w", kind, err)
	}

	return &ResolveResult{ID: id, Kind: kind}, nil
}

// resolveWorkspaceSelector resolves workspace selectors like @ws:name:head.
func (r *Resolver) resolveWorkspaceSelector(selector string) (*ResolveResult, error) {
	// Parse name:head or name:base
	parts := strings.Split(selector, ":")
	if len(parts) < 2 {
		// Just workspace name, return the workspace itself
		ws, err := r.db.GetWorkspaceByName(selector)
		if err != nil {
			return nil, err
		}
		if ws == nil {
			return nil, &NotFoundError{Input: "@ws:" + selector}
		}
		return &ResolveResult{ID: ws.ID, Kind: KindWorkspace}, nil
	}

	wsName := strings.Join(parts[:len(parts)-1], ":")
	position := parts[len(parts)-1]

	// First try as a ref
	refName := fmt.Sprintf("ws.%s.%s", wsName, position)
	result, err := r.resolveRef(refName, nil)
	if err == nil && result != nil {
		return result, nil
	}

	// Otherwise look up the workspace
	ws, err := r.db.GetWorkspaceByName(wsName)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, &NotFoundError{Input: "@ws:" + selector}
	}

	switch position {
	case "head":
		if headSnap, ok := ws.Payload["headSnapshot"].(string); ok && headSnap != "" {
			id, err := util.HexToBytes(headSnap)
			if err != nil {
				return nil, err
			}
			return &ResolveResult{ID: id, Kind: KindSnapshot}, nil
		}
		return nil, fmt.Errorf("workspace '%s' has no head snapshot", wsName)
	case "base":
		if baseSnap, ok := ws.Payload["baseSnapshot"].(string); ok && baseSnap != "" {
			id, err := util.HexToBytes(baseSnap)
			if err != nil {
				return nil, err
			}
			return &ResolveResult{ID: id, Kind: KindSnapshot}, nil
		}
		return nil, fmt.Errorf("workspace '%s' has no base snapshot", wsName)
	default:
		return nil, fmt.Errorf("unknown workspace position: %s (use 'head' or 'base')", position)
	}
}

// MustResolve is like Resolve but panics on error.
func (r *Resolver) MustResolve(input string, wantKind *Kind) *ResolveResult {
	result, err := r.Resolve(input, wantKind)
	if err != nil {
		panic(err)
	}
	return result
}

// isHex checks if a string is valid hexadecimal.
func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// RefManager handles CRUD operations for refs.
type RefManager struct {
	db *graph.DB
}

// NewRefManager creates a new ref manager.
func NewRefManager(db *graph.DB) *RefManager {
	mgr := &RefManager{db: db}
	// Ensure tables exist
	mgr.ensureTables()
	return mgr
}

// ensureTables creates the refs table if it doesn't exist.
func (m *RefManager) ensureTables() {
	m.db.Exec(`
		CREATE TABLE IF NOT EXISTS refs (
			name TEXT PRIMARY KEY,
			target_id BLOB NOT NULL,
			target_kind TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	m.db.Exec(`CREATE INDEX IF NOT EXISTS refs_kind ON refs(target_kind)`)
	// Ref log for auditability
	m.db.Exec(`
		CREATE TABLE IF NOT EXISTS ref_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			old_target BLOB,
			new_target BLOB NOT NULL,
			actor TEXT,
			moved_at INTEGER NOT NULL
		)
	`)
	m.db.Exec(`CREATE INDEX IF NOT EXISTS ref_log_name ON ref_log(name)`)
}

// Set creates or updates a ref.
func (m *RefManager) Set(name string, targetID []byte, targetKind Kind) error {
	return m.SetWithMeta(name, targetID, targetKind, "", nil)
}

// SetWithActor creates or updates a ref with actor attribution.
func (m *RefManager) SetWithActor(name string, targetID []byte, targetKind Kind, actor string) error {
	return m.SetWithMeta(name, targetID, targetKind, actor, nil)
}

// SetWithMeta creates or updates a ref with actor and metadata.
// Meta is stored as JSON in the refs table for git commit info, messages, etc.
func (m *RefManager) SetWithMeta(name string, targetID []byte, targetKind Kind, actor string, meta map[string]string) error {
	now := util.NowMs()

	// Get old target for logging (may be nil for new refs)
	var oldTarget []byte
	m.db.QueryRow(`SELECT target_id FROM refs WHERE name = ?`, name).Scan(&oldTarget)

	var metaJSON *string
	if len(meta) > 0 {
		b, _ := json.Marshal(meta)
		s := string(b)
		metaJSON = &s
	}

	query := `
		INSERT INTO refs (name, target_id, target_kind, created_at, updated_at, meta)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			target_id = excluded.target_id,
			target_kind = excluded.target_kind,
			updated_at = excluded.updated_at,
			meta = excluded.meta
	`
	_, err := m.db.Exec(query, name, targetID, string(targetKind), now, now, metaJSON)
	if err != nil {
		return err
	}

	// Log the change for auditability
	_, err = m.db.Exec(`
		INSERT INTO ref_log (name, old_target, new_target, actor, moved_at)
		VALUES (?, ?, ?, ?, ?)
	`, name, oldTarget, targetID, actor, now)
	return err
}

// Get retrieves a ref by name.
func (m *RefManager) Get(name string) (*Ref, error) {
	query := `SELECT name, target_id, target_kind, created_at, updated_at, meta FROM refs WHERE name = ?`
	var r Ref
	var kind string
	var metaJSON sql.NullString
	err := m.db.QueryRow(query, name).Scan(&r.Name, &r.TargetID, &kind, &r.CreatedAt, &r.UpdatedAt, &metaJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		// Fallback for DBs without meta column
		query2 := `SELECT name, target_id, target_kind, created_at, updated_at FROM refs WHERE name = ?`
		err2 := m.db.QueryRow(query2, name).Scan(&r.Name, &r.TargetID, &kind, &r.CreatedAt, &r.UpdatedAt)
		if err2 == sql.ErrNoRows {
			return nil, nil
		}
		if err2 != nil {
			return nil, err2
		}
		r.TargetKind = Kind(kind)
		return &r, nil
	}
	r.TargetKind = Kind(kind)
	if metaJSON.Valid && metaJSON.String != "" {
		r.Meta = make(map[string]string)
		json.Unmarshal([]byte(metaJSON.String), &r.Meta)
	}
	return &r, nil
}

// Delete removes a ref.
func (m *RefManager) Delete(name string) error {
	result, err := m.db.Exec(`DELETE FROM refs WHERE name = ?`, name)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("ref not found: %s", name)
	}
	return nil
}

// List returns all refs, optionally filtered by kind.
func (m *RefManager) List(filterKind *Kind) ([]*Ref, error) {
	query := `SELECT name, target_id, target_kind, created_at, updated_at, meta FROM refs`
	args := []interface{}{}
	if filterKind != nil {
		query += ` WHERE target_kind = ?`
		args = append(args, string(*filterKind))
	}
	query += ` ORDER BY name`

	rows, err := m.db.Query(query, args...)
	if err != nil {
		// Fallback for DBs without meta column
		return m.listWithoutMeta(filterKind)
	}
	defer rows.Close()

	var refs []*Ref
	for rows.Next() {
		var r Ref
		var kind string
		var metaJSON sql.NullString
		if err := rows.Scan(&r.Name, &r.TargetID, &kind, &r.CreatedAt, &r.UpdatedAt, &metaJSON); err != nil {
			return nil, err
		}
		r.TargetKind = Kind(kind)
		if metaJSON.Valid && metaJSON.String != "" {
			r.Meta = make(map[string]string)
			json.Unmarshal([]byte(metaJSON.String), &r.Meta)
		}
		refs = append(refs, &r)
	}
	return refs, rows.Err()
}

func (m *RefManager) listWithoutMeta(filterKind *Kind) ([]*Ref, error) {
	query := `SELECT name, target_id, target_kind, created_at, updated_at FROM refs`
	args := []interface{}{}
	if filterKind != nil {
		query += ` WHERE target_kind = ?`
		args = append(args, string(*filterKind))
	}
	query += ` ORDER BY name`
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []*Ref
	for rows.Next() {
		var r Ref
		var kind string
		if err := rows.Scan(&r.Name, &r.TargetID, &kind, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.TargetKind = Kind(kind)
		refs = append(refs, &r)
	}
	return refs, rows.Err()
}

// SlugManager handles slug operations.
type SlugManager struct {
	db *graph.DB
}

// NewSlugManager creates a new slug manager.
func NewSlugManager(db *graph.DB) *SlugManager {
	mgr := &SlugManager{db: db}
	mgr.ensureTables()
	return mgr
}

// ensureTables creates the slugs table if it doesn't exist.
func (m *SlugManager) ensureTables() {
	m.db.Exec(`
		CREATE TABLE IF NOT EXISTS slugs (
			target_id BLOB PRIMARY KEY,
			slug TEXT UNIQUE NOT NULL
		)
	`)
}

// GetOrCreate gets an existing slug or creates a new one.
func (m *SlugManager) GetOrCreate(id []byte, kind Kind) (string, error) {
	// Check for existing slug
	var existing string
	err := m.db.QueryRow(`SELECT slug FROM slugs WHERE target_id = ?`, id).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Generate new slug
	slug, err := m.generateSlug(kind)
	if err != nil {
		return "", err
	}

	// Insert slug
	_, err = m.db.Exec(`INSERT INTO slugs (target_id, slug) VALUES (?, ?)`, id, slug)
	if err != nil {
		return "", err
	}

	return slug, nil
}

// generateSlug creates a new unique slug for a kind.
func (m *SlugManager) generateSlug(kind Kind) (string, error) {
	now := time.Now().UTC()
	dateStr := now.Format("20060102-150405")

	var prefix string
	switch kind {
	case KindSnapshot:
		prefix = "snap"
	case KindChangeSet:
		prefix = "cs"
	case KindWorkspace:
		prefix = "ws"
	default:
		prefix = strings.ToLower(string(kind)[:3])
	}

	// Find next sequence number for this prefix+date
	pattern := fmt.Sprintf("%s_%s_%%", prefix, dateStr)
	var count int
	err := m.db.QueryRow(`SELECT COUNT(*) FROM slugs WHERE slug LIKE ?`, pattern).Scan(&count)
	if err != nil {
		return "", err
	}

	seq := count + 1
	return fmt.Sprintf("%s_%s_%03d", prefix, dateStr, seq), nil
}

// Get retrieves a slug for an ID.
func (m *SlugManager) Get(id []byte) (string, error) {
	var slug string
	err := m.db.QueryRow(`SELECT slug FROM slugs WHERE target_id = ?`, id).Scan(&slug)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return slug, err
}

// LogManager handles sequence logging for navigation.
type LogManager struct {
	db *graph.DB
}

// NewLogManager creates a new log manager.
func NewLogManager(db *graph.DB) *LogManager {
	mgr := &LogManager{db: db}
	mgr.ensureTables()
	return mgr
}

// ensureTables creates the logs table if it doesn't exist.
func (m *LogManager) ensureTables() {
	m.db.Exec(`
		CREATE TABLE IF NOT EXISTS logs (
			kind TEXT NOT NULL,
			seq INTEGER NOT NULL,
			id BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (kind, seq)
		)
	`)
	m.db.Exec(`CREATE INDEX IF NOT EXISTS logs_id ON logs(id)`)
}

// Append adds a new entry to the log.
func (m *LogManager) Append(kind Kind, id []byte) error {
	// Get next sequence number
	var maxSeq sql.NullInt64
	err := m.db.QueryRow(`SELECT MAX(seq) FROM logs WHERE kind = ?`, string(kind)).Scan(&maxSeq)
	if err != nil {
		return err
	}

	nextSeq := int64(1)
	if maxSeq.Valid {
		nextSeq = maxSeq.Int64 + 1
	}

	now := util.NowMs()
	_, err = m.db.Exec(`INSERT OR IGNORE INTO logs (kind, seq, id, created_at) VALUES (?, ?, ?, ?)`,
		string(kind), nextSeq, id, now)
	return err
}

// GetBySeq retrieves a log entry by sequence number.
func (m *LogManager) GetBySeq(kind Kind, seq int64) (*LogEntry, error) {
	var entry LogEntry
	var kindStr string
	err := m.db.QueryRow(`SELECT kind, seq, id, created_at FROM logs WHERE kind = ? AND seq = ?`,
		string(kind), seq).Scan(&kindStr, &entry.Seq, &entry.ID, &entry.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entry.Kind = Kind(kindStr)
	return &entry, nil
}

// GetLatestSeq returns the latest sequence number for a kind.
func (m *LogManager) GetLatestSeq(kind Kind) (int64, error) {
	var seq sql.NullInt64
	err := m.db.QueryRow(`SELECT MAX(seq) FROM logs WHERE kind = ?`, string(kind)).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

// AutoRefManager handles automatic ref updates.
type AutoRefManager struct {
	refMgr *RefManager
	logMgr *LogManager
}

// NewAutoRefManager creates a new auto-ref manager.
func NewAutoRefManager(db *graph.DB) *AutoRefManager {
	return &AutoRefManager{
		refMgr: NewRefManager(db),
		logMgr: NewLogManager(db),
	}
}

// OnSnapshotCreated updates refs after a snapshot is created (committed snapshot).
// This adds the snapshot to the log, making it resolvable via @snap:last.
func (m *AutoRefManager) OnSnapshotCreated(id []byte) error {
	return m.OnSnapshotCreatedWithMeta(id, nil)
}

// OnSnapshotCreatedWithMeta updates refs with optional metadata (git commit, message, author).
func (m *AutoRefManager) OnSnapshotCreatedWithMeta(id []byte, meta map[string]string) error {
	// Update snap.latest with metadata
	if err := m.refMgr.SetWithMeta("snap.latest", id, KindSnapshot, "", meta); err != nil {
		return err
	}
	// Append to log (this makes it @snap:last)
	return m.logMgr.Append(KindSnapshot, id)
}

// OnWorkingSnapshotUpdated updates the working snapshot ref without logging.
// This is used by 'kai scan' - the snapshot is ephemeral until promoted.
// Unlike OnSnapshotCreated, this does NOT add to the log, so:
// - @snap:working resolves to this snapshot
// - @snap:last still points to the previous committed snapshot
// - GC can reclaim old working snapshots when overwritten
func (m *AutoRefManager) OnWorkingSnapshotUpdated(id []byte) error {
	return m.refMgr.Set("snap.working", id, KindSnapshot)
}

// PromoteWorkingSnapshot promotes the working snapshot to a committed snapshot.
// This adds it to the log, making it @snap:last.
func (m *AutoRefManager) PromoteWorkingSnapshot() error {
	// Get current working snapshot
	ref, err := m.refMgr.Get("snap.working")
	if err != nil {
		return fmt.Errorf("no working snapshot to promote: %w", err)
	}
	// Now commit it (add to log, update snap.latest)
	return m.OnSnapshotCreated(ref.TargetID)
}

// OnChangeSetCreated updates refs after a changeset is created.
func (m *AutoRefManager) OnChangeSetCreated(id []byte) error {
	// Update cs.latest
	if err := m.refMgr.Set("cs.latest", id, KindChangeSet); err != nil {
		return err
	}
	// Append to log
	return m.logMgr.Append(KindChangeSet, id)
}

// OnWorkspaceHeadChanged updates refs when a workspace head changes.
func (m *AutoRefManager) OnWorkspaceHeadChanged(wsName string, headID []byte) error {
	refName := fmt.Sprintf("ws.%s.head", wsName)
	return m.refMgr.Set(refName, headID, KindSnapshot)
}

// OnWorkspaceCreated updates refs when a workspace is created.
func (m *AutoRefManager) OnWorkspaceCreated(wsName string, baseID []byte) error {
	// Set both base and head refs
	baseRef := fmt.Sprintf("ws.%s.base", wsName)
	headRef := fmt.Sprintf("ws.%s.head", wsName)

	if err := m.refMgr.Set(baseRef, baseID, KindSnapshot); err != nil {
		return err
	}
	return m.refMgr.Set(headRef, baseID, KindSnapshot)
}
