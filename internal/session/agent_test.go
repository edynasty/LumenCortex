package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestAgentPatchAndNextStepDoNotRewriteHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, Session{ID: "s", Goal: "goal", Metadata: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(ctx, "s", "user", map[string]any{"role": "user", "content": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendStep(ctx, "s", 1, map[string]any{"step": 1}); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextStep(ctx, "s")
	if err != nil || next != 2 {
		t.Fatalf("next=%d err=%v", next, err)
	}
	status := "interrupted"
	metadata := json.RawMessage(`{"workflow":{"status":"running"}}`)
	errJSON := json.RawMessage(`{"message":"boom"}`)
	if err := store.Update(ctx, "s", Patch{Status: &status, Metadata: &metadata, Error: &errJSON}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, "s", Patch{ClearError: true}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != status || len(got.Error) != 0 {
		t.Fatalf("session=%#v", got)
	}
	messages, err := store.RecentMessages(ctx, "s", 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%d err=%v", len(messages), err)
	}
}
