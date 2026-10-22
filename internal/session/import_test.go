package session

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func importItem(id, parent string) ImportedSession {
	item := ImportedSession{
		Session:  Session{ID: id, Status: "completed", Goal: "g"},
		Messages: []protocol.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
	}
	if parent != "" {
		item.Relation = &Relation{ParentSessionID: parent, ChildSessionID: id, Kind: "subagent"}
	}
	return item
}

func countSessions(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An import writes every session, row and relation or nothing: a failure on a later item rolls back
// the earlier ones, and an id that exists writes nothing.
func TestImportSessionsIsAllOrNothing(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	bad := importItem("session_c", "session_a")
	bad.Relation.ChildSessionID = "session_other"
	if err := store.ImportSessions(ctx, []ImportedSession{importItem("session_a", ""), bad}); err == nil {
		t.Fatal("an invalid relation was accepted")
	}
	if n := countSessions(t, store); n != 0 {
		t.Fatalf("a failed import left %d sessions", n)
	}
	running := importItem("session_a", "")
	running.Session.Status = "running"
	if err := store.ImportSessions(ctx, []ImportedSession{running}); err == nil {
		t.Fatal("a running imported session was accepted")
	}

	if err := store.ImportSessions(ctx, []ImportedSession{importItem("session_a", ""), importItem("session_b", "session_a")}); err != nil {
		t.Fatal(err)
	}
	if err := store.ImportSessions(ctx, []ImportedSession{importItem("session_z", ""), importItem("session_a", "")}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err=%v", err)
	}
	if n := countSessions(t, store); n != 2 {
		t.Fatalf("sessions=%d", n)
	}
	relation, err := store.ParentRelation(ctx, "session_b")
	if err != nil || relation.ParentSessionID != "session_a" {
		t.Fatalf("relation %+v %v", relation, err)
	}
	if next, err := store.NextMessageSeq(ctx, "session_b"); err != nil || next != 2 {
		t.Fatalf("next seq %d %v", next, err)
	}
}
