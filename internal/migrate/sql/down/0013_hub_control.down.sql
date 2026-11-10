-- 0013_hub_control.down.sql: remove the Hub control plane's durable state. The worker_messages
-- columns are dropped in reverse order of their addition; SQLite supports DROP COLUMN (as 0002 does).
DROP INDEX worker_messages_pending;
ALTER TABLE worker_messages DROP COLUMN attempts;
ALTER TABLE worker_messages DROP COLUMN acked_at;
ALTER TABLE worker_messages DROP COLUMN delivered_at;
ALTER TABLE worker_messages DROP COLUMN delivered_seq;
ALTER TABLE worker_messages DROP COLUMN delivered_run;
ALTER TABLE worker_messages DROP COLUMN delivery_state;
DROP TABLE hub_commands;
DROP TABLE work_unit_leases;
DROP TABLE run_pauses;
DROP TABLE steer_messages;
