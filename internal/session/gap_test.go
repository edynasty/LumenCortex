package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// These tests cover Reconcile (design §4.2, invariant I2): it closes the gap at the
// tail of a transcript and nothing else.

func gapStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func gapSession(t *testing.T, store *Store, id, status, errorJSON string) {
	t.Helper()
	v := Session{ID: id, Goal: "goal " + id, Status: status}
	if errorJSON != "" {
		v.Error = json.RawMessage(errorJSON)
	}
	if err := store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func gapRow(t *testing.T, store *Store, id, role string, m protocol.Message) {
	t.Helper()
	m.Role = role
	if _, err := store.AppendMessage(context.Background(), id, role, m); err != nil {
		t.Fatal(err)
	}
}

func calls(ids ...string) []protocol.ToolCall {
	out := make([]protocol.ToolCall, 0, len(ids))
	for _, id := range ids {
		out = append(out, protocol.ToolCall{ID: id, Name: "read_file", Arguments: json.RawMessage(`{}`)})
	}
	return out
}

func rowsOf(t *testing.T, store *Store, id string) []protocol.Message {
	t.Helper()
	rows, err := store.RecentMessages(context.Background(), id, 500)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]protocol.Message, 0, len(rows))
	for _, row := range rows {
		var m protocol.Message
		if err := json.Unmarshal(row.JSON, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestReconcileClosesOnlyTheUnansweredCallsOfTheTailGap(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	gapSession(t, store, "s", "interrupted", "")
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("c1", "c2", "c3")})
	gapRow(t, store, "s", "tool", protocol.Message{ToolCallID: "c2", Name: "read_file", Content: "saved"})

	inserted, err := store.Reconcile(ctx, "s")
	if err != nil || inserted != 2 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 2", inserted, err)
	}
	rows := rowsOf(t, store, "s")
	if len(rows) != 5 {
		t.Fatalf("%d rows, want 5: %+v", len(rows), rows)
	}
	for i, wantID := range map[int]string{3: "c1", 4: "c3"} {
		m := rows[i]
		if m.Role != "tool" || m.ToolCallID != wantID || m.Origin != protocol.OriginToolInterrupted || m.Content != InterruptedToolResult {
			t.Errorf("row %d = %+v, want the %s tool_interrupted result", i, m, wantID)
		}
	}
	if rows[2].Content != "saved" || rows[2].Origin != "" {
		t.Errorf("the saved result was changed: %+v", rows[2])
	}
	if again, err := store.Reconcile(ctx, "s"); err != nil || again != 0 {
		t.Fatalf("second Reconcile inserted=%d err=%v, want 0", again, err)
	}
}

func TestReconcileLeavesANonTailGapUntouched(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	gapSession(t, store, "s", "interrupted", "")
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("c1")})
	gapRow(t, store, "s", "user", protocol.Message{Content: "legacy follow-up appended after the gap"})
	gapRow(t, store, "s", "assistant", protocol.Message{Content: "answer"})

	inserted, err := store.Reconcile(ctx, "s")
	if err != nil || inserted != 0 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 0 for a gap followed by another row", inserted, err)
	}
	if got := len(rowsOf(t, store, "s")); got != 4 {
		t.Fatalf("%d rows, want the 4 original rows", got)
	}
}

func TestReconcileKeepsTheCauseOfANonRunningSession(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	const cause = `{"kind":"max_steps","message":"kept","at":"2026-10-04T00:00:00Z"}`
	gapSession(t, store, "s", "interrupted", cause)
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("c1")})

	if inserted, err := store.Reconcile(ctx, "s"); err != nil || inserted != 1 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 1", inserted, err)
	}
	got, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || string(got.Error) != cause {
		t.Fatalf("status=%q error=%s; the existing cause must stay", got.Status, got.Error)
	}
}

func TestReconcileMarksARunningSessionStaleRunRecovered(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	gapSession(t, store, "s", "running", "")
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("c1")})

	if inserted, err := store.Reconcile(ctx, "s"); err != nil || inserted != 1 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 1", inserted, err)
	}
	got, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var cause struct{ Kind, Message, At string }
	if err := json.Unmarshal(got.Error, &cause); err != nil {
		t.Fatalf("error_json %s: %v", got.Error, err)
	}
	if got.Status != "interrupted" || cause.Kind != KindStaleRunRecovered || cause.Message == "" || cause.At == "" {
		t.Fatalf("status=%q cause=%+v", got.Status, cause)
	}
}

func TestReconcileRunningSessionWithoutGapOnlyChangesStatus(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	gapSession(t, store, "s", "running", "")
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("c1")})
	gapRow(t, store, "s", "tool", protocol.Message{ToolCallID: "c1", Name: "read_file", Content: "ok"})

	if inserted, err := store.Reconcile(ctx, "s"); err != nil || inserted != 0 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 0", inserted, err)
	}
	if got, _ := store.Get(ctx, "s"); got.Status != "interrupted" {
		t.Fatalf("status=%q, want interrupted", got.Status)
	}
	if n := len(rowsOf(t, store, "s")); n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}
}

func TestReconcileUnknownSession(t *testing.T) {
	if _, err := gapStore(t).Reconcile(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestCreateWithMessageIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := gapStore(t)
	if err := store.CreateWithMessage(ctx, Session{ID: "s", Goal: "g"}, protocol.Message{Role: "user", Content: "first"}); err != nil {
		t.Fatal(err)
	}
	rows := rowsOf(t, store, "s")
	if len(rows) != 1 || rows[0].Content != "first" {
		t.Fatalf("rows=%+v", rows)
	}
	// A duplicate id fails and leaves no stray transcript row behind.
	if err := store.CreateWithMessage(ctx, Session{ID: "s", Goal: "g"}, protocol.Message{Role: "user", Content: "second"}); err == nil {
		t.Fatal("duplicate session id accepted")
	}
	if rows := rowsOf(t, store, "s"); len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestBeginRunWritesInputIdentityAndRunningTogether(t *testing.T) {
	store := gapStore(t)
	gapSession(t, store, "s", "completed", `{"kind":"cancelled","message":"m","at":"t"}`)
	if err := store.Update(context.Background(), "s", Patch{Final: ptr("old final")}); err != nil {
		t.Fatal(err)
	}
	gapRow(t, store, "s", "user", protocol.Message{Content: "goal"})

	seq, err := store.BeginRun(context.Background(), "s", RunStart{
		Input: &protocol.Message{Role: "user", Content: "next"}, Provider: ptr("p"), Model: ptr("m"),
	})
	if err != nil || seq != 1 {
		t.Fatalf("BeginRun = %d, %v; want seq 1", seq, err)
	}
	got, err := store.Get(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.Final != nil || len(got.Error) != 0 || got.Provider != "p" || got.Model != "m" {
		t.Fatalf("session after BeginRun: %+v", got)
	}
	rows := rowsOf(t, store, "s")
	if len(rows) != 2 || rows[1].Content != "next" || rows[1].Origin != "" {
		t.Fatalf("rows = %+v", rows)
	}

	if _, err := store.BeginRun(context.Background(), "missing", RunStart{Input: &protocol.Message{Role: "user", Content: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BeginRun on a missing session = %v, want ErrNotFound", err)
	}
}

func ptr(s string) *string { return &s }
