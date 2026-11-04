-- 0008_schedules.sql: timers and scheduled tasks (RUNTIME-004). One row per scheduled task. Each Engine
-- that opens the workspace runs one scheduler over this table; a firing is claimed by one UPDATE guarded
-- by the next_run_at it read (compare-and-set), so two processes sharing the database never fire the
-- same occurrence twice.
--
-- kind 'shell' submits a job (payload_json holds the command and its timeout); kind 'agent' starts a run
-- on session_id with a prompt (payload_json holds the prompt and the run's policy). trigger_kind 'once'
-- fires at run_at; 'interval' fires every interval_ms from its first next_run_at; 'cron' fires at the
-- next time cron_expr matches in timezone (a standard 5-field expression, IANA zone, 'UTC' when the
-- host names none). missed_policy decides what an occurrence that came due while no scheduler ran does:
-- 'skip' advances past it without firing, 'run_once' fires once and then advances. next_run_at is NULL
-- once a one-shot task has fired.
--
-- payload_json is content: a task that names a session is deleted with it (ON DELETE CASCADE and
-- session purgeTables). last_status is validated in Go, not by CHECK, so a later status needs no rebuild.
CREATE TABLE scheduled_tasks (
  schedule_id TEXT PRIMARY KEY CHECK (length(schedule_id) BETWEEN 1 AND 64),
  name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  kind TEXT NOT NULL CHECK (kind IN ('shell','agent')),
  session_id TEXT,
  payload_json TEXT NOT NULL CHECK (json_valid(payload_json) AND json_type(payload_json) = 'object' AND length(payload_json) <= 65536),
  trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('once','interval','cron')),
  run_at TEXT,
  interval_ms INTEGER CHECK (interval_ms IS NULL OR interval_ms >= 1000),
  cron_expr TEXT CHECK (cron_expr IS NULL OR length(cron_expr) BETWEEN 1 AND 200),
  timezone TEXT NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 64),
  missed_policy TEXT NOT NULL DEFAULT 'skip' CHECK (missed_policy IN ('skip','run_once')),
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  next_run_at TEXT,
  last_fired_at TEXT,
  last_status TEXT CHECK (last_status IS NULL OR length(last_status) BETWEEN 1 AND 40),
  last_ref TEXT CHECK (last_ref IS NULL OR length(last_ref) BETWEEN 1 AND 128),
  last_error TEXT CHECK (last_error IS NULL OR length(last_error) <= 4096),
  fire_count INTEGER NOT NULL DEFAULT 0 CHECK (fire_count >= 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (
    (trigger_kind = 'once' AND run_at IS NOT NULL AND interval_ms IS NULL AND cron_expr IS NULL) OR
    (trigger_kind = 'interval' AND interval_ms IS NOT NULL AND cron_expr IS NULL) OR
    (trigger_kind = 'cron' AND cron_expr IS NOT NULL AND interval_ms IS NULL AND run_at IS NULL)),
  CHECK (kind <> 'agent' OR session_id IS NOT NULL),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX idx_scheduled_tasks_due ON scheduled_tasks(enabled, next_run_at);
CREATE INDEX idx_scheduled_tasks_session ON scheduled_tasks(session_id) WHERE session_id IS NOT NULL;
