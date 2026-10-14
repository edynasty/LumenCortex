-- 0002_model_ref.sql: catalog model ref "<provider>/<model>". No backfill: legacy rows keep NULL
-- (provider/model columns hold the provider id + upstream modelID, not the catalog ref).
ALTER TABLE sessions ADD COLUMN model_ref TEXT CHECK (model_ref IS NULL OR (instr(model_ref,'/') > 1 AND instr(substr(model_ref, instr(model_ref,'/')+1),'/') = 0 AND length(model_ref) > instr(model_ref,'/')));
