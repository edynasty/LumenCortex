package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestSessionHistoryIsPagedFromSQLite(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Create(ctx, Session{ID: "session_test", Goal: "long task"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if _, err := store.AppendMessage(ctx, "session_test", "tool", map[string]any{"index": i, "body": fmt.Sprintf("message-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.RecentMessages(ctx, "session_test", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 6 {
		t.Fatalf("got %d recent messages, want 6", len(messages))
	}
	if messages[0].Seq != 994 || messages[5].Seq != 999 {
		t.Fatalf("unexpected recent sequence range: %d..%d", messages[0].Seq, messages[5].Seq)
	}
}
