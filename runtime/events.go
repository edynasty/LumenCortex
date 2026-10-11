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
	Published uint64 `json:"published"`
	Dropped   uint64 `json:"dropped"`
}

type subscriber struct {
	ch chan Event
}

// eventBus fans events out to subscribers without ever waiting for one (I11).
// publish performs only non-blocking channel operations under mu.
type eventBus struct {
	mu          sync.Mutex
	nextSubID   uint64
	nextEventID atomic.Uint64
	published   atomic.Uint64
	dropped     atomic.Uint64
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
	b.published.Add(1)
	for _, s := range b.subs {
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
	return e
}

func (b *eventBus) stats() EventStats {
	return EventStats{Published: b.published.Load(), Dropped: b.dropped.Load()}
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
