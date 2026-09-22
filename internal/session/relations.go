package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Relation struct {
	ParentSessionID string          `json:"parentSessionId"`
	ChildSessionID  string          `json:"childSessionId"`
	Kind            string          `json:"kind"`
	CreatedAt       time.Time       `json:"createdAt"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

type Checkpoint struct {
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	At        time.Time       `json:"at"`
	Reason    string          `json:"reason"`
	JSON      json.RawMessage `json:"json"`
}

func (s *Store) AddRelation(ctx context.Context, relation Relation) error {
	if relation.ParentSessionID == "" || relation.ChildSessionID == "" {
		return errors.New("parent and child session ids are required")
	}
	if relation.ParentSessionID == relation.ChildSessionID {
		return errors.New("session cannot be related to itself")
	}
	if relation.Kind == "" {
		relation.Kind = "subagent"
	}
	if relation.CreatedAt.IsZero() {
		relation.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO session_relations(child_session_id, parent_session_id, kind, created_at, metadata_json)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(child_session_id) DO UPDATE SET
  parent_session_id = excluded.parent_session_id,
  kind = excluded.kind,
  created_at = excluded.created_at,
  metadata_json = excluded.metadata_json
`,
		relation.ChildSessionID,
		relation.ParentSessionID,
		relation.Kind,
		relation.CreatedAt.Format(time.RFC3339Nano),
		normalizeJSON(relation.Metadata, "{}"),
	)
	return err
}

func (s *Store) ParentRelation(ctx context.Context, childSessionID string) (Relation, error) {
	var relation Relation
	var createdAt, metadata string
	err := s.db.QueryRowContext(ctx, `
SELECT parent_session_id, child_session_id, kind, created_at, metadata_json
FROM session_relations
WHERE child_session_id = ?
`, childSessionID).Scan(
		&relation.ParentSessionID,
		&relation.ChildSessionID,
		&relation.Kind,
		&createdAt,
		&metadata,
	)
	if err != nil {
		return Relation{}, err
	}
	relation.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	relation.Metadata = json.RawMessage(metadata)
	return relation, nil
}

func (s *Store) ChildRelations(ctx context.Context, parentSessionID string, limit int) ([]Relation, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT parent_session_id, child_session_id, kind, created_at, metadata_json
FROM session_relations
WHERE parent_session_id = ?
ORDER BY created_at ASC
LIMIT ?
`, parentSessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Relation, 0, limit)
	for rows.Next() {
		var relation Relation
		var createdAt, metadata string
		if err := rows.Scan(
			&relation.ParentSessionID,
			&relation.ChildSessionID,
			&relation.Kind,
			&createdAt,
			&metadata,
		); err != nil {
			return nil, err
		}
		relation.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		relation.Metadata = json.RawMessage(metadata)
		out = append(out, relation)
	}
	return out, rows.Err()
}

func (s *Store) CountChildren(ctx context.Context, parentSessionID, kind string) (int, error) {
	query := `SELECT COUNT(*) FROM session_relations WHERE parent_session_id = ?`
	args := []any{parentSessionID}
	if kind != "" {
		query += " AND kind = ?"
		args = append(args, kind)
	}
	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *Store) AppendCheckpoint(ctx context.Context, sessionID, reason string, payload any) (Checkpoint, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Checkpoint{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Checkpoint{}, err
	}
	defer tx.Rollback()

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_checkpoints WHERE session_id = ?`,
		sessionID,
	).Scan(&seq); err != nil {
		return Checkpoint{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO session_checkpoints(session_id, seq, at, reason, json)
VALUES(?, ?, ?, ?, ?)
`, sessionID, seq, now.Format(time.RFC3339Nano), reason, string(raw)); err != nil {
		return Checkpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{
		SessionID: sessionID,
		Seq:       seq,
		At:        now,
		Reason:    reason,
		JSON:      json.RawMessage(raw),
	}, nil
}

func (s *Store) Checkpoints(ctx context.Context, sessionID string, limit int) ([]Checkpoint, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT session_id, seq, at, reason, json
FROM session_checkpoints
WHERE session_id = ?
ORDER BY seq DESC
LIMIT ?
`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Checkpoint, 0, limit)
	for rows.Next() {
		var checkpoint Checkpoint
		var at, raw string
		if err := rows.Scan(
			&checkpoint.SessionID,
			&checkpoint.Seq,
			&at,
			&checkpoint.Reason,
			&raw,
		); err != nil {
			return nil, err
		}
		checkpoint.At, _ = time.Parse(time.RFC3339Nano, at)
		checkpoint.JSON = json.RawMessage(raw)
		out = append(out, checkpoint)
	}
	return out, rows.Err()
}
