package runtime

import (
	"context"
	"slices"

	"github.com/edynasty/LumenCortex/internal/session"
)

type MessagePage struct {
	Messages   []Message `json:"messages"`
	HasMore    bool      `json:"hasMore"`
	NextBefore int64     `json:"nextBefore"`
}

func (e *Engine) MessagePage(ctx context.Context, sessionID string, beforeSeq int64, limit int) (MessagePage, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	items, err := e.store.MessagesBefore(ctx, sessionID, beforeSeq, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[len(items)-limit:]
	}
	out, err := e.hostMessages(ctx, sessionID, items)
	if err != nil {
		return MessagePage{}, err
	}
	nextBefore := int64(-1)
	if len(out) > 0 {
		nextBefore = out[0].Seq
	}
	return MessagePage{
		Messages:   out,
		HasMore:    hasMore,
		NextBefore: nextBefore,
	}, nil
}

// hostMessages converts transcript rows for a host read and marks the rows no run sends again
// (Message.Withheld): every row of a session whose thread cannot be projected (the revoked source
// itself, or a reader that cannot be continued), else the rows the projection withholds. A session no
// fence names costs one indexed read. Nothing is dropped, so the host's counts and paging are those of
// the transcript.
func (e *Engine) hostMessages(ctx context.Context, sessionID string, items []session.Message) ([]Message, error) {
	p, err := e.store.Projection(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(items))
	for _, item := range items {
		_, withheld := slices.BinarySearch(p.Withheld, item.Seq)
		out = append(out, Message{SessionID: item.SessionID, Seq: item.Seq, Role: item.Role, JSON: item.JSON,
			Withheld: p.Isolated != "" || withheld})
	}
	return out, nil
}
