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
	_, info, err := engine.Session(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "created" {
		t.Fatalf("new session status=%q, want created", info.Status)
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
