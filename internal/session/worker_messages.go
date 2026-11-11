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

// Push delivery states of one worker message, and the bounded redelivery of an unacked one.
const (
	WorkerMessageQueued    = "queued"
	WorkerMessageDelivered = "delivered"
	WorkerMessageAcked     = "acked"
	// MaxWorkerMessageAttempts bounds how many runs write one message into a request window before it
	// stops being redelivered unacked; a host still reads it with worker_inbox.
	MaxWorkerMessageAttempts = 3
)

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
	// Delivery is the push state (migration 0013): queued until a run wrote it into a request window,
	// then delivered, then acked by the recipient. DeliveredSeq is the transcript row it became,
	// DeliveredRun the run that wrote it (an unacked message is redelivered once per run, bounded).
	Delivery     string     `json:"delivery"`
	DeliveredSeq int64      `json:"deliveredSeq,omitempty"`
	DeliveredRun string     `json:"deliveredRun,omitempty"`
	DeliveredAt  *time.Time `json:"deliveredAt,omitempty"`
	AckedAt      *time.Time `json:"ackedAt,omitempty"`
	Attempts     int        `json:"attempts,omitempty"`
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
	m.Delivery = WorkerMessageQueued
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
		rows, err := conn.QueryContext(ctx, `SELECT seq,sender_id,recipient_id,message_id,body,created_at,received_at,
delivery_state,delivered_seq,delivered_run,delivered_at,acked_at,attempts FROM worker_messages WHERE `+clause+` ORDER BY seq LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			m := WorkerMessage{RootID: root}
			var created, received, deliveredAt, ackedAt string
			if err = rows.Scan(&m.Seq, &m.SenderID, &m.RecipientID, &m.ID, &m.Body, &created, &received,
				&m.Delivery, &m.DeliveredSeq, &m.DeliveredRun, &deliveredAt, &ackedAt, &m.Attempts); err != nil {
				rows.Close()
				return err
			}
			m.CreatedAt = parseTime(created)
			if received != "" {
				at := parseTime(received)
				m.ReceivedAt = &at
			}
			if deliveredAt != "" {
				at := parseTime(deliveredAt)
				m.DeliveredAt = &at
			}
			if ackedAt != "" {
				at := parseTime(ackedAt)
				m.AckedAt = &at
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
			if ack && (m.ReceivedAt == nil || m.AckedAt == nil) {
				at := time.Now().UTC()
				stamp := at.Format(time.RFC3339Nano)
				// An explicit inbox read is the recipient's acknowledgement, so the push state moves to
				// acked as well: a message read on purpose is not redelivered, whether or not a run already
				// wrote it into a request window. Each stamp keeps its first value.
				if _, err = conn.ExecContext(ctx, `UPDATE worker_messages
SET received_at=CASE WHEN received_at='' THEN ? ELSE received_at END,
    acked_at=CASE WHEN acked_at='' THEN ? ELSE acked_at END, delivery_state=?
WHERE seq=? AND (received_at='' OR acked_at='')`, stamp, stamp, WorkerMessageAcked, m.Seq); err != nil {
					return err
				}
				if m.ReceivedAt == nil {
					m.ReceivedAt = &at
				}
				if m.AckedAt == nil {
					m.AckedAt = &at
				}
				m.Delivery = WorkerMessageAcked
			}
		}
		return nil
	})
	return page, err
}

// PendingWorkerMessages returns the messages a run of recipient must still write into its request
// window: queued ones, and unacked delivered ones that no run has redelivered yet (bounded attempts).
// A message the run that receives it now already wrote is not returned again for that run.
func (s *Store) PendingWorkerMessages(ctx context.Context, root, recipient, runID string, limit int) ([]WorkerMessage, error) {
	if root == "" || recipient == "" {
		return nil, ErrInvalid
	}
	if limit <= 0 {
		limit = 4
	}
	limit = min(limit, 16)
	rows, err := s.db.QueryContext(ctx, `SELECT seq,sender_id,recipient_id,message_id,body,created_at,received_at,
delivery_state,delivered_seq,delivered_run,delivered_at,acked_at,attempts FROM worker_messages
WHERE session_id=? AND recipient_id=? AND (
  delivery_state='queued'
  OR (delivery_state='delivered' AND acked_at='' AND delivered_run<>? AND attempts < ?))
ORDER BY seq LIMIT ?`, root, recipient, runID, MaxWorkerMessageAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkerMessage{}
	for rows.Next() {
		m := WorkerMessage{RootID: root}
		var created, received, deliveredAt, ackedAt string
		if err := rows.Scan(&m.Seq, &m.SenderID, &m.RecipientID, &m.ID, &m.Body, &created, &received,
			&m.Delivery, &m.DeliveredSeq, &m.DeliveredRun, &deliveredAt, &ackedAt, &m.Attempts); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		if received != "" {
			at := parseTime(received)
			m.ReceivedAt = &at
		}
		if deliveredAt != "" {
			at := parseTime(deliveredAt)
			m.DeliveredAt = &at
		}
		if ackedAt != "" {
			at := parseTime(ackedAt)
			m.AckedAt = &at
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeliveredWorkerMessage records the transcript row one message became in one run.
type DeliveredWorkerMessage struct {
	Seq           int64
	TranscriptSeq int64
}

// MarkWorkerMessagesDelivered moves messages to delivered and records the run and transcript row that
// carried them. The mark is idempotent: a seq already delivered by the same run keeps its row. A delivery
// is not a read: received_at stays the recipient's explicit inbox read.
func (s *Store) MarkWorkerMessagesDelivered(ctx context.Context, root, recipient, runID string, messages []DeliveredWorkerMessage) error {
	if root == "" || recipient == "" || len(messages) == 0 {
		return nil
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		for _, m := range messages {
			if _, err := conn.ExecContext(ctx, `UPDATE worker_messages
SET delivery_state='delivered', delivered_run=?, delivered_seq=?, delivered_at=?, attempts=attempts+1
WHERE session_id=? AND recipient_id=? AND seq=? AND acked_at='' AND (delivered_run<>? OR delivered_seq<0)`,
				runID, m.TranscriptSeq, at, root, recipient, m.Seq, runID); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// AckWorkerMessages acknowledges every message for recipient up to and including through, so a
// redelivery stops. It returns how many rows moved to acked.
func (s *Store) AckWorkerMessages(ctx context.Context, root, recipient string, through int64) (int, error) {
	if root == "" || recipient == "" || through < 0 {
		return 0, ErrInvalid
	}
	var changed int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := workerMember(ctx, conn, root, recipient); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `UPDATE worker_messages SET delivery_state=?, acked_at=?
WHERE session_id=? AND recipient_id=? AND seq<=? AND acked_at=''`, WorkerMessageAcked, now(), root, recipient, through)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	return int(changed), err
}
