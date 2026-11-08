package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeEventsRecoverDisconnectedSubscriber(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	opts := Options{Workspace: workspace, RepoDir: filepath.Join(workspace, "state")}
	engine, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	h, err := engine.NewSession(ctx, SessionOptions{Goal: "event recovery"})
	if err != nil {
		t.Fatal(err)
	}
	engine.noteRunStarted(h.ID, "event-run")
	slow, stop := engine.events.subscribeSession(1, h.ID)
	for i := 0; i < 8; i++ {
		engine.events.publish("tool.completed", h.ID, map[string]any{"name": "read_file", "ok": true, "index": i})
	}
	newest := <-slow
	stop()
	if newest.Cursor == 0 || newest.Sequence != 8 {
		t.Fatalf("live identity %+v", newest)
	}
	for i := 0; i < 500; i++ {
		engine.events.publish("tool.output", h.ID, map[string]any{"chunk": "stream text", "stream": "stdout"})
	}
	page, err := engine.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: h.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 9 {
		t.Fatalf("live output entered durable history: %d", len(page.Events))
	}
	cursor := page.Cursor
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	page, err = engine.ResumeRuntimeEvents(ctx, RuntimeEventQuery{SessionID: h.ID})
	if err != nil || page.Cursor != cursor || len(page.Events) != 9 {
		t.Fatalf("reopen %+v %v", page, err)
	}
	next, err := engine.ResumeRuntimeEvents(ctx, RuntimeEventQuery{SessionID: h.ID, After: cursor})
	if err != nil || len(next.Events) != 0 {
		t.Fatalf("duplicated resume %+v %v", next, err)
	}
}

func TestRuntimeEventPersistenceFailureIsObservable(t *testing.T) {
	engine := openTestEngine(t)
	h, err := engine.NewSession(context.Background(), SessionOptions{Goal: "storage fault"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.store.Close(); err != nil {
		t.Fatal(err)
	}
	event := engine.events.publish("tool.started", h.ID, map[string]any{"name": "bash"})
	if event.Reliability != "unavailable" || event.Cursor != 0 || engine.EventStats().PersistenceFailures != 1 {
		t.Fatalf("failure hidden %+v %+v", event, engine.EventStats())
	}
	if _, err = engine.QueryRuntimeEvents(context.Background(), RuntimeEventQuery{SessionID: h.ID}); err == nil {
		t.Fatal("query reported complete history after failed persistence")
	}
}

func TestRuntimeEventRedactionPrecedesSubscriptions(t *testing.T) {
	bus := newEventBus()
	ch, stop := bus.subscribe(2)
	defer stop()
	bus.publish("tool.started", "s", map[string]any{"args": `{"password":"PASSWORD-SENTINEL","reasoning":"PRIVATE-SENTINEL","path":"a.go"}`, "scratchpad": "PRIVATE-SENTINEL", "chunk": "Authorization: Bearer abcdefghijklmnop"})
	event := <-ch
	raw, _ := json.Marshal(event)
	for _, secret := range []string{"PASSWORD-SENTINEL", "PRIVATE-SENTINEL", "abcdefghijklmnop"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret leaked: %s", raw)
		}
	}
	if !strings.Contains(string(raw), "a.go") {
		t.Fatal("non-secret diagnostic argument lost")
	}
}

func TestRuntimeEventSubscriptionIsSessionScopedAndCancellable(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	a, err := engine.NewSession(ctx, SessionOptions{Goal: "one"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.NewSession(ctx, SessionOptions{Goal: "two"})
	if err != nil {
		t.Fatal(err)
	}
	stream, cancel, err := engine.SubscribeRuntimeEvents(ctx, a.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	engine.events.publish("tool.completed", b.ID, map[string]any{"name": "foreign"})
	engine.events.publish("tool.completed", a.ID, map[string]any{"name": "own"})
	select {
	case event := <-stream:
		if event.SessionID != a.ID {
			t.Fatal("cross-session event")
		}
	case <-time.After(time.Second):
		t.Fatal("event absent")
	}
	cancel()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("stream open")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	if _, _, err = engine.SubscribeRuntimeEvents(ctx, "", 1); err == nil {
		t.Fatal("unscoped subscription accepted")
	}
	if _, err = engine.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: "missing"}); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("missing session %v", err)
	}
}
