-- 0002_model_ref.down.sql: drop the catalog model ref for a binary that predates it.
-- The column's column-level CHECK references only model_ref, so SQLite drops it together with the
-- column (verified: ALTER TABLE ... DROP COLUMN succeeds and the rebuilt schema has no CHECK left).
ALTER TABLE sessions DROP COLUMN model_ref;
