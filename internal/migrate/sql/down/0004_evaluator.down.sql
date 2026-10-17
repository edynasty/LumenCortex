-- 0004_evaluator.down.sql: older binaries have no evaluator; results and artifacts are dropped.
DROP INDEX idx_eval_results_check;
DROP TABLE eval_results;
DROP TABLE artifacts;
