-- 0014_cognition: persistent cognition maintenance after the mainline Hub schema (0013). The maintenance state of
-- this database's project: a monotonic dirty revision the source commit advances in its own transaction,
-- the cursor a maintenance commit advances, and the lease (id, generation, expiry) every maintenance write
-- is compared against. One row: this database is the single maintenance authority of its project.
CREATE TABLE maintenance_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  project_id TEXT NOT NULL CHECK (length(project_id) BETWEEN 1 AND 4096),
  policy_version TEXT NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 64),
  dirty_revision INTEGER NOT NULL DEFAULT 0 CHECK (dirty_revision >= 0),
  cursor_revision INTEGER NOT NULL DEFAULT 0 CHECK (cursor_revision >= 0 AND cursor_revision <= dirty_revision),
  lease_id TEXT NOT NULL DEFAULT '' CHECK (length(lease_id) <= 64),
  generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  expires_at TEXT NOT NULL DEFAULT '',
  pending_state TEXT NOT NULL DEFAULT 'idle' CHECK (pending_state IN ('idle','running','failed')),
  last_error TEXT NOT NULL DEFAULT '' CHECK (length(last_error) <= 512),
  updated_at TEXT NOT NULL
) STRICT;

-- The sources a revision made dirty, one row per revision. A snapshot reads (cursor, frozen]; the commit
-- that moves the cursor past a row deletes it, in the same transaction. A row goes with its session.
CREATE TABLE maintenance_sources (
  revision INTEGER PRIMARY KEY CHECK (revision >= 1),
  kind TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 32),
  source_id TEXT NOT NULL CHECK (length(source_id) BETWEEN 1 AND 512),
  digest TEXT NOT NULL CHECK (length(digest) = 71 AND substr(digest,1,7) = 'sha256:'),
  scope TEXT NOT NULL CHECK (length(scope) BETWEEN 1 AND 512),
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL DEFAULT '' CHECK (length(run_id) <= 128),
  sensitivity TEXT NOT NULL CHECK (sensitivity IN ('session','private')),
  provenance TEXT NOT NULL CHECK (provenance = 'runtime'),
  created_at TEXT NOT NULL
) STRICT;

-- One processing of one source version under one policy for one reader scope: the same key is never
-- sent to a maintainer again. It holds no source text.
CREATE TABLE maintenance_dedupe (
  dedupe_key TEXT PRIMARY KEY CHECK (length(dedupe_key) = 71 AND substr(dedupe_key,1,7) = 'sha256:'),
  source_ref TEXT NOT NULL CHECK (length(source_ref) BETWEEN 1 AND 1200),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  generation INTEGER NOT NULL CHECK (generation >= 1),
  outcome TEXT NOT NULL CHECK (outcome IN ('candidates','no_candidate','skipped')),
  maintenance_session_id TEXT NOT NULL DEFAULT '',
  policy_version TEXT NOT NULL,
  created_at TEXT NOT NULL
) STRICT;

-- Candidates a maintainer proposed, as Core validated them. Untrusted text; append-only: a row's status
-- is the decision taken at commit, never rewritten. A candidate goes with its maintenance session.
CREATE TABLE cognition_candidates (
  candidate_id TEXT PRIMARY KEY CHECK (length(candidate_id) BETWEEN 4 AND 64),
  schema_version INTEGER NOT NULL CHECK (schema_version >= 1),
  project_id TEXT NOT NULL,
  maintenance_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  generation INTEGER NOT NULL CHECK (generation >= 1),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  source_refs_json TEXT NOT NULL CHECK (json_valid(source_refs_json) AND json_type(source_refs_json) = 'array'),
  source_digest TEXT NOT NULL,
  source_scope TEXT NOT NULL,
  origin_run_id TEXT NOT NULL DEFAULT '',
  origin_session_id TEXT NOT NULL DEFAULT '',
  claim_type TEXT NOT NULL,
  subject TEXT NOT NULL,
  subject_key TEXT NOT NULL,
  proposed_claim TEXT NOT NULL CHECK (length(CAST(proposed_claim AS BLOB)) <= 2048),
  rationale TEXT NOT NULL CHECK (length(CAST(rationale AS BLOB)) <= 1024),
  claim_digest TEXT NOT NULL,
  cited_paths_json TEXT NOT NULL CHECK (json_valid(cited_paths_json) AND json_type(cited_paths_json) = 'array'),
  check_ids_json TEXT NOT NULL CHECK (json_valid(check_ids_json) AND json_type(check_ids_json) = 'array'),
  policy_version TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('qualified','candidate_only','rejected')),
  status_reason TEXT NOT NULL DEFAULT '' CHECK (length(status_reason) <= 512),
  created_at TEXT NOT NULL,
  CHECK (status = 'rejected' OR claim_type IN ('procedure','fact','failure','decision'))
) STRICT;
CREATE INDEX idx_cognition_candidates_claim ON cognition_candidates(claim_digest);
CREATE INDEX idx_cognition_candidates_subject ON cognition_candidates(subject_key);

-- Qualified observations: an approved check's result in a closed episode, linked to the candidate it was
-- run for. Derived from the eval result row itself; append-only, so a later pass never overwrites an
-- earlier failure. An observation goes with its candidate or with the session holding the result.
CREATE TABLE cognition_observations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  candidate_id TEXT NOT NULL REFERENCES cognition_candidates(candidate_id) ON DELETE CASCADE,
  check_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  result_seq INTEGER NOT NULL,
  episode_id TEXT NOT NULL,
  fingerprint TEXT NOT NULL CHECK (length(fingerprint) >= 1),
  spec_digest TEXT NOT NULL,
  approval_epoch INTEGER NOT NULL CHECK (approval_epoch >= 1),
  outcome TEXT NOT NULL CHECK (outcome IN ('passed','failed')),
  scope TEXT NOT NULL,
  purpose TEXT NOT NULL CHECK (purpose IN ('adoption','revalidation')),
  run_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  FOREIGN KEY (session_id, result_seq) REFERENCES eval_results(session_id, seq) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_cognition_observations_candidate ON cognition_observations(candidate_id, check_id, id DESC);
CREATE INDEX idx_cognition_observations_result ON cognition_observations(session_id, result_seq);
