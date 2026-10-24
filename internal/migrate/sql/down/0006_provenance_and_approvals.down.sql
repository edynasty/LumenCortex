-- 0006_provenance_and_approvals.down.sql: drop request provenance, derived origins, source fences and
-- check approvals for a binary that predates them. Provenance and approval records are lost (the runner
-- backs the database up first); episode columns go with them. Indexes first, then tables, then columns;
-- the column-level CHECKs reference only their own column, so SQLite drops them with it (as
-- 0005_agent.down.sql).
DROP INDEX idx_eval_results_episode;
DROP INDEX idx_check_approvals_project;
DROP INDEX idx_source_fences_scope;
DROP INDEX idx_derived_origins_session;
DROP INDEX idx_derived_origins_execution;
DROP INDEX idx_derived_origins_source;
DROP INDEX idx_request_envelopes_run;
DROP INDEX idx_request_envelopes_session_state;
DROP TABLE check_approvals;
DROP TABLE source_fences;
DROP TABLE derived_origins;
DROP TABLE request_envelopes;
ALTER TABLE eval_results DROP COLUMN episode_status;
ALTER TABLE eval_results DROP COLUMN approval_epoch;
ALTER TABLE eval_results DROP COLUMN spec_digest;
ALTER TABLE eval_results DROP COLUMN post_fingerprint;
ALTER TABLE eval_results DROP COLUMN pre_fingerprint;
ALTER TABLE eval_results DROP COLUMN episode_id;
