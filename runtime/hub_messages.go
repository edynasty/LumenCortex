package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

type WorkerMessage = session.WorkerMessage
type WorkerMessagePage = session.WorkerMessagePage

// These are trusted host APIs. Agent tools below bind root and sender internally.
func (e *Engine) SendWorkerMessage(ctx context.Context, root, sender, recipient, id, body string) (WorkerMessage, error) {
	m, err := e.store.SendWorkerMessage(ctx, WorkerMessage{RootID: root, SenderID: sender, RecipientID: recipient, ID: id, Body: body})
	if err == nil {
		e.events.publish("worker.message.sent", root, map[string]any{"workerId": sender, "messageId": m.ID, "seq": m.Seq, "recipientId": recipient})
	}
	return m, err
}
func (e *Engine) WorkerMessages(ctx context.Context, root, reader string, after int64, limit int, receive bool) (WorkerMessagePage, error) {
	page, err := e.store.ReadWorkerMessages(ctx, root, reader, after, limit, receive)
	if err == nil && receive && len(page.Messages) > 0 {
		e.events.publish("worker.message.received", root, map[string]any{"workerId": reader, "count": len(page.Messages), "cursor": page.Cursor})
	}
	return page, err
}

type runMessenger struct {
	engine       *Engine
	life         context.Context
	root, sender string
}

func (r runMessenger) Send(ctx context.Context, recipient, id, body string) (any, error) {
	if err := r.life.Err(); err != nil {
		return nil, err
	}
	m, err := r.engine.SendWorkerMessage(ctx, r.root, r.sender, recipient, id, body)
	// The send receipt carries no recipient content or caller-selected authority.
	return map[string]any{"id": m.ID, "seq": m.Seq, "recipientId": m.RecipientID}, err
}
func (r runMessenger) Inbox(ctx context.Context, after int64, limit int) (any, error) {
	if err := r.life.Err(); err != nil {
		return nil, err
	}
	return r.engine.WorkerMessages(ctx, r.root, r.sender, after, limit, true)
}
func (e *Engine) bindWorkerMessages(ctx context.Context, id string, set *toolset.Set) error {
	root := id
	relation, err := e.store.ParentRelation(ctx, id)
	if err == nil {
		root = relation.ParentSessionID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return set.BindWorkerMessages(runMessenger{engine: e, life: ctx, root: root, sender: id})
}
