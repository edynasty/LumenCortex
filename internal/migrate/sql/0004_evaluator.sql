-- 0004_evaluator.sql: content-addressed artifacts and evaluator results (design §4.7, I8).
--
-- artifacts holds check output (kind log) and, for a result that did not pass, the workspace diff its
-- fingerprint covers (kind diff), keyed by sha256 content hash per session and capped at 1 MiB.
-- eval_results holds one row per decided or accepted check. seq is the session's own result counter, not
-- a transcript seq. provenance says who produced the result: runtime, human:<id> or llm:<model>; only
-- passed runtime and human results at the current fingerprint satisfy a check, and nothing in the
-- runtime writes llm:*. The schema is the design reference verify/sql/0004_evaluator.sql.
CREATE TABLE artifacts (
  session_id TEXT NOT NULL,
  hash TEXT NOT NULL CHECK (length(hash) = 71 AND substr(hash,1,7) = 'sha256:'),
  kind TEXT NOT NULL CHECK (kind IN ('log','diff')),
  size INTEGER NOT NULL CHECK (size >= 0),
  truncated INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0,1)),
  created_at TEXT NOT NULL,
  data BLOB NOT NULL,
  PRIMARY KEY (session_id, hash),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE eval_results (
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  work_unit_id TEXT,
  check_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('test','build','schema')),
  status TEXT NOT NULL CHECK (status IN ('passed','failed','error')),
  provenance TEXT NOT NULL CHECK (provenance = 'runtime' OR provenance GLOB 'llm:?*' OR provenance GLOB 'human:?*'),
  fingerprint TEXT NOT NULL,
  artifacts_json TEXT NOT NULL CHECK (json_valid(artifacts_json) AND json_type(artifacts_json) = 'array'),
  at TEXT NOT NULL,
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_eval_results_check ON eval_results(session_id, check_id, seq DESC);
