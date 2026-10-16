-- 0003_task_state.sql: Task State (design §4.4, ADR-0006 D1′/D2).
--
-- task_states holds one row per version of a session's durable task memory; the newest version is the
-- current state and a session without a row has the empty state (version 0). task_state_inbox holds host
-- edits (brief, references, resolution) that a run applies at its next step boundary or Start.
--
-- Version 1 of every session is built from the legacy loop metadata keys, which are then removed. A
-- session that never ran gets its goal as first transcript row, and windowStartSeq moves past the newest
-- gap that another row follows. Node databases never reach this file (§6.1), so it carries no Node
-- normalization.
CREATE TABLE task_states (
  session_id TEXT NOT NULL,
  version INTEGER NOT NULL CHECK (version >= 1),
  created_at TEXT NOT NULL,
  reason TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  PRIMARY KEY (session_id, version),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

-- seq orders edits across the database; an applied edit is deleted in the transaction of the Task State
-- version that applied it.
CREATE TABLE task_state_inbox (
  seq INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_task_state_inbox_session ON task_state_inbox(session_id, seq);

-- Version 1 from the legacy keys (reason "migrated"). The goal is not copied: sessions.goal stays its only
-- store. The nested CASE keeps json_type away from invalid JSON; json_patch drops null members.
INSERT INTO task_states (session_id, version, created_at, reason, json)
SELECT id, 1, updated_at, 'migrated',
  json_patch(
    json_object('windowStartSeq', 0),
    CASE WHEN json_valid(metadata_json) THEN json_object(
      'workUnits', CASE WHEN json_type(metadata_json, '$.workUnits') = 'object' THEN metadata_json -> '$.workUnits' END,
      'workflow',  CASE WHEN json_type(metadata_json, '$.workflow') = 'object' THEN metadata_json -> '$.workflow' END,
      'progress',  CASE WHEN json_type(metadata_json, '$.cognition.progress') = 'object' THEN metadata_json -> '$.cognition.progress' END
    ) ELSE '{}' END
  )
FROM sessions;

-- The first transcript row is the session's first user message; NewSession writes it from now on. A legacy
-- session that never ran has none (the old loop added the goal at its first run unless strings.TrimSpace
-- left it empty), so it gets its goal here. The char() list is the set unicode.IsSpace accepts, which is what
-- TrimSpace removes, so this rule and NewSession's agree (TestGoalRowFollowsTrimSpace).
INSERT INTO session_messages (session_id, seq, role, json)
SELECT s.id, 0, 'user', json_object('role', 'user', 'content', s.goal)
FROM sessions s
WHERE trim(s.goal, char(9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199,
                        8200, 8201, 8202, 8232, 8233, 8239, 8287, 12288)) <> ''
  AND NOT EXISTS (SELECT 1 FROM session_messages m WHERE m.session_id = s.id);

-- windowStartSeq: an assistant row `a` with calls has stop(a) = the smallest later seq of the session whose
-- role is not 'tool'. A non-tail gap is an `a` whose stop exists and that leaves a call id without a 'tool'
-- row in (a.seq, stop); a window starting before it would hold an orphan or a missing result, so
-- windowStartSeq moves to the largest stop over all non-tail gaps. Tail gaps (no stop) stay: Reconcile
-- closes them at the next Start.
WITH gaps(sid, start) AS (
  SELECT g.sid, max(g.stop) FROM (
    SELECT a.session_id AS sid, a.seq AS aseq, a.json AS ajson,
      (SELECT min(s.seq) FROM session_messages s
        WHERE s.session_id = a.session_id AND s.seq > a.seq AND s.role <> 'tool') AS stop
    FROM session_messages a
    WHERE a.role = 'assistant'
      AND json_valid(a.json)
      AND json_type(a.json, '$.tool_calls') = 'array'
      AND json_array_length(a.json, '$.tool_calls') > 0
  ) AS g
  WHERE g.stop IS NOT NULL
    AND EXISTS (
      SELECT 1 FROM json_each(g.ajson, '$.tool_calls') AS tc
      WHERE NOT EXISTS (
        SELECT 1 FROM session_messages t
        WHERE t.session_id = g.sid AND t.role = 'tool'
          AND t.seq > g.aseq AND t.seq < g.stop
          AND t.json ->> '$.tool_call_id' = tc.value ->> '$.id'
      )
    )
  GROUP BY g.sid
)
UPDATE task_states
SET json = json_set(json, '$.windowStartSeq', (SELECT start FROM gaps WHERE gaps.sid = task_states.session_id))
WHERE version = 1 AND session_id IN (SELECT sid FROM gaps);

-- Remove the moved keys; every other metadata key stays untouched.
UPDATE sessions
SET metadata_json = json_remove(metadata_json, '$.workUnits', '$.workflow', '$.cognition')
WHERE json_valid(metadata_json)
  AND (json_type(metadata_json, '$.workUnits') IS NOT NULL
    OR json_type(metadata_json, '$.workflow') IS NOT NULL
    OR json_type(metadata_json, '$.cognition') IS NOT NULL);
