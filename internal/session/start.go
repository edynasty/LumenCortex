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
	// Input is appended as the next user row when non-nil. Without it, a transcript whose newest row
	// is not a user row gets a runtime resume row instead, so a turn never spans runs (design §3.2, §4.6).
	Input *protocol.Message
	// Provider and Model replace the session's provider/model identity when non-nil;
	// an empty string clears the column.
	Provider *string
	Model    *string
	// ModelRef pins the session to a catalog model when non-nil. The runtime sets it for an
	// explicit change (model.user) and for the first Start of a session that has no pin
	// (model.default); a continuation on the pinned model leaves it nil.
	ModelRef *string
	// TaskState is the Start's Task State version (reason "start": seeds from the run options and
	// the queued inbox edits it applied), when the Start changes Task State.
	TaskState *TaskStateWrite
	// Results are runtime rows (origin task_result) appended before Input. The Task State version
	// that advances their delivery cursor is TaskState, so rows and cursor commit together.
	Results []protocol.Message
}

// BeginRun is the Start transaction. The caller holds the session's run lock and
// has already reconciled the transcript and validated everything that can reject
// the Start. In one BEGIN IMMEDIATE transaction it
//   - appends Results, then Input as a user row when there is one, else a resume row after a
//     non-user newest row;
//   - writes the provider/model identity;
//   - writes the Start's Task State version and consumes the inbox edits it applied;
//   - sets status = running and clears the previous cause and final.
//
// All of it commits or none of it does, so a Start that fails leaves no new
// transcript row and the session row as it was, and a Start that succeeds is
// already running when the loop begins. It returns the session row as committed
// (read inside the transaction, so the caller needs no read after the commit) and
// the seqs of the rows it appended, in order: one per Result, then Input or resume when appended.
//
// COMMIT runs without the caller's cancellation: once BeginRun returns, the commit
// is known to have happened, and an error means it did not.
func (s *Store) BeginRun(ctx context.Context, sessionID string, start RunStart) (Session, []int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Session{}, nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Session{}, nil, err
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
			return Session{}, nil, ErrNotFound
		}
		return Session{}, nil, err
	}
	next := int64(0)
	var newestRole sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), -1) + 1, (SELECT role FROM session_messages WHERE session_id = ? ORDER BY seq DESC LIMIT 1)
		 FROM session_messages WHERE session_id = ?`, sessionID, sessionID,
	).Scan(&next, &newestRole); err != nil {
		return Session{}, nil, err
	}
	rows := append([]protocol.Message(nil), start.Results...)
	if len(rows) > 0 {
		newestRole = sql.NullString{String: "user", Valid: true}
	}
	if start.Input != nil {
		rows = append(rows, *start.Input)
	} else if newestRole.Valid && newestRole.String != "user" {
		rows = append(rows, protocol.Message{Role: "user", Origin: protocol.OriginResume, Content: protocol.ResumeContent})
	}
	seqs := make([]int64, 0, len(rows))
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return Session{}, nil, err
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`,
			sessionID, next, row.Role, string(raw)); err != nil {
			return Session{}, nil, err
		}
		seqs = append(seqs, next)
		next++
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
	if start.ModelRef != nil {
		sets += `, model_ref = NULLIF(?, '')`
		args = append(args, *start.ModelRef)
	}
	args = append(args, sessionID)
	if _, err := conn.ExecContext(ctx, `UPDATE sessions SET `+sets+` WHERE id = ?`, args...); err != nil {
		return Session{}, nil, err
	}
	if start.TaskState != nil {
		if _, err := putTaskState(ctx, conn, sessionID, *start.TaskState); err != nil {
			return Session{}, nil, err
		}
	}
	// Read the row that is about to commit; this is the last fallible step before it.
	row, err := getSession(ctx, conn, sessionID)
	if err != nil {
		return Session{}, nil, err
	}
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), `COMMIT`); err != nil {
		return Session{}, nil, err
	}
	committed = true
	return row, seqs, nil
}
