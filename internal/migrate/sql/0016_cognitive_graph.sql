-- 0016: P0 native graph after cognition/support 0014/0015 in the Go authority. Node tables remain forbidden.
CREATE TABLE cognition_nodes (
  id TEXT NOT NULL CHECK (length(id) BETWEEN 1 AND 128),
  version INTEGER NOT NULL CHECK (version >= 1),
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('entity','evidence','belief','abstraction','task','negative','experience')),
  created_at TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json) AND json_type(json) = 'object' AND length(CAST(json AS BLOB)) <= 65536),
  PRIMARY KEY (id, version)
) STRICT;
CREATE INDEX idx_cognition_nodes_session ON cognition_nodes(session_id);
CREATE TABLE cognition_edges (
  id TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  from_id TEXT NOT NULL,
  to_id TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json) AND json_type(json) = 'object' AND length(CAST(json AS BLOB)) <= 16384)
) STRICT;
CREATE INDEX idx_cognition_edges_session ON cognition_edges(session_id);
CREATE INDEX idx_cognition_edges_from ON cognition_edges(from_id);
CREATE INDEX idx_cognition_edges_to ON cognition_edges(to_id);
-- Original result remains immutable. This is the source-fence generation at append, not a failure log.
ALTER TABLE eval_results ADD COLUMN cognition_epoch INTEGER NOT NULL DEFAULT 0 CHECK (cognition_epoch >= 0);
