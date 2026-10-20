-- 0005_agent.sql: the agent a session runs (ADR-0009), pinned like model_ref. No backfill: legacy rows
-- keep NULL and get the default agent on their next Start without moving their model pin.
ALTER TABLE sessions ADD COLUMN agent_id TEXT CHECK (agent_id IS NULL OR (length(agent_id) BETWEEN 1 AND 40 AND agent_id NOT GLOB '*[^a-z0-9-]*' AND substr(agent_id,1,1) BETWEEN 'a' AND 'z'));
