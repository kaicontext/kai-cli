PRAGMA journal_mode=WAL;

CREATE TABLE IF NOT EXISTS nodes (
  id BLOB PRIMARY KEY,         -- blake3(kind+payloadCanonicalJSON)
  kind TEXT NOT NULL,          -- File | Module | Symbol | Snapshot | ChangeSet | ChangeType
  payload TEXT NOT NULL,       -- canonical JSON
  created_at INTEGER NOT NULL  -- unix ms
);
CREATE INDEX IF NOT EXISTS nodes_kind ON nodes(kind);

CREATE TABLE IF NOT EXISTS edges (
  src BLOB NOT NULL,
  type TEXT NOT NULL,          -- CONTAINS | DEFINES_IN | HAS_FILE | MODIFIES | HAS | AFFECTS
  dst BLOB NOT NULL,
  at  BLOB,                    -- snapshot or changeset context (metadata, not in PK)
  created_at INTEGER NOT NULL,
  PRIMARY KEY (src, type, dst)
);

CREATE INDEX IF NOT EXISTS edges_src ON edges(src);
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst);
CREATE INDEX IF NOT EXISTS edges_type ON edges(type);
CREATE INDEX IF NOT EXISTS edges_at ON edges(at);

-- Named references (aliases)
CREATE TABLE IF NOT EXISTS refs (
  name TEXT PRIMARY KEY,           -- e.g., 'snap.main', 'cs.latest', 'ws.feature/auth.head'
  target_id BLOB NOT NULL,         -- nodes.id
  target_kind TEXT NOT NULL,       -- 'Snapshot' | 'ChangeSet' | ...
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS refs_kind ON refs(target_kind);

-- Human-readable slugs
CREATE TABLE IF NOT EXISTS slugs (
  target_id BLOB PRIMARY KEY,
  slug TEXT UNIQUE NOT NULL       -- 'snap_20241202-143000_001', 'cs_00042'
);

-- Sequence log for navigation
CREATE TABLE IF NOT EXISTS logs (
  kind TEXT NOT NULL,              -- 'ChangeSet' or 'Snapshot'
  seq INTEGER NOT NULL,            -- monotonic sequence number
  id BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (kind, seq)
);
CREATE INDEX IF NOT EXISTS logs_id ON logs(id);
