-- 0003_task_state.down.sql: undo 0003 for a binary that predates Task State (version 2).
--
-- The newest Task State's workUnits, workflow and progress go back into the metadata keys those binaries
-- read (workUnits, workflow, cognition.progress); json_patch drops null members, so a key the Task State
-- does not have is not written. Task State history, windowStartSeq, brief, references, resolution and
-- pending inbox edits are lost (design §8.4). The goal rows 0003 added stay: older binaries add one only to
-- an empty transcript.
UPDATE sessions
SET metadata_json = json_patch(
  CASE WHEN json_valid(metadata_json) THEN metadata_json ELSE '{}' END,
  json_object(
    'workUnits', CASE WHEN json_type(ts.json, '$.workUnits') = 'object' THEN ts.json -> '$.workUnits' END,
    'workflow',  CASE WHEN json_type(ts.json, '$.workflow') = 'object' THEN ts.json -> '$.workflow' END,
    'cognition', CASE WHEN json_type(ts.json, '$.progress') = 'object' THEN json_object('progress', ts.json -> '$.progress') END
  ))
FROM (
  SELECT t.session_id, t.json FROM task_states t
  WHERE t.version = (SELECT max(version) FROM task_states WHERE session_id = t.session_id)
) AS ts
WHERE ts.session_id = sessions.id;

DROP INDEX idx_task_state_inbox_session;
DROP TABLE task_state_inbox;
DROP TABLE task_states;
