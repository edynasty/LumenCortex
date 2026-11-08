package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/diag"
)

const MaxWorkerMessages = 1000

type WorkerMessage struct {
	Seq         int64      `json:"seq"`
	RootID      string     `json:"rootId"`
	SenderID    string     `json:"senderId"`
	RecipientID string     `json:"recipientId"`
	ID          string     `json:"id"`
	Body        string     `json:"body,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReceivedAt  *time.Time `json:"receivedAt,omitempty"`
	Withdrawn   bool       `json:"withdrawn,omitempty"`
}
type WorkerMessagePage struct {
	Messages []WorkerMessage `json:"messages"`
	Cursor   int64           `json:"cursor"`
	HasMore  bool            `json:"hasMore"`
}

// Membership and source fences are checked in the same transaction as admission.
// Only this root or its direct workers participate; no foreign session is edited.
func workerMember(ctx context.Context, q rowQuerier, root, id string) error {
	for _, value := range []string{root, id} {
		if err := sessionExists(ctx, q, value); err != nil {
			return err
		}
		if _, fenced, err := sessionFence(ctx, q, value); err != nil {
			return err
		} else if fenced {
			return ErrFenced
		}
	}
	if id == root {
		return nil
	}
	var found int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM session_relations WHERE parent_session_id=? AND child_session_id=? AND kind IN ('execution','subagent')`, root, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func (s *Store) SendWorkerMessage(ctx context.Context, m WorkerMessage) (WorkerMessage, error) {
	if err := checkID("message id", m.ID); err != nil {
		return m, err
	}
	if len(m.ID) > 128 || strings.TrimSpace(m.Body) == "" || len(m.Body) > 8192 || !utf8.ValidString(m.Body) || m.SenderID == m.RecipientID {
		return m, ErrInvalid
	}
	m.Body = diag.MaskSecrets(m.Body)
	m.CreatedAt = time.Now().UTC()
	m.ReceivedAt = nil
	m.Withdrawn = false
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := workerMember(ctx, conn, m.RootID, m.SenderID); err != nil {
			return err
		}
		if err := workerMember(ctx, conn, m.RootID, m.RecipientID); err != nil {
			return err
		}
		var prior WorkerMessage
		var created, received string
		err := conn.QueryRowContext(ctx, `SELECT seq,recipient_id,body,created_at,received_at FROM worker_messages WHERE session_id=? AND sender_id=? AND message_id=?`, m.RootID, m.SenderID, m.ID).Scan(&prior.Seq, &prior.RecipientID, &prior.Body, &created, &received)
		if err == nil {
			if prior.RecipientID != m.RecipientID || prior.Body != m.Body {
				return ErrExists
			}
			m.Seq = prior.Seq
			m.CreatedAt = parseTime(created)
			if received != "" {
				at := parseTime(received)
				m.ReceivedAt = &at
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM worker_messages WHERE session_id=?`, m.RootID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxWorkerMessages {
			return errors.New("Hub message retention limit reached")
		}
		result, err := conn.ExecContext(ctx, `INSERT INTO worker_messages(session_id,sender_id,recipient_id,message_id,body,created_at) VALUES(?,?,?,?,?,?)`, m.RootID, m.SenderID, m.RecipientID, m.ID, m.Body, m.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		m.Seq, err = result.LastInsertId()
		return err
	})
	return m, err
}

// ReadWorkerMessages does not delete inbox data. The cursor makes retries stable;
// received_at records an explicit recipient read, not successful downstream use.
// A host root may read the whole timeline without acknowledging another inbox.
func (s *Store) ReadWorkerMessages(ctx context.Context, root, reader string, after int64, limit int, ack bool) (WorkerMessagePage, error) {
	page := WorkerMessagePage{Messages: []WorkerMessage{}, Cursor: after}
	if after < 0 || limit < 0 {
		return page, ErrInvalid
	}
	if limit == 0 {
		limit = 50
	}
	limit = min(limit, 100)
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := workerMember(ctx, conn, root, reader); err != nil {
			return err
		}
		clause := "session_id=? AND seq>?"
		args := []any{root, after}
		if reader != root || ack {
			clause += " AND recipient_id=?"
			args = append(args, reader)
		}
		args = append(args, limit+1)
		rows, err := conn.QueryContext(ctx, `SELECT seq,sender_id,recipient_id,message_id,body,created_at,received_at FROM worker_messages WHERE `+clause+` ORDER BY seq LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			m := WorkerMessage{RootID: root}
			var created, received string
			if err = rows.Scan(&m.Seq, &m.SenderID, &m.RecipientID, &m.ID, &m.Body, &created, &received); err != nil {
				rows.Close()
				return err
			}
			m.CreatedAt = parseTime(created)
			if received != "" {
				at := parseTime(received)
				m.ReceivedAt = &at
			}
			page.Messages = append(page.Messages, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page.Messages) > limit {
			page.HasMore = true
			page.Messages = page.Messages[:limit]
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			page.Cursor = m.Seq
			_, fenced, err := sessionFence(ctx, conn, m.SenderID)
			if err != nil {
				return err
			}
			if fenced {
				m.Withdrawn = true
				m.Body = ""
				continue
			}
			if ack && m.ReceivedAt == nil {
				at := time.Now().UTC()
				if _, err = conn.ExecContext(ctx, `UPDATE worker_messages SET received_at=? WHERE seq=? AND received_at=''`, at.Format(time.RFC3339Nano), m.Seq); err != nil {
					return err
				}
				m.ReceivedAt = &at
			}
		}
		return nil
	})
	return page, err
}
