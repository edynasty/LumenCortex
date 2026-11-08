package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edynasty/LumenCortex/pluginapi"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeEventHistoryReopensWithSequencesAndIsolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err = s.Create(ctx, Session{ID: id, Goal: "event test"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AppendRuntimeEvent(ctx, pluginapi.Event{ID: fmt.Sprintf("event-%d", i), SessionID: "one", RunID: "run", Type: "tool.completed", At: time.Now(), Data: json.RawMessage(`{"name":"read_file","reasoning":"PRIVATE","args":{"token":"SECRET"},"ok":true}`)})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: "one", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 10 || !page.HasMore {
		t.Fatalf("page %+v", page)
	}
	all := page.Events
	for page.HasMore {
		page, err = s.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: "one", Limit: 10, After: page.Cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Events...)
	}
	if len(all) != 32 {
		t.Fatalf("events=%d", len(all))
	}
	for i, event := range all {
		if event.Sequence != uint64(i+1) || event.Cursor == 0 || event.ID == "" {
			t.Fatalf("identity %+v", event)
		}
		if strings.Contains(string(event.Data), "PRIVATE") || strings.Contains(string(event.Data), "SECRET") {
			t.Fatal("private data persisted")
		}
	}
	other, err := s.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: "two"})
	if err != nil || len(other.Events) != 0 {
		t.Fatalf("cross-session leak %+v %v", other, err)
	}
	duplicate, err := s.AppendRuntimeEvent(ctx, all[0])
	if err != nil || duplicate.Cursor != all[0].Cursor {
		t.Fatalf("duplicate %+v %v", duplicate, err)
	}
	all[0].SessionID = "two"
	if _, err = s.AppendRuntimeEvent(ctx, all[0]); !errors.Is(err, ErrExists) {
		t.Fatalf("cross-session ID accepted: %v", err)
	}
}

func TestRuntimeEventRetentionSignalsExpiredCursor(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Create(ctx, Session{ID: "s", Goal: "retention"}); err != nil {
		t.Fatal(err)
	}
	// Seed a full metadata window in one transaction; exercise public append across its boundary.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxRuntimeEventsPerSession; i++ {
		_, err = tx.ExecContext(ctx, `INSERT INTO runtime_events(id,session_id,run_id,sequence,type,event_json) VALUES(?,'s','seed',?,'run.started','{}')`, fmt.Sprintf("seed-%d", i), i)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.AppendRuntimeEvent(ctx, pluginapi.Event{ID: fmt.Sprintf("fresh-%d", i), SessionID: "s", RunID: "new", Type: "run.started", Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: "s", After: 1})
	if !errors.Is(err, ErrEventCursorExpired) || page.PrunedThrough != 2 {
		t.Fatalf("cursor gap hidden %+v %v", page, err)
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM runtime_events`).Scan(&count)
	if count != MaxRuntimeEventsPerSession {
		t.Fatalf("unbounded history %d", count)
	}
}
