-- 0007_jobs.sql: the durable job queue (RUNTIME-002/003). One row per background command a run, the
-- host or a schedule submitted. The row is the job's source of truth: a state change is one UPDATE
-- guarded by the state it leaves, so two writers never both move a job, and a restart finds every job
-- where its last committed transition left it.
--
-- States: queued -> running -> completed | failed | timed_out | cancelled; queued -> cancelled; and
-- running -> outcome_unknown when the process that ran the job died before recording how it ended (it
-- may have had effects, so it is never run again automatically). completed, failed, timed_out,
-- cancelled and outcome_unknown are terminal.
--
-- command is content (it can carry anything the user or the model wrote): the row belongs to its
-- session and is deleted with it (ON DELETE CASCADE and session purgeTables). Output never lives in the
-- database: it is written, bounded, to files under <state>/jobs/<job_id>/, and only byte counts are kept
-- here. A host job may have no session (session_id NULL).
--
-- runner_id/runner_pid name the job manager (one per open Engine, runner_pid its process) that owns the
-- job: only that manager starts or finishes it. A manager that opens finds the rows of runners whose
-- process is gone: their queued jobs it adopts, their running jobs become outcome_unknown. pid is the
-- process group leader of the running command.
CREATE TABLE jobs (
  job_id TEXT PRIMARY KEY CHECK (length(job_id) BETWEEN 1 AND 64),
  kind TEXT NOT NULL CHECK (kind IN ('shell')),
  source TEXT NOT NULL CHECK (source IN ('agent','host','schedule')),
  session_id TEXT,
  run_id TEXT CHECK (run_id IS NULL OR length(run_id) BETWEEN 1 AND 128),
  tool_call_id TEXT CHECK (tool_call_id IS NULL OR length(tool_call_id) BETWEEN 1 AND 256),
  schedule_id TEXT CHECK (schedule_id IS NULL OR length(schedule_id) BETWEEN 1 AND 64),
  workspace TEXT NOT NULL CHECK (length(workspace) BETWEEN 1 AND 4096),
  command TEXT NOT NULL CHECK (length(command) BETWEEN 1 AND 65536),
  timeout_ms INTEGER NOT NULL DEFAULT 0 CHECK (timeout_ms >= 0),
  state TEXT NOT NULL CHECK (state IN ('queued','running','completed','failed','timed_out','cancelled','outcome_unknown')),
  exit_code INTEGER,
  error TEXT CHECK (error IS NULL OR length(error) <= 4096),
  pid INTEGER CHECK (pid IS NULL OR pid > 0),
  runner_id TEXT NOT NULL CHECK (length(runner_id) BETWEEN 1 AND 64),
  runner_pid INTEGER NOT NULL CHECK (runner_pid > 0),
  stdout_bytes INTEGER NOT NULL DEFAULT 0 CHECK (stdout_bytes >= 0),
  stderr_bytes INTEGER NOT NULL DEFAULT 0 CHECK (stderr_bytes >= 0),
  output_truncated INTEGER NOT NULL DEFAULT 0 CHECK (output_truncated IN (0,1)),
  cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0,1)),
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT,
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX idx_jobs_state ON jobs(state, created_at);
CREATE INDEX idx_jobs_session ON jobs(session_id, created_at);
CREATE INDEX idx_jobs_run ON jobs(run_id) WHERE run_id IS NOT NULL;
