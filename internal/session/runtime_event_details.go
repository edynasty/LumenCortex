package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/edynasty/LumenCortex/pluginapi"
)

// RuntimeEvent reads one retained event within its session, never a caller-supplied payload.
func (s *Store) RuntimeEvent(ctx context.Context, sessionID, eventID string) (pluginapi.Event, error) {
	var event pluginapi.Event
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT cursor,event_json FROM runtime_events WHERE session_id=? AND id=?`, sessionID, eventID).Scan(&event.Cursor, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return event, ErrNotFound
	}
	if err != nil {
		return event, err
	}
	cursor := event.Cursor
	err = json.Unmarshal([]byte(raw), &event)
	event.Cursor = cursor
	return event, err
}

// RuntimeToolMessages follows the run's answered envelope, so repeated provider call IDs
// in a later run cannot attach another tool's arguments/result to this event.
func (s *Store) RuntimeToolMessages(ctx context.Context, sessionID, runID, callID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.session_id,m.seq,m.role,m.json FROM session_messages m
 WHERE m.session_id=? AND m.seq IN (
 SELECT a.seq FROM request_envelopes r JOIN session_messages a ON a.session_id=r.session_id AND a.seq=r.answered_seq
 WHERE r.session_id=? AND r.run_id=? AND EXISTS(SELECT 1 FROM json_each(a.json,'$.tool_calls') c WHERE json_extract(c.value,'$.id')=?)
 UNION ALL
 SELECT t.seq FROM request_envelopes r JOIN session_messages a ON a.session_id=r.session_id AND a.seq=r.answered_seq
 JOIN session_messages t ON t.session_id=r.session_id AND t.seq>r.answered_seq AND t.role='tool' AND json_extract(t.json,'$.tool_call_id')=?
 WHERE r.session_id=? AND r.run_id=? AND EXISTS(SELECT 1 FROM json_each(a.json,'$.tool_calls') c WHERE json_extract(c.value,'$.id')=?)
 AND NOT EXISTS(SELECT 1 FROM session_messages later WHERE later.session_id=t.session_id AND later.role='assistant' AND later.seq>r.answered_seq AND later.seq<t.seq)
 ) ORDER BY m.seq LIMIT 3`, sessionID, sessionID, runID, callID, callID, sessionID, runID, callID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var raw string
		if err = rows.Scan(&m.SessionID, &m.Seq, &m.Role, &raw); err != nil {
			return nil, err
		}
		m.JSON = json.RawMessage(raw)
		out = append(out, m)
	}
	return out, rows.Err()
}
