package session

import (
	"context"
	"encoding/json"

	"github.com/edynasty/LumenCortex/pluginapi"
)

// ContextEvents reads only the newest evidence candidates, never output streams.
// The extra row makes truncation explicit even when unrelated sessions have advanced the cursor.
func (s *Store) ContextEvents(ctx context.Context, sessionID string, limit int) ([]pluginapi.Event, bool, error) {
	if limit < 1 || limit > 256 {
		return nil, false, ErrInvalid
	}
	if err := sessionExists(ctx, s.db, sessionID); err != nil {
		return nil, false, err
	}
	if _, fenced, err := sessionFence(ctx, s.db, sessionID); err != nil {
		return nil, false, err
	} else if fenced {
		return nil, false, ErrFenced
	}
	rows, err := s.db.QueryContext(ctx, `SELECT cursor,event_json FROM runtime_events WHERE session_id=? AND type IN ('tool.end','eval.callback','workflow.transition','workflow.approved','worker.result.ready','verification.completed','verification.end','delivery.result') ORDER BY cursor DESC LIMIT ?`, sessionID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events := make([]pluginapi.Event, 0, limit)
	truncated := false
	for rows.Next() {
		if len(events) == limit {
			truncated = true
			break
		}
		var cursor uint64
		var raw string
		var event pluginapi.Event
		if err := rows.Scan(&cursor, &raw); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return nil, false, err
		}
		event.Cursor = cursor
		events = append(events, event)
	}
	return events, truncated, rows.Err()
}
