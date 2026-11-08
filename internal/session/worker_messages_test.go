package session

import (
	"context"
	"errors"
	"testing"
)

func TestWorkerMessagesReopenFencesAndPrivacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"root", "a", "b", "foreign"} {
		if err = s.Create(ctx, Session{ID: id, Goal: "messages"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if err = s.AddRelation(ctx, Relation{ParentSessionID: "root", ChildSessionID: id, Kind: "execution"}); err != nil {
			t.Fatal(err)
		}
	}
	message, err := s.SendWorkerMessage(ctx, WorkerMessage{RootID: "root", SenderID: "a", RecipientID: "b", ID: "one", Body: "private-source-71"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.ReadWorkerMessages(ctx, "root", "b", 0, 1, true)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Seq != message.Seq || page.Messages[0].Body != "private-source-71" || page.Messages[0].ReceivedAt == nil {
		t.Fatalf("reopen %+v %v", page, err)
	}
	page, err = s.ReadWorkerMessages(ctx, "root", "b", page.Cursor, 1, true)
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("cursor repeated %+v %v", page, err)
	}
	if _, err = s.ReadWorkerMessages(ctx, "root", "foreign", 0, 10, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign %v", err)
	}
	if _, err = s.RevokeSession(ctx, "a", "owner_revoke"); err != nil {
		t.Fatal(err)
	}
	page, err = s.ReadWorkerMessages(ctx, "root", "b", 0, 10, false)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != "" || !page.Messages[0].Withdrawn {
		t.Fatalf("fence %+v %v", page, err)
	}
	if _, err = s.SendWorkerMessage(ctx, WorkerMessage{RootID: "root", SenderID: "a", RecipientID: "b", ID: "two", Body: "forbidden"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("revoked sender %v", err)
	}
	deletion, _, err := s.BeginPrivacyDeletion(ctx, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	done := false
	for i := 0; i < 30 && !done; i++ {
		done, err = s.PurgeStep(ctx, deletion)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("privacy cleanup did not finish")
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM worker_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("message content survived source deletion %d %v", count, err)
	}
}
