package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func openMetadataStore(t *testing.T, metadata string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Create(context.Background(), Session{ID: "s", Goal: "goal", Status: "created", Metadata: json.RawMessage(metadata)}); err != nil {
		t.Fatal(err)
	}
	return store
}

func metadataOf(t *testing.T, store *Store) map[string]any {
	t.Helper()
	got, err := store.Get(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(got.Metadata, &out); err != nil {
		t.Fatalf("metadata %s: %v", got.Metadata, err)
	}
	return out
}

func TestSetHostMetadataKeyReplacesOneKeyAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"ui":{"title":"old"},"contextPaths":["a.go"],"runtime":{"kind":"worktree","path":"/w"}}`)

	if err := store.SetHostMetadataKey(ctx, "s", "ui", json.RawMessage(`{"title":"new","pinned":true}`)); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ui":           map[string]any{"title": "new", "pinned": true},
		"contextPaths": []any{"a.go"},
		"runtime":      map[string]any{"kind": "worktree", "path": "/w"},
	}
	if got := metadataOf(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata=%#v want %#v", got, want)
	}

	// A new key is added as structured JSON, not as an encoded string.
	if err := store.SetHostMetadataKey(ctx, "s", "workflow", json.RawMessage(`{"status":"running"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadataOf(t, store)["workflow"].(map[string]any); !ok {
		t.Fatalf("workflow stored as %#v", metadataOf(t, store)["workflow"])
	}
}

func TestRemoveHostMetadataKeyRemovesOneKeyAndKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"ui":{"title":"t"},"contextPaths":["a.go"]}`)

	if err := store.RemoveHostMetadataKey(ctx, "s", "ui"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveHostMetadataKey(ctx, "s", "absent"); err != nil {
		t.Fatalf("removing an absent key: %v", err)
	}
	want := map[string]any{"contextPaths": []any{"a.go"}}
	if got := metadataOf(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata=%#v want %#v", got, want)
	}
}

func TestMetadataKeyValidation(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"keep":1}`)
	invalid := []string{"", strings.Repeat("a", 65), "a-b", "a.b", "a b", `a"b`, "a$", "ключ", "a\nb", "$.x"}
	for _, key := range invalid {
		if err := store.SetHostMetadataKey(ctx, "s", key, json.RawMessage(`1`)); !errors.Is(err, ErrInvalidMetadataKey) {
			t.Errorf("set %q: err=%v, want ErrInvalidMetadataKey", key, err)
		}
		if err := store.RemoveHostMetadataKey(ctx, "s", key); !errors.Is(err, ErrInvalidMetadataKey) {
			t.Errorf("remove %q: err=%v, want ErrInvalidMetadataKey", key, err)
		}
	}
	for _, key := range []string{"a", "A_9", "_", "0", strings.Repeat("a", 64)} {
		if err := store.SetHostMetadataKey(ctx, "s", key, json.RawMessage(`1`)); err != nil {
			t.Errorf("set %q: %v", key, err)
		}
	}
	if err := store.SetHostMetadataKey(ctx, "s", "bad", json.RawMessage(`{not json`)); !errors.Is(err, ErrInvalidMetadataValue) {
		t.Fatalf("invalid value: err=%v", err)
	}
	if _, ok := metadataOf(t, store)["bad"]; ok {
		t.Fatal("invalid value was stored")
	}
	if err := store.SetHostMetadataKey(ctx, "missing", "k", json.RawMessage(`1`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: err=%v", err)
	}
	if err := store.RemoveHostMetadataKey(ctx, "missing", "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: err=%v", err)
	}
}

