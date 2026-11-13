-- Refuse data loss and an older reader reviving pre-deletion observations.
CREATE TEMP TABLE p0_downgrade_guard (safe INTEGER CHECK (safe = 1));
INSERT INTO p0_downgrade_guard(safe)
SELECT CASE WHEN EXISTS(SELECT 1 FROM cognition_nodes) OR EXISTS(SELECT 1 FROM cognition_edges)
  OR EXISTS(SELECT 1 FROM source_fences WHERE scope_kind = 'privacy') THEN 0 ELSE 1 END;
DROP TABLE p0_downgrade_guard;
DROP TABLE cognition_edges;
DROP TABLE cognition_nodes;
ALTER TABLE eval_results DROP COLUMN cognition_epoch;
