-- 0001_v1.sql: the schema that internal/session created before the migration runner existed.
-- Every statement is IF NOT EXISTS, so it is a no-op on databases created by a current Go core and only
-- adds session_relations / session_checkpoints to databases created by an older Desktop-pinned core.
-- Databases created by the legacy Node runtime are refused before this file runs (ErrLegacyNodeDatabase);
-- they are not supported and never migrated.
-- The runner sets PRAGMA user_version = 1 in the same transaction. From here on user_version is the only
-- schema authority: the legacy metadata.schema_version row is deleted, so a row that reappears later was
-- written by a pre-runner binary (an older Go core or the Node runtime) and is reported as a foreign writer.
-- It also removes the provider reasoning that earlier cores stored in transcript rows: the first migrated
-- version already holds no hidden reasoning (AGENTS.md reasoning boundary; design README §6.3).

CREATE TABLE IF NOT EXISTS metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  status TEXT NOT NULL,
  provider TEXT,
  model TEXT,
  goal TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  final TEXT,
  usage_json TEXT,
  error_json TEXT
) STRICT;
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);

CREATE TABLE IF NOT EXISTS session_messages (
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(session_id, seq),
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS agent_steps (
  session_id TEXT NOT NULL,
  step INTEGER NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(session_id, step),
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS journal (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  event TEXT NOT NULL,
  payload_json TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_journal_event_at ON journal(event, at DESC);

CREATE TABLE IF NOT EXISTS session_relations (
  child_session_id TEXT PRIMARY KEY,
  parent_session_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  created_at TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  FOREIGN KEY(parent_session_id) REFERENCES sessions(id) ON DELETE CASCADE,
  FOREIGN KEY(child_session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX IF NOT EXISTS idx_session_relations_parent
ON session_relations(parent_session_id, created_at);

CREATE TABLE IF NOT EXISTS session_checkpoints (
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  at TEXT NOT NULL,
  reason TEXT NOT NULL,
  json TEXT NOT NULL,
  PRIMARY KEY(session_id, seq),
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX IF NOT EXISTS idx_session_checkpoints_at
ON session_checkpoints(session_id, at DESC);

-- Keep this statement identical to scrubStatement in internal/migrate/backup.go: the same removal runs
-- on the pre-migration backup, and TestScrubStatementMatches0001 compares the two.
UPDATE session_messages
SET json = json_remove(json, '$.reasoning', '$.reasoning_content')
WHERE json_valid(json)
  AND (json_type(json, '$.reasoning') IS NOT NULL OR json_type(json, '$.reasoning_content') IS NOT NULL);

DELETE FROM metadata WHERE key = 'schema_version';
