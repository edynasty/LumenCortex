-- 0005_agent.down.sql: drop the session agent for a binary that predates it. The column-level CHECK
-- references only agent_id, so SQLite drops it with the column (as 0002_model_ref.down.sql).
ALTER TABLE sessions DROP COLUMN agent_id;
