package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// ErrSessionExists reports an import whose session id is already in the database. Imports use ids
// derived from the source, so it means the source was imported before.
var ErrSessionExists = errors.New("session already exists")

// ImportedSession is one session an import writes: its row, its whole transcript (seq 0 first) and,
// for a child, the relation to its parent, which must be written earlier in the same import.
type ImportedSession struct {
	Session  Session
	Messages []protocol.Message
	Relation *Relation
}

// ImportSessions writes sessions that were not produced by a run here, each with its complete
// transcript, in one BEGIN IMMEDIATE transaction: either every session, row and relation is
// written or none is. A session id that already exists is ErrSessionExists and writes nothing,
// which makes an import with deterministic ids idempotent even when two run at once.
//
// An imported session is never running: it has no run to own it, so a "running" status would be
// reconciled into interrupted on the next Open.
func (s *Store) ImportSessions(ctx context.Context, items []ImportedSession) error {
	if len(items) == 0 {
		return errors.New("import: no sessions")
	}
	for _, item := range items {
		switch {
		case item.Session.ID == "":
			return errors.New("import: session id is required")
		case item.Session.Status == "" || item.Session.Status == "running":
			return fmt.Errorf("import: session %s: status %q is not allowed", item.Session.ID, item.Session.Status)
		case len(item.Messages) == 0:
			return fmt.Errorf("import: session %s has no transcript", item.Session.ID)
		}
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	insertMessage, err := conn.PrepareContext(ctx, `INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insertMessage.Close()
	for _, item := range items {
		var exists int
		err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, item.Session.ID).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			return fmt.Errorf("%w: %s", ErrSessionExists, item.Session.ID)
		}
		// A privacy deletion of this id that finished (state cleaned) is superseded: importing the
		// source again is the user's new decision, and the fences of the deleted content would refuse
		// the imported session's own runs. A deletion still pending keeps its row, refused above.
		if _, err := conn.ExecContext(ctx, `DELETE FROM source_fences
			WHERE scope_kind IN ('session','privacy') AND scope_ref = ? AND state = 'cleaned'`, item.Session.ID); err != nil {
			return err
		}
		if err := insertSession(ctx, conn, item.Session); err != nil {
			return err
		}
		for seq, message := range item.Messages {
			raw, err := json.Marshal(message)
			if err != nil {
				return err
			}
			if _, err := insertMessage.ExecContext(ctx, item.Session.ID, int64(seq), message.Role, string(raw)); err != nil {
				return err
			}
		}
		if r := item.Relation; r != nil {
			if r.ChildSessionID != item.Session.ID || r.ParentSessionID == "" || r.ParentSessionID == r.ChildSessionID || r.Kind == "" {
				return fmt.Errorf("import: session %s: invalid relation", item.Session.ID)
			}
			created := r.CreatedAt
			if created.IsZero() {
				created = item.Session.CreatedAt
			}
			if _, err := conn.ExecContext(ctx, `
INSERT INTO session_relations(child_session_id, parent_session_id, kind, created_at, metadata_json)
VALUES(?, ?, ?, ?, ?)`,
				r.ChildSessionID, r.ParentSessionID, r.Kind, created.UTC().Format(time.RFC3339Nano),
				normalizeJSON(r.Metadata, "{}")); err != nil {
				return err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
