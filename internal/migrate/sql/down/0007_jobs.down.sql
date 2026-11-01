-- 0007_jobs.down.sql: drop the job queue for a binary that predates it. Job rows are lost (the runner
-- backs the database up first); output files under <state>/jobs are left for the newer binary.
DROP INDEX idx_jobs_run;
DROP INDEX idx_jobs_session;
DROP INDEX idx_jobs_state;
DROP TABLE jobs;
