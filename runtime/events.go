package runtime

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

type Event struct {
	Seq       uint64          `json:"seq"`
	At        time.Time       `json:"at"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type subscriber struct {
	ch chan Event
}

type eventBus struct {
	mu          sync.Mutex
	nextSubID   uint64
	nextEventID atomic.Uint64
	subs        map[uint64]*subscriber
	closed      bool
}

func newEventBus() *eventBus { return &eventBus{subs: map[uint64]*subscriber{}} }

func (b *eventBus) subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	b.nextSubID++
	id := b.nextSubID
	s := &subscriber{ch: make(chan Event, buffer)}
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
	raw, _ := json.Marshal(payload)
	e := Event{
		Seq:       b.nextEventID.Add(1),
		At:        time.Now().UTC(),
		Type:      eventType,
		SessionID: sessionID,
		Data:      raw,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return e
	}
	for _, s := range b.subs {
		select {
		case s.ch <- e:
		default:
			// Drop the oldest event for this subscriber rather than let a slow UI
			// create unbounded runtime memory pressure.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- e:
			default:
			}
		}
	}
	return e
}

func (b *eventBus) close() {
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
