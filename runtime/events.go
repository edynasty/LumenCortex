package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/pluginapi"
)

// Event is a Core notification. Plugins receive the same type through pluginapi.Host.
type Event = pluginapi.Event

// EventStats is an immutable snapshot of the event bus counters. Counters only
// grow; reading them never resets them.
//
//   - Published counts events accepted by publish while the bus was open: one per
//     event, whatever the number of subscribers (zero included). Events published
//     after Close are not accepted and are not counted.
//   - Dropped counts lost deliveries, not affected events: each time publish had to
//     discard an event because one subscriber's buffer was full, Dropped grows by
//     one. When it makes room it discards that subscriber's oldest queued event;
//     the new event is delivered. Two subscribers that both overflow on the same
//     event add two. Events left unread in a buffer when a subscriber unsubscribes
//     or the bus closes are not counted.
//
// Published and Dropped are read independently, so a snapshot taken during
// concurrent publishing may show a Dropped that includes deliveries of an event
// not yet reflected in Published.
type EventStats struct {
	Published           uint64 `json:"published"`
	Dropped             uint64 `json:"dropped"`
	PersistenceFailures uint64 `json:"persistenceFailures"`
}

type subscriber struct {
	ch        chan Event
	sessionID string
}

// eventBus fans events out to subscribers without ever waiting for one (I11).
// publish performs only non-blocking channel operations under mu.
type eventBus struct {
	mu          sync.Mutex
	writeMu     sync.Mutex
	store       *session.Store
	runs        map[string]string
	onFailure   func(string, error)
	failure     error
	failed      atomic.Uint64
	nextSubID   uint64
	nextEventID atomic.Uint64
	published   atomic.Uint64
	dropped     atomic.Uint64
	subs        map[uint64]*subscriber
	closed      bool
}

func newEventBus() *eventBus {
	return &eventBus{subs: map[uint64]*subscriber{}, runs: map[string]string{}}
}

func (b *eventBus) subscribe(buffer int) (<-chan Event, func()) {
	return b.subscribeSession(buffer, "")
}

func (b *eventBus) subscribeSession(buffer int, sessionID string) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	buffer = min(buffer, 4096)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	b.nextSubID++
	id := b.nextSubID
	s := &subscriber{ch: make(chan Event, buffer), sessionID: sessionID}
	b.subs[id] = s
	return s.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(current.ch)
		}
	}
}

func (b *eventBus) publish(eventType, sessionID string, payload any) Event {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return Event{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		b.persistenceFailed(sessionID, err)
		return Event{}
	}
	id, err := newRunID()
	if err != nil {
		b.persistenceFailed(sessionID, err)
		return Event{}
	}
	e := Event{ID: id, SchemaVersion: 1, At: time.Now().UTC(), Type: eventType,
		SessionID: sessionID, Data: diag.EventPayload(raw), Sensitivity: "session", Reliability: "live"}
	b.mu.Lock()
	e.RunID = b.runs[sessionID]
	b.mu.Unlock()
	var fields struct {
		RunID       string `json:"runId"`
		ParentRunID string `json:"parentRunId"`
		WorkerID    string `json:"workerId"`
		TaskID      string `json:"taskId"`
		RequestID   string `json:"requestId"`
		ToolCallID  string `json:"toolCallId"`
		Phase       string `json:"phase"`
	}
	_ = json.Unmarshal(e.Data, &fields)
	if fields.RunID != "" {
		e.RunID = fields.RunID
	}
	e.ParentRunID, e.WorkerID, e.TaskID, e.Phase = fields.ParentRunID, fields.WorkerID, fields.TaskID, fields.Phase
	e.CorrelationID, e.CausationID = fields.ToolCallID, fields.RequestID
	if e.CorrelationID == "" {
		e.CorrelationID = fields.RequestID
	}
	if b.store != nil && durableEvent(eventType, sessionID) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stored, saveErr := b.store.AppendRuntimeEvent(ctx, e)
		cancel()
		if saveErr != nil {
			b.persistenceFailed(sessionID, saveErr)
			e.Reliability = "unavailable"
		} else {
			e.Cursor, e.Sequence, e.Reliability = stored.Cursor, stored.Sequence, "durable"
		}
	}
	e.Seq = b.nextEventID.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return e
	}
	b.published.Add(1)
	for _, s := range b.subs {
		if s.sessionID != "" && s.sessionID != sessionID {
			continue
		}
		select {
		case s.ch <- e:
			continue
		default:
		}
		// The buffer is full. Discard this subscriber's oldest event rather than
		// block the runtime or let a slow UI grow memory without bound. Only
		// publish sends and the lock is held, so the retry below succeeds; the
		// consumer may have drained the buffer meanwhile, in which case nothing
		// was discarded.
		select {
		case <-s.ch:
			b.dropped.Add(1)
		default:
		}
		select {
		case s.ch <- e:
		default:
			b.dropped.Add(1)
		}
	}
	if eventType == "run.stopped" {
		delete(b.runs, sessionID)
	}
	return e
}

func (b *eventBus) stats() EventStats {
	return EventStats{Published: b.published.Load(), Dropped: b.dropped.Load(), PersistenceFailures: b.failed.Load()}
}

func (b *eventBus) close() {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		close(s.ch)
		delete(b.subs, id)
	}
}

func durableEvent(kind, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	switch kind {
	case "tool.output", "llm.delta", "model.output", "dap.output", "shell.output", "eval.output", "subagent.event":
		return false
	}
	return true
}

func (b *eventBus) persistenceFailed(sessionID string, err error) {
	b.failed.Add(1)
	b.mu.Lock()
	if b.failure == nil {
		b.failure = errors.New("runtime event persistence unavailable")
	}
	b.mu.Unlock()
	if b.onFailure != nil {
		b.onFailure(sessionID, err)
	}
}

func (b *eventBus) persistenceError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failure
}

func (b *eventBus) noteRun(sessionID, runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runs[sessionID] = runID
}

func (b *eventBus) forgetRun(sessionID, runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runs[sessionID] == runID {
		delete(b.runs, sessionID)
	}
}
