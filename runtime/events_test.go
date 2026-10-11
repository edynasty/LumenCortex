package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

// guard bounds a test step that must finish at once; it only turns a deadlock
// into a failure. Correctness is asserted on counters and channel contents.
const guard = 10 * time.Second

func publishN(bus *eventBus, n int) {
	for range n {
		bus.publish("tick", "", map[string]int{"n": n})
	}
}

// within runs fn and fails the test if it has not returned after guard.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(guard):
		t.Fatalf("%s did not return: publish is blocked", what)
	}
}

// I11: a subscriber that never reads cannot block publish, loses its oldest
// events, and every lost delivery is counted; a healthy subscriber is unaffected.
func TestSlowSubscriberCannotBlockPublish(t *testing.T) {
	const burst = 500
	bus := newEventBus()
	slow, unsubscribeSlow := bus.subscribe(2)
	healthy, _ := bus.subscribe(1024)

	// Fill the slow subscriber's buffer. publish delivers synchronously, so once
	// these return the buffer is full: the burst below must take the drop path.
	publishN(bus, 2)
	if len(slow) != cap(slow) {
		t.Fatalf("slow subscriber holds %d of %d events before the burst", len(slow), cap(slow))
	}
	if got := bus.stats(); got != (EventStats{Published: 2}) {
		t.Fatalf("stats before the burst = %+v", got)
	}

	within(t, "burst", func() { publishN(bus, burst) })

	// One lost delivery per event the full slow buffer could not take; the healthy
	// subscriber (buffer larger than the total) lost none.
	if got, want := bus.stats(), (EventStats{Published: 2 + burst, Dropped: burst}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}

	// The healthy subscriber received every event, in order.
	for seq := uint64(1); seq <= 2+burst; seq++ {
		select {
		case e := <-healthy:
			if e.Seq != seq {
				t.Fatalf("healthy subscriber got seq %d, want %d", e.Seq, seq)
			}
		default:
			t.Fatalf("healthy subscriber is missing seq %d", seq)
		}
	}

	// The slow subscriber kept the newest events, not the oldest.
	if len(slow) != 2 {
		t.Fatalf("slow subscriber holds %d events", len(slow))
	}
	if first, second := (<-slow).Seq, (<-slow).Seq; first != 2+burst-1 || second != 2+burst {
		t.Fatalf("slow subscriber kept seq %d,%d, want %d,%d", first, second, 2+burst-1, 2+burst)
	}

	// Unsubscribing a subscriber that dropped events is safe, also twice.
	unsubscribeSlow()
	unsubscribeSlow()
	if _, open := <-slow; open {
		t.Fatal("slow channel still open after unsubscribe")
	}
	publishN(bus, 3)
	if got, want := bus.stats(), (EventStats{Published: 2 + burst + 3, Dropped: burst}); got != want {
		t.Fatalf("stats after unsubscribe = %+v, want %+v", got, want)
	}
}

// Dropped counts lost deliveries: two subscribers overflowing on the same event
// add two.
func TestDroppedCountsDeliveriesNotEvents(t *testing.T) {
	bus := newEventBus()
	_, unsubscribeA := bus.subscribe(1)
	_, unsubscribeB := bus.subscribe(1)
	_, unsubscribeC := bus.subscribe(100)
	defer unsubscribeA()
	defer unsubscribeB()
	defer unsubscribeC()

	publishN(bus, 1+5)
	if got, want := bus.stats(), (EventStats{Published: 6, Dropped: 2 * 5}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestEventStatsAreNotResetByReadingAndNoSubscribersCountsPublished(t *testing.T) {
	bus := newEventBus()
	publishN(bus, 4)
	first, second := bus.stats(), bus.stats()
	if first != second || first != (EventStats{Published: 4}) {
		t.Fatalf("stats = %+v then %+v", first, second)
	}
}

func TestCloseAfterDropsIsSafeAndLaterPublishesAreNotCounted(t *testing.T) {
	bus := newEventBus()
	slow, unsubscribe := bus.subscribe(1)
	publishN(bus, 10)
	before := bus.stats()
	if before != (EventStats{Published: 10, Dropped: 9}) {
		t.Fatalf("stats = %+v", before)
	}

	bus.close()
	bus.close()
	unsubscribe() // after close: no panic, no double close
	if e, open := <-slow; !open {
		t.Fatal("close discarded the buffered event")
	} else if e.Seq != 10 {
		t.Fatalf("buffered event seq=%d", e.Seq)
	}
	if _, open := <-slow; open {
		t.Fatal("channel still open after close")
	}

	publishN(bus, 5)
	if got := bus.stats(); got != before {
		t.Fatalf("publish after close changed stats: %+v, want %+v", got, before)
	}
	ch, unsubscribeLate := bus.subscribe(1)
	unsubscribeLate()
	if _, open := <-ch; open {
		t.Fatal("subscribe after close returned an open channel")
	}
}

// Publishing, subscribing, unsubscribing and reading stats at the same time is
// race-free (run under -race), stats never go backwards, and no publish is lost.
func TestConcurrentPublishSubscribeAndStats(t *testing.T) {
	const (
		publishers = 4
		perPub     = 500
	)
	bus := newEventBus()
	// Never read and never unsubscribed: it guarantees drops, and a deferred
	// unsubscribe would deadlock behind a blocked publish instead of failing.
	bus.subscribe(1)

	stop := make(chan struct{})
	var background sync.WaitGroup
	background.Add(2)
	go func() { // churn subscribers, some of which read and some of which do not
		defer background.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ch, unsubscribe := bus.subscribe(1 + i%3)
			if i%2 == 0 {
				select {
				case <-ch:
				default:
				}
			}
			unsubscribe()
		}
	}()
	go func() { // monotonic stats reader
		defer background.Done()
		var last EventStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := bus.stats()
			if got.Published < last.Published || got.Dropped < last.Dropped {
				t.Errorf("stats went backwards: %+v after %+v", got, last)
				return
			}
			last = got
		}
	}()

	var wg sync.WaitGroup
	for range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			publishN(bus, perPub)
		}()
	}
	within(t, "concurrent publishers", wg.Wait)
	close(stop)
	background.Wait()

	got := bus.stats()
	if got.Published != publishers*perPub {
		t.Fatalf("Published = %d, want %d", got.Published, publishers*perPub)
	}
	if got.Dropped < publishers*perPub-1 {
		t.Fatalf("Dropped = %d: the stalled subscriber alone loses all but %d events", got.Dropped, 1)
	}
}

// The engine exposes the bus counters; a subscriber that never reads does not stop
// the engine from publishing.
func TestEngineEventStatsCountSlowSubscriberDrops(t *testing.T) {
	engine := openTestEngine(t)
	_, unsubscribe := engine.Events(1)
	defer unsubscribe()
	before := engine.EventStats()

	const sessions = 3
	within(t, "session creation", func() {
		for range sessions {
			if _, err := engine.NewSession(context.Background(), SessionOptions{Goal: "g"}); err != nil {
				t.Error(err)
			}
		}
	})

	got := engine.EventStats()
	if published := got.Published - before.Published; published != sessions {
		t.Fatalf("Published grew by %d, want %d", published, sessions)
	}
	if dropped := got.Dropped - before.Dropped; dropped != sessions-1 {
		t.Fatalf("Dropped grew by %d, want %d", dropped, sessions-1)
	}
}
