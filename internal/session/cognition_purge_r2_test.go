//go:build cgo

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/mattn/go-sqlite3"
)

// The SQLite authorizer refuses VACUUM's internal ATTACH, not the surrounding
// deletion transaction. This exercises a real SQLite failure without a production hook.
func TestCognitionPurgeVacuumFailureRetainsPendingAndRetries(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	const secret = "T5A-R2-VACUUM-PRIVATE-58c0fbf0"
	createSession(t, s, "source", "")
	createSession(t, s, "unrelated", "")
	if _, err := s.AppendMessage(ctx, "source", "assistant", protocol.Message{Role: "assistant", Content: secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, "unrelated", "assistant", protocol.Message{Role: "assistant", Content: "retained independent work"}); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "source", "completed")
	refs, err := s.MaintenanceSources(ctx, 0, state(t, s).Dirty, 10)
	if err != nil || len(refs) != 1 {
		t.Fatalf("source: %+v %v", refs, err)
	}
	if err := s.CreateDerivedSession(ctx, Session{ID: "maintainer", Goal: "maintain", Metadata: json.RawMessage(`{"purpose":"maintenance"}`)},
		protocol.Message{Role: "user", Content: secret}, []string{"source"}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate("kc_private", secret)
	c.SourceRefs = []string{refs[0].Ref.Ref()}
	c.SourceDigest = knowledge.Digest(c.SourceRefs...)
	commit := commitFor(lease, refs[0].Revision, "maintainer", c)
	commit.Sources = []knowledge.SourceRef{refs[0].Ref}
	if _, err := s.CommitMaintenance(ctx, commit, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, _, err := s.BeginPrivacyDeletion(ctx, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OweCheckpoint(); err != nil {
		t.Fatal(err)
	}
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("purge did not reach rebuild")
		}
		var rebuild int
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(tombstone_json, '$.rebuild'),0) FROM source_fences WHERE scope_kind='privacy' AND scope_ref='source'`).Scan(&rebuild); err != nil {
			t.Fatal(err)
		}
		if rebuild == 1 {
			break
		}
		if done, err := s.PurgeStep(ctx, d); err != nil || done {
			t.Fatalf("rows phase: %v %v", done, err)
		}
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rejected atomic.Int64
	if err := conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_ATTACH {
				rejected.Add(1)
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	clear := func() {
		c, err := s.db.Conn(ctx)
		if err == nil {
			_ = c.Raw(func(raw any) error { raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(nil); return nil })
			_ = c.Close()
		}
	}
	defer clear()
	for range 2 {
		if done, err := s.PurgeStep(ctx, d); done || err == nil || !strings.Contains(err.Error(), "rebuild database after a privacy deletion") {
			t.Fatalf("VACUUM failure reported complete: %v %v", done, err)
		}
		if pending, err := s.PendingPrivacyDeletion(ctx, "source"); err != nil || pending == nil {
			t.Fatalf("failed VACUUM lost retry: %+v %v", pending, err)
		}
		if withdrawn, err := s.SessionWithdrawn(ctx, "source"); err != nil || !withdrawn {
			t.Fatalf("failed purge exposes source: %v %v", withdrawn, err)
		}
	}
	if rejected.Load() != 2 {
		t.Fatalf("VACUUM was not the injected failure: %d attach denials", rejected.Load())
	}
	clear()
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("purge retry did not finish")
		}
		done, err := s.PurgeStep(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if done, err := s.PurgeStep(ctx, d); err != nil || !done {
		t.Fatalf("completed retry is not idempotent: %v %v", done, err)
	}
	if pending, err := s.PendingPrivacyDeletion(ctx, "source"); err != nil || pending != nil {
		t.Fatalf("finished deletion pending: %+v %v", pending, err)
	}
	var kept string
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(json,'$.content') FROM session_messages WHERE session_id='unrelated'`).Scan(&kept); err != nil || kept != "retained independent work" {
		t.Fatalf("unrelated session changed: %q %v", kept, err)
	}
	if rec, err := s.Candidate(ctx, c.ID); err != nil || rec.Claim != "" || rec.PurgedAt.IsZero() {
		t.Fatalf("candidate not purged: %+v %v", rec, err)
	}
	for _, path := range []string{s.Path(), s.Path() + "-wal", s.Path() + "-shm"} {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("private bytes remain in %s", path)
		}
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM session_messages WHERE session_id IN ('source','maintainer')`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("private transcript survived: %d %v", rows, err)
	}
	t.Log("VACUUM denied twice by SQLite authorizer; both attempts pending; successful retry cleaned all local DB files and retained unrelated source")
}
