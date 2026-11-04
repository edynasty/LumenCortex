-- 0006_provenance_and_approvals.sql: request provenance, derived-origin edges, source fences and check
-- approvals (ADR-0018; cognitive design OPUS_ENGINEERING_EXECUTION_PLAN §3.3). Four new tables and six
-- eval_results columns; nothing is backfilled.
--
-- Ownership. request_envelopes and derived_origins belong to their CONSUMER session and cascade with it.
-- Every reference to a SOURCE (source session/seq, path+blob, check result) is a plain column, never a
-- foreign key: deleting a source must leave the edge in place so that it resolves to "source missing"
-- instead of silently becoming an edge-less, apparently clean row. source_fences and check_approvals
-- have no foreign key at all: a fence tombstone or an approval must outlive the Session it names.
--
-- No row holds message text, tool arguments, provider echo or hidden reasoning. Bodies are represented
-- by ids, sequence ranges, epochs and digests only. lineage_complete is 0 unless the writer proved the
-- lineage complete; rows that predate this migration simply do not exist, so no legacy message ever
-- acquires a complete lineage.
--
-- The tables are created WITHOUT IF NOT EXISTS: a database that already holds one of these names is not
-- a database this migration may adopt, and it must fail (the runner rolls the transaction back and
-- leaves user_version at 5).

