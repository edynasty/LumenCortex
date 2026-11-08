-- Observable metadata only; content remains in transcript/artifact stores.
CREATE TABLE runtime_event_sequences (
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK(sequence >= 0),
  PRIMARY KEY(session_id, run_id)
) STRICT;
CREATE TABLE runtime_events (
  cursor INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK(sequence > 0),
  type TEXT NOT NULL CHECK(length(type) BETWEEN 1 AND 128),
  event_json TEXT NOT NULL CHECK(json_valid(event_json) AND length(CAST(event_json AS BLOB)) <= 16384),
  UNIQUE(session_id, run_id, sequence)
) STRICT;
CREATE INDEX runtime_events_session ON runtime_events(session_id, cursor);
CREATE INDEX runtime_events_run ON runtime_events(session_id, run_id, cursor);
CREATE INDEX runtime_events_type ON runtime_events(session_id, type, cursor);
CREATE TABLE runtime_event_retention (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  pruned_through INTEGER NOT NULL DEFAULT 0
) STRICT;
