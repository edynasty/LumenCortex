-- An older binary cannot preserve application evidence, decisions, or the new
-- revision/dependency/retention semantics. Refuse rather than silently lose them.
CREATE TEMP TABLE cognitive_completion_down_guard (safe INTEGER CHECK (safe=1));
INSERT INTO cognitive_completion_down_guard(safe)
SELECT CASE WHEN EXISTS(SELECT 1 FROM cognition_applications)
 OR EXISTS(SELECT 1 FROM cognition_proposals)
 OR EXISTS(SELECT 1 FROM cognition_decisions)
 OR EXISTS(SELECT 1 FROM cognition_access)
 OR EXISTS(SELECT 1 FROM cognition_nodes n WHERE json_type(n.json,'$.memory') IS NOT NULL
   OR json_type(n.json,'$.dependencies') IS NOT NULL OR json_type(n.json,'$.revisionReason') IS NOT NULL
   OR json_type(n.json,'$.epistemic') IS NOT NULL OR json_type(n.json,'$.modelConfidence') IS NOT NULL
   OR EXISTS(SELECT 1 FROM json_each(n.json,'$.experience.uses') u WHERE json_type(u.value,'$.runId') IS NOT NULL
     OR json_type(u.value,'$.feedback.applicationId') IS NOT NULL))
 THEN 0 ELSE 1 END;
DROP TABLE cognitive_completion_down_guard;
DROP TABLE cognition_access;
DROP TABLE cognition_decisions;
DROP TABLE cognition_proposals;
DROP TABLE cognition_applications;
DROP INDEX idx_cognition_nodes_kind;
