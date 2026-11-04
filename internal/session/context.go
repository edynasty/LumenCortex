package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	// Fenced is true when a revocation or privacy fence names the session (ADR-0018): its content is
	// withdrawn and a reader shows none of it.
	Fenced bool
}

const contextSelect = `
SELECT s.id, COALESCE(r.parent_session_id, ''), COALESCE(r.kind, ''), s.status, COALESCE(s.agent_id, ''),
       s.goal, substr(COALESCE(s.final, ''), 1, ?), s.updated_at,
       COALESCE(json_extract(t.json, '$.brief'), ''),
       COALESCE(json_extract(t.json, '$.resolution.state'), ''),
       COALESCE(json_extract(t.json, '$.notes'), ''),
       EXISTS(SELECT 1 FROM source_fences f WHERE f.scope_kind = 'session' AND f.scope_ref = s.id)
FROM sessions s
LEFT JOIN session_relations r ON r.child_session_id = s.id
LEFT JOIN task_states t ON t.session_id = s.id
     AND t.version = (SELECT MAX(version) FROM task_states WHERE session_id = s.id)
`

// MaxMatchTerms bounds the query terms ContextRowsMatching filters by.
const MaxMatchTerms = 16

// ContextRows returns the newest sessions by update time (ties by id), at most limit, with the final
// answer cut to finalBytes.
func (s *Store) ContextRows(ctx context.Context, limit, finalBytes int) ([]ContextRow, error) {
	rows, err := s.db.QueryContext(ctx, contextSelect+`ORDER BY s.updated_at DESC, s.id DESC LIMIT ?`, finalBytes, limit)
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

// ContextRowsMatching returns the sessions older than the newest skip (by update time, as
// ContextRows orders them) whose goal, final answer, brief, resolution or notes contain at least one
// of terms, newest first, at most limit. Containment is a case-insensitive (ASCII) substring test, a
// superset of the token match recall ranks by, so a session it leaves out cannot score on an ASCII
// term; at most MaxMatchTerms terms are used. No terms match nothing.
func (s *Store) ContextRowsMatching(ctx context.Context, terms []string, skip, limit, finalBytes int) ([]ContextRow, error) {
	if len(terms) > MaxMatchTerms {
		terms = terms[:MaxMatchTerms]
	}
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	conds := make([]string, len(terms))
	args := []any{skip}
	for i, term := range terms {
		conds[i] = "instr(txt, ?) > 0"
		args = append(args, strings.ToLower(term))
	}
	args = append(args, finalBytes, limit)
	// The text is built once per older session; the newest skip are left to ContextRows.
	query := `WITH old AS (
  SELECT s.id, lower(s.goal || ' ' || COALESCE(s.final, '') || ' ' || COALESCE(json_extract(t.json, '$.brief'), '') || ' ' ||
         COALESCE(json_extract(t.json, '$.resolution.state'), '') || ' ' || COALESCE(json_extract(t.json, '$.notes'), '')) AS txt
  FROM sessions s
  LEFT JOIN task_states t ON t.session_id = s.id
       AND t.version = (SELECT MAX(version) FROM task_states WHERE session_id = s.id)
  WHERE s.id NOT IN (SELECT id FROM sessions ORDER BY updated_at DESC, id DESC LIMIT ?)
), hit AS (SELECT id FROM old WHERE ` + strings.Join(conds, " OR ") + `)
` + contextSelect + `WHERE s.id IN (SELECT id FROM hit)
ORDER BY s.updated_at DESC, s.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContextRow
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
		&row.Brief, &row.Resolution, &notes, &row.Fenced); err != nil {
		return ContextRow{}, err
	}
	row.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if notes != "" {
		row.Notes = json.RawMessage(notes)
	}
	return row, nil
}
