package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestCognitionExpandedFenceDoesNotPurgeUnrelatedMaintenance(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "source", "")
	createSession(t, s, "unrelated", "")
	finish(t, s, "unrelated", "completed")
	if err := s.CreateDerivedSession(ctx, Session{ID: "mU", Goal: "maintain unrelated", Metadata: json.RawMessage(`{"purpose":"maintenance"}`)},
		protocol.Message{Role: "user", Content: "independent input"}, []string{"unrelated"}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate("kc_unrelated", "independent retained claim")
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "mU", c), t0); err != nil {
		t.Fatal(err)
	}
	createSession(t, s, "q", "")
	copy := protocol.Message{Role: "tool", ToolCallID: "read", Content: c.ID + " independent retained claim"}
	if _, err := s.AppendMessage(ctx, "q", "tool", copy); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxRevokeSessions; i++ {
		id := fmt.Sprintf("reader_%03d", i)
		createSession(t, s, id, "")
		insertEdge(t, s, id, "message", "tool:r:c", SourceSession, "source")
	}
	d, rev, err := s.BeginPrivacyDeletion(ctx, []string{"source"})
	if err != nil || !rev.Expanded {
		t.Fatalf("wide fence fixture: %+v %v", rev, err)
	}
	if slices.Contains(d.Derived, "mU") {
		t.Error("unrelated maintenance was included in destructive purge")
	}
	if err := s.OweCheckpoint(); err != nil {
		t.Fatal(err)
	}
	for n := 0; ; n++ {
		if n > 1000 {
			t.Fatal("purge did not finish")
		}
		done, err := s.PurgeStep(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if got, err := s.Candidate(ctx, c.ID); err != nil || got.Claim != c.Claim || !got.PurgedAt.IsZero() {
		t.Fatalf("wide admission fence destroyed unrelated cognition: %+v %v", got, err)
	}
	var kept string
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(json,'$.content') FROM session_messages WHERE session_id='q'`).Scan(&kept); err != nil || kept != copy.Content {
		t.Fatalf("unrelated reader copy changed: %q %v", kept, err)
	}
	if fenced, err := s.SessionWithdrawn(ctx, "mU"); err != nil || !fenced {
		t.Fatalf("existing wide admission fence weakened: %v %v", fenced, err)
	}
}
