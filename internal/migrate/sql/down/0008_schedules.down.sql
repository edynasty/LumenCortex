-- 0008_schedules.down.sql: drop scheduled tasks for a binary that predates them. Rows are lost (the
-- runner backs the database up first).
DROP INDEX idx_scheduled_tasks_session;
DROP INDEX idx_scheduled_tasks_due;
DROP TABLE scheduled_tasks;
