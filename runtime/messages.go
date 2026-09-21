package runtime

import "context"

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
	out := make([]Message, 0, len(items))
	for _, item := range items {
		out = append(out, Message{
			SessionID: item.SessionID,
			Seq:       item.Seq,
			Role:      item.Role,
			JSON:      item.JSON,
		})
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
