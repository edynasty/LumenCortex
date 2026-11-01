package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// notebookStore opens a store with one session, ready for notebook tests.
func notebookStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Create(ctx, Session{ID: "s1", Goal: "notebook tests", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

func TestNotebookCreateReadUpdateAndDelete(t *testing.T) {
	store, ctx := notebookStore(t)
	body := "# Plan\n\n## Plan\n\n- [ ] read the code\n\n## Handoff\n\nstart with internal/session\n"
	doc, err := store.CreateNotebook(ctx, "s1", "plan", body, "agent:run1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.ID == "" || doc.Version != 1 || doc.Bytes != len(body) || doc.UpdatedBy != "agent:run1" {
		t.Fatalf("created = %#v", doc)
	}
	if strings.Join(doc.Sections, ",") != "Plan,Handoff" {
		t.Fatalf("sections = %v", doc.Sections)
	}

	// The id and the title both name the notebook.
	for _, ref := range []string{doc.ID, "plan"} {
		got, err := store.GetNotebook(ctx, "s1", ref)
		if err != nil {
			t.Fatalf("get %q: %v", ref, err)
		}
		if got.Body != body || got.Version != 1 {
			t.Fatalf("get %q = %#v", ref, got)
		}
	}
	if _, err := store.GetNotebook(ctx, "s1", "missing"); !errors.Is(err, ErrNotebookNotFound) {
		t.Fatalf("missing notebook: %v", err)
	}

	// An append extends the current version in one transaction.
	updated, err := store.UpdateNotebook(ctx, "s1", "plan", "agent:run2", "append", -1,
		func(current string) (string, error) { return current + "\n## Verification\n\nnot yet\n", nil })
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || !strings.Contains(updated.Body, "## Verification") || updated.UpdatedBy != "agent:run2" {
		t.Fatalf("updated = %#v", updated)
	}

	// A compare-and-set against the old version is refused and writes nothing.
	_, err = store.UpdateNotebook(ctx, "s1", "plan", "agent:run3", "replace", 1,
		func(string) (string, error) { return "clobbered", nil })
	var conflict *NotebookConflict
	if !errors.As(err, &conflict) || !errors.Is(err, ErrNotebookVersionConflict) || conflict.CurrentVersion != 2 {
		t.Fatalf("stale write = %v, want a conflict carrying version 2", err)
	}
	if got, _ := store.GetNotebook(ctx, "s1", "plan"); got.Version != 2 || got.Body != updated.Body {
		t.Fatalf("a refused write changed the notebook: %#v", got)
	}

	// The revision history holds both versions, newest first.
	revisions, err := store.NotebookRevisions(ctx, "s1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || revisions[0].Version != 2 || revisions[0].Op != "append" || revisions[1].Op != "create" {
		t.Fatalf("revisions = %#v", revisions)
	}
	if revisions[0].Body != "" {
		t.Fatal("a revision list carried a body")
	}
	first, err := store.NotebookRevision(ctx, "s1", "plan", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Body != body || first.Bytes != len(body) {
		t.Fatalf("revision 1 = %#v", first)
	}
	if _, err := store.NotebookRevision(ctx, "s1", "plan", 99); err == nil {
		t.Fatal("an unknown revision was accepted")
	}

	list, err := store.ListNotebooks(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "plan" || list[0].Version != 2 || list[0].Bytes != len(updated.Body) {
		t.Fatalf("list = %#v", list)
	}

	deleted, err := store.DeleteNotebook(ctx, "s1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Title != "plan" || deleted.Version != 2 {
		t.Fatalf("deleted metadata = %#v", deleted)
	}
	if _, err := store.GetNotebook(ctx, "s1", "plan"); !errors.Is(err, ErrNotebookNotFound) {
		t.Fatalf("deleted notebook still readable: %v", err)
	}
	if revisions, err := store.NotebookRevisions(ctx, "s1", "plan"); !errors.Is(err, ErrNotebookNotFound) || revisions != nil {
		t.Fatalf("deleted notebook's revisions: %v, %v", revisions, err)
	}
}

func TestNotebookLimitsAreRefused(t *testing.T) {
	store, ctx := notebookStore(t)

	big := strings.Repeat("x", MaxNotebookBytes+1)
	if _, err := store.CreateNotebook(ctx, "s1", "big", big, "host"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("oversized body = %v", err)
	}
	long := strings.Repeat("t", MaxNotebookTitleChars+1)
	if _, err := store.CreateNotebook(ctx, "s1", long, "x", "host"); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("oversized title = %v", err)
	}
	if _, err := store.CreateNotebook(ctx, "s1", "  ", "x", "host"); err == nil {
		t.Fatal("blank title accepted")
	}
	if _, err := store.CreateNotebook(ctx, "s1", "two\nlines", "x", "host"); err == nil {
		t.Fatal("multi-line title accepted")
	}
	if _, err := store.CreateNotebook(ctx, "ghost", "plan", "x", "host"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session = %v", err)
	}

	if _, err := store.CreateNotebook(ctx, "s1", "plan", "x", "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateNotebook(ctx, "s1", "plan", "y", "host"); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("duplicate title = %v", err)
	}
	for i := 2; i <= MaxNotebooksPerSession; i++ {
		if _, err := store.CreateNotebook(ctx, "s1", fmt.Sprintf("n%d", i), "x", "host"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateNotebook(ctx, "s1", "one too many", "x", "host"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("notebook count limit = %v", err)
	}
	// An update cannot grow a notebook past the body limit either.
	if _, err := store.UpdateNotebook(ctx, "s1", "plan", "host", "replace", 1,
		func(string) (string, error) { return big, nil }); err == nil {
		t.Fatal("an update grew the body past the limit")
	}
}

func TestNotebookRevisionsKeepTheNewestFifty(t *testing.T) {
	store, ctx := notebookStore(t)
	doc, err := store.CreateNotebook(ctx, "s1", "plan", "v1\n", "host")
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxNotebookRevisions + 10 {
		if _, err := store.UpdateNotebook(ctx, "s1", "plan", "host", "append", -1,
			func(current string) (string, error) { return current + fmt.Sprintf("line %d\n", i), nil }); err != nil {
			t.Fatal(err)
		}
	}
	revisions, err := store.NotebookRevisions(ctx, "s1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != MaxNotebookRevisions {
		t.Fatalf("kept %d revisions, want %d", len(revisions), MaxNotebookRevisions)
	}
	if newest, oldest := revisions[0].Version, revisions[len(revisions)-1].Version; newest != doc.Version+MaxNotebookRevisions+10 || oldest != newest-MaxNotebookRevisions+1 {
		t.Fatalf("kept versions %d..%d", oldest, newest)
	}
}

func TestNotebookConcurrentWritersHaveOneWinner(t *testing.T) {
	store, ctx := notebookStore(t)
	if _, err := store.CreateNotebook(ctx, "s1", "plan", "start\n", "host"); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.UpdateNotebook(ctx, "s1", "plan", fmt.Sprintf("agent:run%d", i), "replace", 1,
				func(string) (string, error) { return fmt.Sprintf("writer %d\n", i), nil })
		}(i)
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range results {
		if err == nil {
			won++
			continue
		}
		var conflict *NotebookConflict
		if !errors.As(err, &conflict) || conflict.CurrentVersion != 2 {
			t.Fatalf("loser got %v, want a conflict at version 2", err)
		}
		lost++
	}
	if won != 1 || lost != writers-1 {
		t.Fatalf("winners=%d losers=%d", won, lost)
	}
	doc, err := store.GetNotebook(ctx, "s1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 2 {
		t.Fatalf("version = %d, want 2", doc.Version)
	}
	revisions, err := store.NotebookRevisions(ctx, "s1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("revisions = %d, want 2 (create and the one winning write)", len(revisions))
	}
}

func TestNotebooksArePurgedWithTheirSession(t *testing.T) {
	store, ctx := notebookStore(t)
	doc, err := store.CreateNotebook(ctx, "s1", "plan", "# Plan\n\n## Handoff\nwork\n", "host")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateNotebook(ctx, "s1", "plan", "host", "append", doc.Version,
		func(current string) (string, error) { return current + "more\n", nil }); err != nil {
		t.Fatal(err)
	}
	deletion, _, err := store.BeginPrivacyDeletion(ctx, []string{"s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.OweCheckpoint(); err != nil {
		t.Fatal(err)
	}
	for {
		done, err := store.PurgeStep(ctx, deletion)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	for _, table := range []string{"notebooks", "notebook_revisions"} {
		var n int
		if err := store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s holds %d rows after the session was deleted", table, n)
		}
	}
}

func TestNotebookBodyIsContentNotJournal(t *testing.T) {
	store, ctx := notebookStore(t)
	doc, err := store.CreateNotebook(ctx, "s1", "plan", "SECRET-BODY", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Journal(ctx, "notebook.updated", map[string]any{
		"sessionId": "s1", "id": doc.ID, "version": doc.Version, "bytes": doc.Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := store.db.QueryRow(`SELECT payload_json FROM journal WHERE event = 'notebook.updated'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "SECRET-BODY") {
		t.Fatalf("journal row carries the body: %s", payload)
	}
	// The notebook id is an identifier, like a session or call id: the whitelist keeps it, and it is
	// not content.
	if !strings.Contains(payload, doc.ID) {
		t.Fatalf("journal row lost the notebook id: %s", payload)
	}
	if !strings.Contains(payload, `"bytes"`) {
		t.Fatalf("journal row lost the byte count: %s", payload)
	}
}

// TestNotebookStorageBytes measures what the notebook tables cost at their limits: 32 notebooks, each
// with 50 kept revisions, of 8 KiB bodies (ADR-0020 §Limits). It reports the database size rather than
// asserting one, because the figure depends on SQLite's page layout.
func TestNotebookStorageBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	store, ctx := notebookStore(t)
	body := strings.Repeat("x", 8<<10)
	for i := range MaxNotebooksPerSession {
		title := fmt.Sprintf("n%d", i)
		if _, err := store.CreateNotebook(ctx, "s1", title, body, "host"); err != nil {
			t.Fatal(err)
		}
		for range MaxNotebookRevisions - 1 {
			if _, err := store.UpdateNotebook(ctx, "s1", title, "host", "replace", -1,
				func(string) (string, error) { return body, nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	var notebooks, revisions int64
	if err := store.db.QueryRow(`SELECT count(*) FROM notebooks`).Scan(&notebooks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM notebook_revisions`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var pages, pageSize int64
	if err := store.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d notebooks x %d revisions x %d byte bodies: %d bytes of bodies, database %d bytes (%d pages x %d)",
		notebooks, revisions/MaxNotebooksPerSession, len(body), revisions*int64(len(body)), info.Size(), pages, pageSize)
}
