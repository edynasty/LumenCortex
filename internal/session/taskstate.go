package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrTaskStateConflict reports a Task State write whose base is no longer the newest version: another
// writer committed in between. Writers hold the session's run lock, so this means a bug or a foreign
// writer; nothing was written.
var ErrTaskStateConflict = errors.New("task state changed since it was read")

// taskStateHistory is how many versions a write keeps (design §4.4 contract PutTaskState).
const taskStateHistory = 32

// TaskStateRow is one stored Task State version. A session without a version reads as version 0 with
// no JSON.
type TaskStateRow struct {
	Version   int64
	CreatedAt time.Time
	Reason    string
	JSON      json.RawMessage
}

// TaskStateWrite is the next Task State version: Base+1, with the reason and JSON given. InboxThrough,
// when positive, deletes the session's inbox edits with seq <= InboxThrough in the same transaction:
// the edits this version applied.
//
// Edges are the derived edges of the entries this version introduces (a user rewrite of agent text, a
// child the user started from a proposal, a task result delivered with its cursor; plan T1-S5, CR-1).
// They commit with the version in the same transaction, after the same validation, cut at
// MaxEdgesPerCommit and fence re-check as CommitToolResult's edges; a fenced session or edge source
// writes nothing and returns ErrFenced. There is no way to store edges without a body.
type TaskStateWrite struct {
	Base         int64
	Reason       string
	JSON         json.RawMessage
	InboxThrough int64
	Edges        []DerivedEdge
}

func (w TaskStateWrite) validateEdges() error {
	for _, e := range w.Edges {
		if err := e.validate(); err != nil {
			return err
		}
	}
	return nil
}

// InboxRow is one queued host edit.
type InboxRow struct {
	Seq       int64
	CreatedAt time.Time
	JSON      json.RawMessage
}

// querier is the part of *sql.Conn and *sql.Tx a transaction step needs.
type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// LatestTaskState returns the newest version, or version 0 when the session has none.
func (s *Store) LatestTaskState(ctx context.Context, sessionID string) (TaskStateRow, error) {
	return latestTaskState(ctx, s.db, sessionID)
}

func latestTaskState(ctx context.Context, q querier, sessionID string) (TaskStateRow, error) {
	var (
		row          TaskStateRow
		created, raw string
	)
	err := q.QueryRowContext(ctx, `SELECT version, created_at, reason, json FROM task_states
WHERE session_id = ? ORDER BY version DESC LIMIT 1`, sessionID).Scan(&row.Version, &created, &row.Reason, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskStateRow{}, nil
	}
	if err != nil {
		return TaskStateRow{}, err
	}
	row.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	row.JSON = json.RawMessage(raw)
	return row, nil
}

// PutTaskState writes the next version in its own write transaction and returns its number.
func (s *Store) PutTaskState(ctx context.Context, sessionID string, w TaskStateWrite) (int64, error) {
	if err := w.validateEdges(); err != nil {
		return 0, err
	}
	var version int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		var err error
		version, err = putTaskState(ctx, conn, sessionID, w)
		return err
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// putTaskState inserts version w.Base+1, prunes versions older than the newest taskStateHistory,
// consumes the applied inbox edits and stores w.Edges. The caller validated the edges and commits.
func putTaskState(ctx context.Context, q querier, sessionID string, w TaskStateWrite) (int64, error) {
	if w.Reason == "" || !json.Valid(w.JSON) {
		return 0, errors.New("task state write needs a reason and valid JSON")
	}
	if len(w.Edges) > 0 {
		fenced, err := commitFenced(ctx, q, sessionID, w.Edges)
		if err != nil {
			return 0, err
		}
		if fenced {
			return 0, ErrFenced
		}
	}
	var newest int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM task_states WHERE session_id = ?`, sessionID).Scan(&newest); err != nil {
		return 0, err
	}
	if newest != w.Base {
		return 0, fmt.Errorf("%w: base %d, newest %d", ErrTaskStateConflict, w.Base, newest)
	}
	version := w.Base + 1
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := q.ExecContext(ctx, `INSERT INTO task_states(session_id, version, created_at, reason, json) VALUES(?, ?, ?, ?, ?)`,
		sessionID, version, at, w.Reason, string(w.JSON)); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM task_states WHERE session_id = ? AND version <= ?`, sessionID, version-taskStateHistory); err != nil {
		return 0, err
	}
	if w.InboxThrough > 0 {
		if _, err := q.ExecContext(ctx, `DELETE FROM task_state_inbox WHERE session_id = ? AND seq <= ?`, sessionID, w.InboxThrough); err != nil {
			return 0, err
		}
	}
	if err := insertEdges(ctx, q, sessionID, "", "", w.Edges); err != nil {
		return 0, err
	}
	return version, nil
}

// appendMessageRow inserts one transcript row at the session's next seq and touches updated_at. It runs
// on the caller's transaction or connection and takes no other lock.
func appendMessageRow(ctx context.Context, q querier, sessionID, role string, raw []byte) (int64, error) {
	var seq int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`, sessionID, seq, role, string(raw)); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now(), sessionID); err != nil {
		return 0, err
	}
	return seq, nil
}

// AppendMessageTaskState appends one transcript row and, when w is non-nil, the Task State version the
// row caused, in one transaction (design §4.4). It returns the row's seq and the new version (0 when w
// is nil).
func (s *Store) AppendMessageTaskState(ctx context.Context, sessionID, role string, payload any, w *TaskStateWrite) (int64, int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, 0, err
	}
	if w != nil {
		if err := w.validateEdges(); err != nil {
			return 0, 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	seq, err := appendMessageRow(ctx, tx, sessionID, role, raw)
	if err != nil {
		return 0, 0, err
	}
	var version int64
	if w != nil {
		if version, err = putTaskState(ctx, tx, sessionID, *w); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return seq, version, nil
}

// NextMessageSeq is the seq the next transcript row of the session will get.
func (s *Store) NextMessageSeq(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages WHERE session_id = ?`, sessionID).Scan(&seq)
	return seq, err
}

// QueueEdit appends one host edit to the session's inbox. It takes no run lock: a running loop applies
// it at its next step boundary, else the next Start does. The caller validated raw.
func (s *Store) QueueEdit(ctx context.Context, sessionID string, raw json.RawMessage) (int64, error) {
	if !json.Valid(raw) {
		return 0, errors.New("edit is not valid JSON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO task_state_inbox(session_id, created_at, json) VALUES(?, ?, ?)`,
		sessionID, time.Now().UTC().Format(time.RFC3339Nano), string(raw))
	if err != nil {
		return 0, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// InboxEdits returns the session's queued edits in order.
func (s *Store) InboxEdits(ctx context.Context, sessionID string) ([]InboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, created_at, json FROM task_state_inbox WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxRow
	for rows.Next() {
		var (
			r            InboxRow
			created, raw string
		)
		if err := rows.Scan(&r.Seq, &created, &raw); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.JSON = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}
