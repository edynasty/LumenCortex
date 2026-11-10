-- 0013: the Agent Hub control plane's durable state (docs/reports/harness-evolution-hub-20261110.md).
-- Metadata and host-authored text only: no model reasoning and no transcript content is duplicated
-- here. Every row belongs to a session and is removed with it.

-- Free-text steer: one host-authored instruction per command id. A run applies it at the next request
-- window it builds (the fix for F-STEER-1 also applies one queued behind a BeforeModelCall pause) and
-- records the transcript row it became, so application is idempotent across restarts.
CREATE TABLE steer_messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL CHECK(length(run_id) <= 128),
  command_id TEXT NOT NULL CHECK(length(command_id) BETWEEN 1 AND 128),
  body TEXT NOT NULL CHECK(length(CAST(body AS BLOB)) BETWEEN 1 AND 4096),
  created_at TEXT NOT NULL,
  applied_at TEXT NOT NULL DEFAULT '',
  applied_seq INTEGER NOT NULL DEFAULT -1 CHECK(applied_seq >= -1),
  UNIQUE(session_id, command_id)
) STRICT;
CREATE INDEX steer_messages_pending ON steer_messages(session_id, applied_at, seq);

-- The pause a run is held at, projected after a restart as interrupted(paused). One row per session:
-- the newest pause of the current run. Cleared when the run resumes, ends or a new Start runs.
CREATE TABLE run_pauses (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL CHECK(length(run_id) BETWEEN 1 AND 128),
  reason TEXT NOT NULL CHECK(length(reason) <= 128),
  boundary TEXT NOT NULL CHECK(length(boundary) <= 64),
  step INTEGER NOT NULL DEFAULT 0,
  paused_at TEXT NOT NULL
) STRICT;

-- Task Lease: one valid owner per work unit of a Hub task DAG. The lease expires, so a dead holder's
-- unit returns to ready without a scheduler; the holder renews at its step boundaries.
CREATE TABLE work_unit_leases (
  root_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL CHECK(length(unit_id) BETWEEN 1 AND 128),
  holder TEXT NOT NULL CHECK(length(holder) BETWEEN 1 AND 128),
  lease_id TEXT NOT NULL CHECK(length(lease_id) BETWEEN 1 AND 128),
  expires_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(root_id, unit_id)
) STRICT;
CREATE INDEX work_unit_leases_holder ON work_unit_leases(holder);
CREATE INDEX work_unit_leases_expiry ON work_unit_leases(expires_at);

-- Host command idempotency: a retried CommandID returns the result it produced, never a second effect.
-- Bounded by retention below; a command result carries observable metadata only.
CREATE TABLE hub_commands (
  command_id TEXT PRIMARY KEY CHECK(length(command_id) BETWEEN 1 AND 128),
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL DEFAULT '' CHECK(length(run_id) <= 128),
  verb TEXT NOT NULL CHECK(length(verb) BETWEEN 1 AND 32),
  result TEXT NOT NULL CHECK(json_valid(result) AND length(CAST(result AS BLOB)) <= 16384),
  created_at TEXT NOT NULL
) STRICT;
CREATE INDEX hub_commands_session ON hub_commands(session_id, created_at);

-- Push delivery of worker messages (0012): queued -> delivered (written into a request window) ->
-- acked (the recipient confirmed it). delivered_run makes an unacked redelivery once per run.
ALTER TABLE worker_messages ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'queued'
  CHECK (delivery_state IN ('queued','delivered','acked'));
ALTER TABLE worker_messages ADD COLUMN delivered_run TEXT NOT NULL DEFAULT '';
ALTER TABLE worker_messages ADD COLUMN delivered_seq INTEGER NOT NULL DEFAULT -1;
ALTER TABLE worker_messages ADD COLUMN delivered_at TEXT NOT NULL DEFAULT '';
ALTER TABLE worker_messages ADD COLUMN acked_at TEXT NOT NULL DEFAULT '';
ALTER TABLE worker_messages ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0);
CREATE INDEX worker_messages_pending ON worker_messages(recipient_id, delivery_state, seq);
