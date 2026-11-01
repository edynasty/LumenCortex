-- 0009_notebooks.down.sql: drop the working notebooks for a binary that predates them. Bodies and
-- revisions are lost (the runner backs the database up first).
DROP INDEX idx_notebooks_session;
DROP TABLE notebook_revisions;
DROP TABLE notebooks;
