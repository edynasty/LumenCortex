-- The same schema as go_schema.sql without session_relations and session_checkpoints, as created by the
-- core revision Desktop pinned before them. user_version stays 0.

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


INSERT INTO metadata(key, value) VALUES('schema_version', '1');
