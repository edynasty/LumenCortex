package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// journalTestStore opens a store and returns it with a reader on its journal.
func journalTestStore(t *testing.T) (*Store, *journalReader) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, &journalReader{t: t, store: store}
}

type journalReader struct {
	t     *testing.T
	store *Store
}

// last returns the payload of the newest journal row.
func (r *journalReader) last() map[string]any {
	r.t.Helper()
	var payload string
	if err := r.store.db.QueryRow(`SELECT payload_json FROM journal ORDER BY seq DESC LIMIT 1`).Scan(&payload); err != nil {
		r.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		r.t.Fatalf("payload %s is not a JSON object: %v", payload, err)
	}
	return out
}

// The journal is a whitelist: a field the projection does not name is dropped, whatever it holds,
// and an event field added later stays private until it is named.
func TestJournalDropsUnnamedFields(t *testing.T) {
	store, journal := journalTestStore(t)
	ctx := context.Background()
	if err := store.Journal(ctx, "tool.start", map[string]any{
		"sessionId":     "session_1",
		"step":          int64(3),
		"name":          "write_file",
		"args":          map[string]any{"path": "note.txt", "content": "the body"},
		"goal":          "the goal",
		"content":       "the body",
		"futureField":   map[string]any{"name": "smuggled"},
		"aLongNamedOne": "x",
	}); err != nil {
		t.Fatal(err)
	}
	payload := journal.last()
	for _, kept := range []string{"sessionId", "step", "name"} {
		if _, ok := payload[kept]; !ok {
			t.Fatalf("payload %v dropped %q", payload, kept)
		}
	}
	for _, dropped := range []string{"args", "goal", "content", "futureField", "aLongNamedOne"} {
		if _, ok := payload[dropped]; ok {
			t.Fatalf("payload %v kept %q", payload, dropped)
		}
	}
	if payload["step"] != float64(3) {
		t.Fatalf("step=%v, want 3", payload["step"])
	}
}

// An event's own data is projected too: the agent loop journals {"sessionId", "data"}, and the
// data's whitelisted fields survive while the output chunk does not.
func TestJournalProjectsEventData(t *testing.T) {
	store, journal := journalTestStore(t)
	ctx := context.Background()
	if err := store.Journal(ctx, "tool.output", map[string]any{
		"sessionId": "session_1",
		"data": map[string]any{
			"step":       int64(2),
			"toolCallId": "call-1",
			"name":       "shell",
			"stream":     "stdout",
			"chunk":      "the output",
			"args":       map[string]any{"command": "the command"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	payload := journal.last()
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("payload %v lost the event data", payload)
	}
	for _, kept := range []string{"step", "toolCallId", "name", "stream"} {
		if _, ok := data[kept]; !ok {
			t.Fatalf("event data %v dropped %q", data, kept)
		}
	}
	for _, dropped := range []string{"chunk", "args"} {
		if _, ok := data[dropped]; ok {
			t.Fatalf("event data %v kept %q", data, dropped)
		}
	}
}

// A provider retry keeps its machine codes (class, failure reason, stop reason, status, counts) in the
// journal; the provider's error text does not survive.
func TestJournalKeepsProviderRetryCodes(t *testing.T) {
	store, journal := journalTestStore(t)
	if err := store.Journal(context.Background(), "provider.retry_stopped", map[string]any{
		"sessionId": "session_1",
		"data": map[string]any{
			"step": int64(3), "modelRef": "deepseek/flash", "class": "availability", "failure": "rate_limited",
			"status": 429, "stop": "deadline", "failedRequests": 7, "error": "provider HTTP 429: body from the gateway",
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := journal.last()["data"].(map[string]any)
	for _, kept := range []string{"step", "modelRef", "class", "failure", "status", "stop", "failedRequests"} {
		if _, ok := data[kept]; !ok {
			t.Fatalf("event data %v dropped %q", data, kept)
		}
	}
	if _, ok := data["error"]; ok {
		t.Fatalf("event data %v kept the error text", data)
	}
}

// A payload with nothing whitelisted, and one that is not an object at all, become an empty object:
// the column holds valid JSON either way.
func TestJournalWritesAnEmptyObjectWhenNothingSurvives(t *testing.T) {
	store, journal := journalTestStore(t)
	ctx := context.Background()
	for _, payload := range []any{
		map[string]any{"goal": "the goal", "output": "the output"},
		"a bare string",
		[]string{"a", "b"},
		nil,
	} {
		if err := store.Journal(ctx, "session.start", payload); err != nil {
			t.Fatalf("%#v: %v", payload, err)
		}
		if got := journal.last(); len(got) != 0 {
			t.Fatalf("%#v became %v, want {}", payload, got)
		}
	}
}

// A string is content unless its field is named and short: a named field carrying a long value is
// dropped rather than written.
func TestJournalDropsLongStringsEvenUnderANamedField(t *testing.T) {
	store, journal := journalTestStore(t)
	long := make([]byte, journalMaxString+1)
	for i := range long {
		long[i] = 'x'
	}
	if err := store.Journal(context.Background(), "tool.start", map[string]any{"name": string(long)}); err != nil {
		t.Fatal(err)
	}
	if got := journal.last(); len(got) != 0 {
		t.Fatalf("a %d-byte name was written: %v", len(long), got)
	}
}
