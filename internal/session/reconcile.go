package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// Reconcile cause written when a row still says "running" while its run lock is free.
const (
	KindStaleRunRecovered = "stale_run_recovered"
	staleRunMessage       = "previous process ended while the agent run was active"

	// InterruptedToolResult is the content of the tool row Reconcile appends for a
	// call whose result was never saved.
	InterruptedToolResult = "run ended before the tool result was saved; re-check workspace state before repeating side effects"
)

// Reconcile repairs the crash and cancel residue of a session. The caller holds
// the session's run lock. In one BEGIN IMMEDIATE transaction it:
//
//   - closes a tail gap: the newest assistant row with tool calls, followed only
//     by tool rows, gets a runtime tool row (origin tool_interrupted) for every
//     call that has no result;
//   - changes a session that still says "running" to interrupted/stale_run_recovered.
//     Any other status and its cause are kept.
//   - settles the provenance residue of the crash (ADR-0018): a request envelope that is still
//     'admitted' becomes 'unknown' (it may have reached the provider; it is never resent and never
//     becomes failed), and an execution that is still 'admitted', so has no post record, becomes
//     'outcome_unknown' with lineage_complete = 0. Rows already in a terminal state are not touched,
//     so a second Reconcile changes nothing, and no tool is executed again.
//
// A gap followed by any non-tool row is not repairable: a result appended after
// that row would be an orphan. It is left alone.
//
// It returns the number of tool rows appended.
func (s *Store) Reconcile(ctx context.Context, sessionID string) (inserted int, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	var status string
	if err := conn.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	rows, err := conn.QueryContext(ctx, `SELECT seq, role, json FROM session_messages WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return 0, err
	}
	type row struct {
		seq  int64
		role string // the role column, which is authoritative for rows whose JSON omits it
		msg  protocol.Message
	}
	var messages []row
	for rows.Next() {
		var r row
		var raw string
		if err := rows.Scan(&r.seq, &r.role, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal([]byte(raw), &r.msg); err != nil {
			rows.Close()
			return 0, err
		}
		messages = append(messages, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].role == "assistant" && len(messages[i].msg.ToolCalls) > 0 {
			last = i
			break
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if last >= 0 {
		answered := map[string]bool{}
		tail := true
		for _, r := range messages[last+1:] {
			if r.role != "tool" {
				tail = false
				break
			}
			answered[r.msg.ToolCallID] = true
		}
		if tail {
			next := messages[len(messages)-1].seq + 1
			for _, call := range messages[last].msg.ToolCalls {
				if answered[call.ID] {
					continue
				}
				body, err := json.Marshal(protocol.Message{
					Role: "tool", ToolCallID: call.ID, Name: call.Name,
					Content: InterruptedToolResult, Origin: protocol.OriginToolInterrupted,
				})
				if err != nil {
					return 0, err
				}
				if _, err := conn.ExecContext(ctx,
					`INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, 'tool', ?)`,
					sessionID, next, string(body)); err != nil {
					return 0, err
				}
				next++
				inserted++
			}
		}
	}
	if err := reconcileProvenance(ctx, conn, sessionID, now); err != nil {
		return 0, err
	}
	if status == "running" {
		cause, err := json.Marshal(map[string]any{"kind": KindStaleRunRecovered, "message": staleRunMessage, "at": now})
		if err != nil {
			return 0, err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE sessions SET status = 'interrupted', error_json = ?, updated_at = ? WHERE id = ?`,
			string(cause), now, sessionID); err != nil {
			return 0, err
		}
	} else if inserted > 0 {
		if _, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
			return 0, err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return inserted, nil
}

// RunningSessionIDs lists the sessions whose durable status is "running".
func (s *Store) RunningSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CreateWithMessage inserts the session and its first transcript row in one
// transaction.
func (s *Store) CreateWithMessage(ctx context.Context, v Session, first protocol.Message) error {
	raw, err := json.Marshal(first)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertSession(ctx, tx, v); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, 0, ?, ?)`,
		v.ID, first.Role, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateDerivedSession is CreateWithMessage for a first row Core composed from other sessions'
// content: in the same write transaction it records that row as derived from each of sources (one
// message edge per source, as of the source's newest seq), so a revocation of a source reaches the new
// session (revocationReach) and fences its copy. A source fenced or deleted since the caller read it
// refuses the creation with ErrFenced or ErrNotFound; nothing is written.
func (s *Store) CreateDerivedSession(ctx context.Context, v Session, first protocol.Message, sources []string) error {
	if len(sources) > MaxEdgesPerCommit {
		return invalid("a derived session cites at most %d sources", MaxEdgesPerCommit)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		edges := make([]DerivedEdge, 0, len(sources))
		for _, id := range sources {
			if err := sessionExists(ctx, conn, id); err != nil {
				return err
			}
			if _, fenced, err := sessionFence(ctx, conn, id); err != nil {
				return err
			} else if fenced {
				return fmt.Errorf("%w: source %s", ErrFenced, id)
			}
			var seq int64
			if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM session_messages WHERE session_id = ?`, id).Scan(&seq); err != nil {
				return err
			}
			edges = append(edges, DerivedEdge{
				Kind: "message", TargetRef: fmt.Sprintf("message:%s:0", v.ID), SourceKind: SourceSession, SourceRef: id,
				OriginClass: OriginAgentGen, DerivedFrom: fmt.Sprintf("session:%s@%d", id, seq), LineageComplete: true,
			})
		}
		for _, e := range edges {
			if err := e.validate(); err != nil {
				return err
			}
		}
		if err := insertSession(ctx, conn, v); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, 0, ?, ?)`,
			v.ID, first.Role, string(raw)); err != nil {
			return err
		}
		return insertEdges(ctx, conn, v.ID, "", "", edges)
	})
}

// Delete removes the sessions in one transaction; their messages, steps, relations
// and checkpoints follow by cascade. The first id is the session being deleted: it
// must exist (else ErrNotFound); the rest are descendants deleted with it. The caller
// holds the run lock of every id.
func (s *Store) Delete(ctx context.Context, sessionIDs ...string) error {
	if len(sessionIDs) == 0 {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range sessionIDs {
		result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 && i == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// reconcileProvenance settles the envelopes and executions a crashed or cancelled run left admitted.
// It runs on Reconcile's transaction and connection; the caller holds the session's run lock, so no
// live run of this session can own an admitted row. Only 'admitted' rows are selected, which is what
// makes it idempotent and leaves every terminal row as it was.
func reconcileProvenance(ctx context.Context, conn *sql.Conn, sessionID, at string) error {
	if _, err := conn.ExecContext(ctx,
		`UPDATE request_envelopes SET state = 'unknown', reason = ?, updated_at = ? WHERE session_id = ? AND state = 'admitted'`,
		ReasonRestartNoAnswer, at, sessionID); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx,
		`UPDATE derived_origins SET outcome = 'outcome_unknown', lineage_complete = 0, updated_at = ?
		 WHERE session_id = ? AND kind = 'execution' AND outcome = 'admitted'`, at, sessionID)
	return err
}
