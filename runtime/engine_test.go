package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEnginePersistsSessionAndStreamsEvents(t *testing.T) {
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace, RepoDir: filepath.Join(workspace, ".lumencortex-test")})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	events, unsubscribe := engine.Events(8)
	defer unsubscribe()
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "test bounded runtime"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.AppendMessage(ctx, "user", map[string]string{"content": "hello"}); err != nil {
		t.Fatal(err)
	}
	messages, err := handle.RecentMessages(ctx, 1)
	if err != nil || len(messages) != 1 {
		t.Fatalf("recent messages: len=%d err=%v", len(messages), err)
	}

	deadline := time.After(time.Second)
	seenStart := false
	for !seenStart {
		select {
		case event := <-events:
			seenStart = event.Type == "session.start"
		case <-deadline:
			t.Fatal("did not receive session.start")
		}
	}
}

func TestSlowEventSubscriberIsBounded(t *testing.T) {
	bus := newEventBus()
	ch, unsubscribe := bus.subscribe(2)
	defer unsubscribe()
	for i := 0; i < 1000; i++ {
		bus.publish("tick", "", map[string]int{"i": i})
	}
	if got := len(ch); got > 2 {
		t.Fatalf("subscriber buffer grew to %d", got)
	}
}
