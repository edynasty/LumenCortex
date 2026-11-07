-- A semantic row cannot be represented by v9. The copy fails atomically rather
-- than deleting or relabelling evidence; callers must keep the newer binary.
CREATE TABLE eval_results_v9 (
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
  episode_id TEXT CHECK (episode_id IS NULL OR length(episode_id) BETWEEN 1 AND 128),
  pre_fingerprint TEXT,
  post_fingerprint TEXT,
  spec_digest TEXT,
  approval_epoch INTEGER CHECK (approval_epoch IS NULL OR approval_epoch >= 0),
  episode_status TEXT CHECK (episode_status IS NULL OR episode_status IN ('open','closed','scope_changed','spec_unapproved','interrupted')),
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
INSERT INTO eval_results_v9 SELECT * FROM eval_results;
DROP TABLE eval_results;
ALTER TABLE eval_results_v9 RENAME TO eval_results;
CREATE INDEX idx_eval_results_check ON eval_results(session_id, check_id, seq DESC);
CREATE INDEX idx_eval_results_episode ON eval_results(session_id, episode_id);
