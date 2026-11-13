-- Completion records live beside the native graph, never in another memory database.
-- A pending application is an admission, not proof that its action ran.
CREATE TABLE cognition_applications (
  id TEXT PRIMARY KEY,
  experience_id TEXT NOT NULL,
  adoption_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','completed','failed','unknown')),
  created_at TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json) AND length(CAST(json AS BLOB)) <= 32768)
) STRICT;
CREATE INDEX idx_cognition_applications_session ON cognition_applications(session_id,run_id);
CREATE INDEX idx_cognition_nodes_kind ON cognition_nodes(kind,project_id,id,version DESC);
CREATE TABLE cognition_proposals (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','completed','refused')),
  node_id TEXT NOT NULL DEFAULT '',
  json TEXT NOT NULL CHECK (json_valid(json) AND length(CAST(json AS BLOB)) <= 16384)
) STRICT;
CREATE INDEX idx_cognition_proposals_session ON cognition_proposals(session_id,run_id,state);
CREATE TABLE cognition_decisions (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json) AND length(CAST(json AS BLOB)) <= 16384)
) STRICT;
CREATE INDEX idx_cognition_decisions_session ON cognition_decisions(session_id,run_id);
-- Reader-local telemetry is not shared evidence and is erased with that reader.
CREATE TABLE cognition_access (
  node_id TEXT NOT NULL,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  access_count INTEGER NOT NULL CHECK (access_count > 0),
  last_access_at TEXT NOT NULL,
  PRIMARY KEY(node_id,session_id)
) STRICT;
CREATE INDEX idx_cognition_access_session ON cognition_access(session_id);