-- One row per provider request that may have been sent. The first persisted state is 'admitted' (the
-- in-memory 'prepared' never reaches disk); a request that was admitted and never answered ends as
-- 'unknown'. answered, failed and unknown are terminal (enforced by internal/session, not by trigger,
-- because the privacy cleanup of ADR-0018 must still be able to clear digests of terminal rows).
-- purpose is validated in Go, not by CHECK, so a later purpose does not need a table rebuild.
CREATE TABLE request_envelopes (
  request_id TEXT PRIMARY KEY CHECK (length(request_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL,
  run_id TEXT CHECK (run_id IS NULL OR length(run_id) BETWEEN 1 AND 128),
  step INTEGER CHECK (step IS NULL OR step >= 0),
  attempt INTEGER CHECK (attempt IS NULL OR attempt >= 0),
  purpose TEXT NOT NULL CHECK (length(purpose) BETWEEN 1 AND 40),
  state TEXT NOT NULL CHECK (state IN ('admitted','answered','failed','unknown')),
  first_seq INTEGER CHECK (first_seq IS NULL OR first_seq >= 0),
  last_seq INTEGER CHECK (last_seq IS NULL OR last_seq >= 0),
  prev_user_seq INTEGER CHECK (prev_user_seq IS NULL OR prev_user_seq >= 0),
  last_assistant_seq INTEGER CHECK (last_assistant_seq IS NULL OR last_assistant_seq >= 0),
  pending_input_digest TEXT CHECK (pending_input_digest IS NULL OR (length(pending_input_digest) = 71 AND substr(pending_input_digest,1,7) = 'sha256:')),
  refs_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(refs_json) AND json_type(refs_json) = 'array' AND length(refs_json) <= 16384),
  tool_specs_digest TEXT CHECK (tool_specs_digest IS NULL OR (length(tool_specs_digest) = 71 AND substr(tool_specs_digest,1,7) = 'sha256:')),
  projection_version INTEGER NOT NULL DEFAULT 0 CHECK (projection_version >= 0),
  lineage_complete INTEGER NOT NULL CHECK (lineage_complete IN (0,1)),
  fence_epoch INTEGER NOT NULL DEFAULT 0 CHECK (fence_epoch >= 0),
  answered_seq INTEGER CHECK (answered_seq IS NULL OR answered_seq >= 0),
  decision_json TEXT CHECK (decision_json IS NULL OR (json_valid(decision_json) AND json_type(decision_json) = 'object' AND length(decision_json) <= 256)),
  reason TEXT CHECK (reason IS NULL OR length(reason) <= 200),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (first_seq IS NULL OR last_seq IS NULL OR first_seq <= last_seq),
  CHECK (answered_seq IS NULL OR state = 'answered'),
  CHECK (decision_json IS NULL OR state = 'answered'),
  CHECK (state NOT IN ('failed','unknown') OR reason IS NOT NULL),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_request_envelopes_session_state ON request_envelopes(session_id, state);
CREATE INDEX idx_request_envelopes_run ON request_envelopes(run_id);

-- Derived-origin edges and side-effect execution records of one consumer session. kind 'execution' is
-- one admitted tool call with its pre/post metadata; the other kinds are edges from a source (source_kind,
-- source_ref) to a derived target (target_ref). origin_class and the source kinds are validated in Go.
-- An execution row starts at outcome 'admitted'; completed, failed and outcome_unknown are terminal.
CREATE TABLE derived_origins (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  run_id TEXT CHECK (run_id IS NULL OR length(run_id) BETWEEN 1 AND 128),
  kind TEXT NOT NULL CHECK (kind IN ('message','file','child_session','artifact','execution')),
  target_ref TEXT NOT NULL CHECK (length(target_ref) BETWEEN 1 AND 512),
  source_kind TEXT NOT NULL CHECK (length(source_kind) BETWEEN 1 AND 40),
  source_ref TEXT NOT NULL CHECK (length(source_ref) BETWEEN 1 AND 512),
  origin_class TEXT NOT NULL CHECK (length(origin_class) BETWEEN 1 AND 40),
  derived_from TEXT CHECK (derived_from IS NULL OR length(derived_from) BETWEEN 1 AND 512),
  support_status TEXT NOT NULL DEFAULT 'unverified' CHECK (support_status IN ('unverified','verified','invalidated')),
  lineage_complete INTEGER NOT NULL CHECK (lineage_complete IN (0,1)),
  execution_id TEXT CHECK (execution_id IS NULL OR length(execution_id) BETWEEN 1 AND 128),
  tool_name TEXT CHECK (tool_name IS NULL OR length(tool_name) BETWEEN 1 AND 128),
  args_digest TEXT CHECK (args_digest IS NULL OR (length(args_digest) = 71 AND substr(args_digest,1,7) = 'sha256:')),
  pre_ref TEXT CHECK (pre_ref IS NULL OR length(pre_ref) BETWEEN 1 AND 512),
  pre_unavailable INTEGER NOT NULL DEFAULT 0 CHECK (pre_unavailable IN (0,1)),
  post_ref TEXT CHECK (post_ref IS NULL OR length(post_ref) BETWEEN 1 AND 512),
  coverage TEXT CHECK (coverage IS NULL OR coverage IN ('complete','partial','unavailable')),
  outcome TEXT CHECK (outcome IS NULL OR outcome IN ('admitted','completed','failed','outcome_unknown')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (kind <> 'execution' OR (execution_id IS NOT NULL AND tool_name IS NOT NULL AND args_digest IS NOT NULL AND outcome IS NOT NULL)),
  CHECK (kind = 'execution' OR (tool_name IS NULL AND args_digest IS NULL AND pre_ref IS NULL AND pre_unavailable = 0 AND post_ref IS NULL AND coverage IS NULL AND outcome IS NULL)),
  CHECK (pre_unavailable = 0 OR pre_ref IS NULL),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_derived_origins_source ON derived_origins(source_kind, source_ref);
CREATE INDEX idx_derived_origins_execution ON derived_origins(execution_id);
CREATE INDEX idx_derived_origins_session ON derived_origins(session_id, kind);

-- Revocation / privacy fences and their body-free tombstones. No foreign key: a tombstone survives the
-- deletion of the Session it names. A row is never deleted by Session deletion; admission reads the
-- (scope_kind, scope_ref) index and so stays O(1). tombstone_json holds ids, ranges and epochs only.
CREATE TABLE source_fences (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('session','source','privacy')),
  scope_ref TEXT NOT NULL CHECK (length(scope_ref) BETWEEN 1 AND 512),
  epoch INTEGER NOT NULL CHECK (epoch >= 1),
  reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 200),
  state TEXT NOT NULL CHECK (state IN ('active','cleanup_pending','cleaned')),
  tombstone_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tombstone_json) AND json_type(tombstone_json) = 'object'),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX idx_source_fences_scope ON source_fences(scope_kind, scope_ref);

-- Host approvals of check sets, check specs and constraint bindings, keyed by the project binding.
-- No foreign key: an approval is host state, not session state. A revoked row stays; a new approval is a
-- new row with a higher approval_epoch.
CREATE TABLE check_approvals (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL CHECK (length(project_id) BETWEEN 1 AND 256),
  kind TEXT NOT NULL CHECK (kind IN ('check_set','check_spec','constraint_binding')),
  check_id TEXT CHECK (check_id IS NULL OR length(check_id) BETWEEN 1 AND 128),
  spec_digest TEXT CHECK (spec_digest IS NULL OR length(spec_digest) BETWEEN 1 AND 128),
  check_set_digest TEXT CHECK (check_set_digest IS NULL OR length(check_set_digest) BETWEEN 1 AND 128),
  constraint_ref TEXT CHECK (constraint_ref IS NULL OR length(constraint_ref) BETWEEN 1 AND 512),
  constraint_digest TEXT CHECK (constraint_digest IS NULL OR length(constraint_digest) BETWEEN 1 AND 128),
  binding_digest TEXT CHECK (binding_digest IS NULL OR length(binding_digest) BETWEEN 1 AND 128),
  approval_epoch INTEGER NOT NULL CHECK (approval_epoch >= 1),
  approved_by TEXT NOT NULL CHECK (length(approved_by) BETWEEN 1 AND 40),
  actor TEXT NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
  decision_ref TEXT CHECK (decision_ref IS NULL OR length(decision_ref) BETWEEN 1 AND 512),
  state TEXT NOT NULL CHECK (state IN ('active','revoked')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (kind <> 'check_set' OR check_set_digest IS NOT NULL),
  CHECK (kind <> 'check_spec' OR (check_id IS NOT NULL AND spec_digest IS NOT NULL)),
  CHECK (kind <> 'constraint_binding' OR (constraint_ref IS NOT NULL AND constraint_digest IS NOT NULL AND check_id IS NOT NULL AND spec_digest IS NOT NULL AND binding_digest IS NOT NULL))
) STRICT;
CREATE INDEX idx_check_approvals_project ON check_approvals(project_id, kind, state);

-- Evaluator episodes (CHECKS-001): the six columns are filled by T4. Legacy rows keep NULL episode_id
-- and so never satisfy a completion gate. Column CHECKs reference only their own column, so
-- 0006_provenance_and_approvals.down.sql can drop them.
ALTER TABLE eval_results ADD COLUMN episode_id TEXT CHECK (episode_id IS NULL OR length(episode_id) BETWEEN 1 AND 128);
ALTER TABLE eval_results ADD COLUMN pre_fingerprint TEXT;
ALTER TABLE eval_results ADD COLUMN post_fingerprint TEXT;
ALTER TABLE eval_results ADD COLUMN spec_digest TEXT;
ALTER TABLE eval_results ADD COLUMN approval_epoch INTEGER CHECK (approval_epoch IS NULL OR approval_epoch >= 0);
ALTER TABLE eval_results ADD COLUMN episode_status TEXT CHECK (episode_status IS NULL OR episode_status IN ('open','closed','scope_changed','spec_unapproved','interrupted'));
CREATE INDEX idx_eval_results_episode ON eval_results(session_id, episode_id);
