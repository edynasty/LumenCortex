package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/pluginapi"
)

const MaxRuntimeEventsPerSession = 10000

var ErrEventCursorExpired = errors.New("runtime event cursor expired; reload snapshot")

type RuntimeEventQuery struct {
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId,omitempty"`
	WorkerID  string `json:"workerId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	Type      string `json:"type,omitempty"`
	Search    string `json:"search,omitempty"`
	After     uint64 `json:"after"`
	Limit     int    `json:"limit,omitempty"`
}

type RuntimeEventPage struct {
	Events        []pluginapi.Event `json:"events"`
	Cursor        uint64            `json:"cursor"`
	PrunedThrough uint64            `json:"prunedThrough"`
	HighWatermark uint64            `json:"highWatermark"`
	HasMore       bool              `json:"hasMore"`
}

// AppendRuntimeEvent atomically allocates a per-run sequence and commits an event.
// Live output never calls this method. Retention evicts metadata, not transcript bodies.
func (s *Store) AppendRuntimeEvent(ctx context.Context, event pluginapi.Event) (pluginapi.Event, error) {
	if event.SessionID == "" || event.ID == "" || len(event.ID) > 128 || len(event.RunID) > 128 || len(event.Type) > 128 || event.Type == "" {
		return event, ErrInvalid
	}
	projected, err := projectJournalPayload(diag.EventPayload(event.Data))
	if err != nil {
		return event, err
	}
	event.Data = projected
	event.SchemaVersion = 1
	event.Sensitivity = "metadata"
	event.Reliability = "durable"
	event.Seq = 0
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, event.SessionID); err != nil {
			return err
		}
		// Delivery of an already committed ID is idempotent, but never transferable.
		var prior string
		var cursor uint64
		err := conn.QueryRowContext(ctx, `SELECT cursor,event_json FROM runtime_events WHERE id=?`, event.ID).Scan(&cursor, &prior)
		if err == nil {
			var old pluginapi.Event
			if err = json.Unmarshal([]byte(prior), &old); err != nil {
				return err
			}
			if old.SessionID != event.SessionID || old.RunID != event.RunID || old.Type != event.Type || string(old.Data) != string(event.Data) {
				return ErrExists
			}
			old.Cursor = cursor
			event = old
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = conn.QueryRowContext(ctx, `INSERT INTO runtime_event_sequences(session_id,run_id,sequence) VALUES(?,?,1)
   ON CONFLICT(session_id,run_id) DO UPDATE SET sequence=sequence+1 RETURNING sequence`, event.SessionID, event.RunID).Scan(&event.Sequence); err != nil {
			return err
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if len(raw) > 16384 {
			return fmt.Errorf("%w: event exceeds 16 KiB", ErrInvalid)
		}
		result, err := conn.ExecContext(ctx, `INSERT INTO runtime_events(id,session_id,run_id,sequence,type,event_json) VALUES(?,?,?,?,?,?)`, event.ID, event.SessionID, event.RunID, event.Sequence, event.Type, string(raw))
		if err != nil {
			return err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return err
		}
		event.Cursor = uint64(id)
		var cutoff uint64
		if err = conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT cursor FROM runtime_events WHERE session_id=? ORDER BY cursor DESC LIMIT 1 OFFSET ?),0)`, event.SessionID, MaxRuntimeEventsPerSession).Scan(&cutoff); err != nil {
			return err
		}
		if cutoff == 0 {
			return nil
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO runtime_event_retention(session_id,pruned_through) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET pruned_through=MAX(pruned_through,excluded.pruned_through)`, event.SessionID, cutoff); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM runtime_events WHERE session_id=? AND cursor<=?`, event.SessionID, cutoff)
		return err
	})
	return event, err
}

// QueryRuntimeEvents cannot read another session or a revoked source. Cursor advancement
// skips nonmatching records, so a filtered consumer neither loops nor misorders events.
func (s *Store) QueryRuntimeEvents(ctx context.Context, q RuntimeEventQuery) (RuntimeEventPage, error) {
	page := RuntimeEventPage{Events: []pluginapi.Event{}, Cursor: q.After}
	if q.SessionID == "" || q.Limit < 0 || len(q.Search) > 256 || len(q.Type) > 128 {
		return page, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = 200
	}
	q.Limit = min(q.Limit, 1000)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return page, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN`); err != nil {
		return page, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	if err = sessionExists(ctx, conn, q.SessionID); err != nil {
		return page, err
	}
	if _, fenced, err := sessionFence(ctx, conn, q.SessionID); err != nil {
		return page, err
	} else if fenced {
		return page, ErrFenced
	}
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT pruned_through FROM runtime_event_retention WHERE session_id=?),0)`, q.SessionID).Scan(&page.PrunedThrough); err != nil {
		return page, err
	}
	if q.After > 0 && q.After < page.PrunedThrough {
		return page, ErrEventCursorExpired
	}
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(cursor),0) FROM runtime_events WHERE session_id=?`, q.SessionID).Scan(&page.HighWatermark); err != nil {
		return page, err
	}
	where := `session_id=? AND cursor>?`
	args := []any{q.SessionID, q.After}
	for _, filter := range []struct{ field, value string }{{"run_id", q.RunID}, {"type", q.Type}, {"json_extract(event_json,'$.workerId')", q.WorkerID}, {"json_extract(event_json,'$.taskId')", q.TaskID}} {
		if filter.value != "" {
			where += " AND " + filter.field + "=?"
			args = append(args, filter.value)
		}
	}
	if q.Search != "" {
		where += ` AND instr(lower(event_json),?)>0`
		args = append(args, strings.ToLower(q.Search))
	}
	args = append(args, q.Limit+1)
	rows, err := conn.QueryContext(ctx, `SELECT cursor,event_json FROM runtime_events WHERE `+where+` ORDER BY cursor LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var event pluginapi.Event
		var raw string
		var cursor uint64
		if err = rows.Scan(&cursor, &raw); err != nil {
			return page, err
		}
		if len(page.Events) == q.Limit {
			page.HasMore = true
			break
		}
		if err = json.Unmarshal([]byte(raw), &event); err != nil {
			return page, err
		}
		event.Cursor = cursor
		page.Cursor = cursor
		page.Events = append(page.Events, event)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if err = rows.Close(); err != nil {
		return page, err
	}
	if !page.HasMore {
		page.Cursor = max(page.Cursor, page.HighWatermark)
	}
	return page, nil
}
