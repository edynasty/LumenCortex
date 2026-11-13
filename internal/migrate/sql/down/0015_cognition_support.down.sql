DROP INDEX idx_cognition_candidates_support;
ALTER TABLE cognition_candidates DROP COLUMN purged_at;
ALTER TABLE cognition_candidates DROP COLUMN support_digest;
ALTER TABLE cognition_candidates DROP COLUMN binding_digest;
ALTER TABLE cognition_candidates DROP COLUMN assertion_json;
