package runtime

import (
	"context"
	"errors"
	"github.com/edynasty/LumenCortex/internal/diag"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

type RuntimeEventQuery = session.RuntimeEventQuery
type RuntimeEventPage = session.RuntimeEventPage

var ErrEventCursorExpired = session.ErrEventCursorExpired

// QueryRuntimeEvents reads content-free durable history for one explicitly selected session.
func (e *Engine) QueryRuntimeEvents(ctx context.Context, query RuntimeEventQuery) (RuntimeEventPage, error) {
	if err := e.events.persistenceError(); err != nil {
		return RuntimeEventPage{}, err
	}
	return e.store.QueryRuntimeEvents(ctx, query)
}

// ResumeRuntimeEvents returns the next cursor page, including events committed while no UI was connected.
func (e *Engine) ResumeRuntimeEvents(ctx context.Context, query RuntimeEventQuery) (RuntimeEventPage, error) {
	return e.QueryRuntimeEvents(ctx, query)
}

// SubscribeRuntimeEvents subscribes before the caller queries history. Deduplicate durable
// notifications by ID/Cursor. A live gap is repaired with Resume, never assumed complete.
func (e *Engine) SubscribeRuntimeEvents(ctx context.Context, sessionID string, buffer int) (<-chan Event, func(), error) {
	if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
		return nil, nil, err
	}
	source, unsubscribe := e.events.subscribeSession(buffer, sessionID)
	ctx, cancel := context.WithCancel(ctx)
	output := make(chan Event, min(max(buffer, 1), 4096))
	go func() {
		defer close(output)
		defer unsubscribe()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-source:
				if !ok {
					return
				}
				if e.runtimeEventAccess(ctx, sessionID) != nil {
					return
				}
				// Subscriber backpressure stays here, never in the producer; the source is bounded.
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output, func() { cancel(); unsubscribe() }, nil
}

func (e *Engine) runtimeEventAccess(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("runtime events require a session")
	}
	if _, err := e.store.Get(ctx, id); err != nil {
		return err
	}
	fenced, err := e.store.SessionFenced(ctx, id)
	if err != nil {
		return err
	}
	if fenced {
		return session.ErrFenced
	}
	return e.events.persistenceError()
}

type RunSnapshot struct {
	Session    SessionInfo `json:"session"`
	RunID      string      `json:"runId,omitempty"`
	Active     bool        `json:"active"`
	CapturedAt time.Time   `json:"capturedAt"`
	Events     EventStats  `json:"events"`
	Cursor     uint64      `json:"cursor"`
}

func (e *Engine) GetRunSnapshot(ctx context.Context, sessionID string) (RunSnapshot, error) {
	if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
		return RunSnapshot{}, err
	}
	_, info, err := e.Session(ctx, sessionID)
	if err != nil {
		return RunSnapshot{}, err
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: sessionID, Limit: 1})
	if err != nil {
		return RunSnapshot{}, err
	}
	info.Usage, info.Error = inspectorJSON(info.Usage), inspectorJSON(info.Error)
	info.Metadata = nil
	info.Goal = diag.MaskSecrets(info.Goal)
	info.Final = nil
	e.runsMu.Lock()
	_, active := e.runs[sessionID]
	e.runsMu.Unlock()
	e.events.mu.Lock()
	runID := e.events.runs[sessionID]
	e.events.mu.Unlock()
	return RunSnapshot{Session: info, RunID: runID, Active: active, CapturedAt: time.Now().UTC(), Events: e.EventStats(), Cursor: page.HighWatermark}, nil
}

func (e *Engine) GetExecutionTimeline(ctx context.Context, query RuntimeEventQuery) (RuntimeEventPage, error) {
	return e.QueryRuntimeEvents(ctx, query)
}
