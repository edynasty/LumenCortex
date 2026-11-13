-- 0014_cognition_support: the support contract of persistent cognition (T5a remediation R1). A candidate
-- records the structured assertion a host-approved binding supports (assertion_json), the binding that
-- qualified it (binding_digest), the identity of its support (claim, source versions, scope, policy and
-- binding: support_digest, which dedupe compares instead of the claim alone) and, after a privacy
-- deletion of a source its maintenance batch read, when its text was purged (purged_at). Every column has
-- a default, so rows 0013 wrote keep their meaning: no assertion, no binding, an empty support identity
-- that never equals a new one, not purged.
ALTER TABLE cognition_candidates ADD COLUMN assertion_json TEXT NOT NULL DEFAULT '';
ALTER TABLE cognition_candidates ADD COLUMN binding_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE cognition_candidates ADD COLUMN support_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE cognition_candidates ADD COLUMN purged_at TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_cognition_candidates_support ON cognition_candidates(support_digest);
