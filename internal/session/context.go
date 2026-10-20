package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ContextRow is what recall and read_thread read of one session (ADR-0010 D2): its identity, goal,
// status, agent and final answer, its parent relation, and the brief, resolution state and notes of
// its newest Task State version. Only those Task State fields are extracted, so a large workflow
// snapshot is never loaded.
type ContextRow struct {
	ID         string
	ParentID   string
	Kind       string
	Status     string
	AgentID    string
	Goal       string
	Final      string
	UpdatedAt  time.Time
	Brief      string
	Resolution string
	Notes      json.RawMessage
}

const contextSelect = `
SELECT s.id, COALESCE(r.parent_session_id, ''), COALESCE(r.kind, ''), s.status, COALESCE(s.agent_id, ''),
       s.goal, substr(COALESCE(s.final, ''), 1, ?), s.updated_at,
       COALESCE(json_extract(t.json, '$.brief'), ''),
       COALESCE(json_extract(t.json, '$.resolution.state'), ''),
       COALESCE(json_extract(t.json, '$.notes'), '')
FROM sessions s
LEFT JOIN session_relations r ON r.child_session_id = s.id
LEFT JOIN task_states t ON t.session_id = s.id
     AND t.version = (SELECT MAX(version) FROM task_states WHERE session_id = s.id)
`

// ContextRows returns the newest sessions by update time, at most limit, with the final answer cut
// to finalBytes.
func (s *Store) ContextRows(ctx context.Context, limit, finalBytes int) ([]ContextRow, error) {
	rows, err := s.db.QueryContext(ctx, contextSelect+`ORDER BY s.updated_at DESC LIMIT ?`, finalBytes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ContextRow, 0, min(limit, 64))
	for rows.Next() {
		row, err := scanContextRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ContextRow returns one session's row, ErrNotFound when there is none.
func (s *Store) ContextRow(ctx context.Context, id string, finalBytes int) (ContextRow, error) {
	row, err := scanContextRow(s.db.QueryRowContext(ctx, contextSelect+`WHERE s.id = ?`, finalBytes, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ContextRow{}, ErrNotFound
	}
	return row, err
}

type scanner interface{ Scan(...any) error }

func scanContextRow(sc scanner) (ContextRow, error) {
	var row ContextRow
	var updated, notes string
	if err := sc.Scan(&row.ID, &row.ParentID, &row.Kind, &row.Status, &row.AgentID, &row.Goal, &row.Final, &updated,
		&row.Brief, &row.Resolution, &notes); err != nil {
		return ContextRow{}, err
	}
	row.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if notes != "" {
		row.Notes = json.RawMessage(notes)
	}
	return row, nil
}