func TestHostMetadataWritesRejectCoreKeys(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"runtime":{"kind":"worktree","path":"/w"},"subagent":{"parentSessionId":"p"}}`)
	before := metadataOf(t, store)

	for _, key := range []string{MetadataKeyRuntime, MetadataKeySubagent} {
		if err := store.SetHostMetadataKey(ctx, "s", key, json.RawMessage(`{"kind":"local"}`)); !errors.Is(err, ErrCoreMetadataKey) {
			t.Errorf("host set %q: err=%v, want ErrCoreMetadataKey", key, err)
		}
		if err := store.RemoveHostMetadataKey(ctx, "s", key); !errors.Is(err, ErrCoreMetadataKey) {
			t.Errorf("host remove %q: err=%v, want ErrCoreMetadataKey", key, err)
		}
	}
	if got := metadataOf(t, store); !reflect.DeepEqual(got, before) {
		t.Fatalf("a rejected host write changed metadata: %#v want %#v", got, before)
	}
}

func TestCoreMetadataWritesTouchOnlyCoreKeys(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"ui":{"title":"t"}}`)

	if err := store.SetCoreMetadataKey(ctx, "s", MetadataKeyRuntime, json.RawMessage(`{"kind":"worktree","path":"/w"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCoreMetadataKey(ctx, "s", MetadataKeySubagent, json.RawMessage(`{"parentSessionId":"p"}`)); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ui":       map[string]any{"title": "t"},
		"runtime":  map[string]any{"kind": "worktree", "path": "/w"},
		"subagent": map[string]any{"parentSessionId": "p"},
	}
	if got := metadataOf(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata=%#v want %#v", got, want)
	}

	if err := store.RemoveCoreMetadataKey(ctx, "s", MetadataKeyRuntime); err != nil {
		t.Fatal(err)
	}
	delete(want, "runtime")
	if got := metadataOf(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata=%#v want %#v", got, want)
	}

	// The core path cannot be used to write host keys.
	if err := store.SetCoreMetadataKey(ctx, "s", "ui", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidMetadataKey) {
		t.Fatalf("core set of a host key: err=%v", err)
	}
	if err := store.RemoveCoreMetadataKey(ctx, "s", "ui"); !errors.Is(err, ErrInvalidMetadataKey) {
		t.Fatalf("core remove of a host key: err=%v", err)
	}
}

func TestUpdateLeavesMetadataAndOtherColumnsAlone(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{"ui":{"title":"t"}}`)
	final := "done"
	usage := json.RawMessage(`{"totalTokens":3}`)
	if err := store.Update(ctx, "s", Patch{Final: &final, Usage: &usage}); err != nil {
		t.Fatal(err)
	}
	// Status is written by a different statement than the metadata key.
	status := "completed"
	if err := store.SetHostMetadataKey(ctx, "s", "extra", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, "s", Patch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Final == nil || *got.Final != "done" || string(got.Usage) != `{"totalTokens":3}` {
		t.Fatalf("columns=%#v", got)
	}
	md := metadataOf(t, store)
	if _, ok := md["ui"]; !ok || md["extra"] != float64(1) {
		t.Fatalf("metadata=%#v", md)
	}
	if err := store.Update(ctx, "missing", Patch{Status: &status}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: err=%v", err)
	}
}

func TestConcurrentSingleKeyPatchesAreNotLost(t *testing.T) {
	ctx := context.Background()
	store := openMetadataStore(t, `{}`)
	const writers = 32
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			if err := store.SetHostMetadataKey(ctx, "s", key, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := metadataOf(t, store); len(got) != writers {
		t.Fatalf("got %d keys, want %d: %#v", len(got), writers, got)
	}
}

func TestCheckHostMetadata(t *testing.T) {
	for _, ok := range []string{``, `null`, `{}`, `{"ui":{"title":"x"},"contextPaths":[]}`} {
		if err := CheckHostMetadata(json.RawMessage(ok)); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"runtime":{}}`, `{"a":1,"subagent":null}`} {
		if err := CheckHostMetadata(json.RawMessage(bad)); !errors.Is(err, ErrCoreMetadataKey) {
			t.Errorf("%q: err=%v, want ErrCoreMetadataKey", bad, err)
		}
	}
	if err := CheckHostMetadata(json.RawMessage(`[1]`)); err == nil {
		t.Error("a non-object metadata value was accepted")
	}
}
