package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// RunStart is the durable write set of a Start, applied by BeginRun.
type RunStart struct {
	// Input is appended as the next user row when non-nil.
	Input *protocol.Message
	// Provider and Model replace the session's provider/model identity when non-nil;
	// an empty string clears the column.
	Provider *string
	Model    *string
}

// BeginRun is the Start transaction. The caller holds the session's run lock and
// has already reconciled the transcript and validated everything that can reject
// the Start. In one BEGIN IMMEDIATE transaction it
//
//   - appends Input as a user row, when there is one;
//   - writes the provider/model identity;
//   - sets status = running and clears the previous cause and final.
//
// All of it commits or none of it does, so a Start that fails leaves no new
// transcript row and the session row as it was, and a Start that succeeds is
// already running when the loop begins. It returns the seq of the Input row, or -1.
func (s *Store) BeginRun(ctx context.Context, sessionID string, start RunStart) (inputSeq int64, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return -1, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, ErrNotFound
		}
		return -1, err
	}
	inputSeq = -1
	if start.Input != nil {
		raw, err := json.Marshal(start.Input)
		if err != nil {
			return -1, err
		}
		if err := conn.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages WHERE session_id = ?`, sessionID,
		).Scan(&inputSeq); err != nil {
			return -1, err
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`,
			sessionID, inputSeq, start.Input.Role, string(raw)); err != nil {
			return -1, err
		}
	}
	sets := `status = 'running', final = NULL, error_json = NULL, updated_at = ?`
	args := []any{time.Now().UTC().Format(time.RFC3339Nano)}
	if start.Provider != nil {
		sets += `, provider = NULLIF(?, '')`
		args = append(args, *start.Provider)
	}
	if start.Model != nil {
		sets += `, model = NULLIF(?, '')`
		args = append(args, *start.Model)
	}
	args = append(args, sessionID)
	if _, err := conn.ExecContext(ctx, `UPDATE sessions SET `+sets+` WHERE id = ?`, args...); err != nil {
		return -1, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return -1, err
	}
	committed = true
	return inputSeq, nil
}
