package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// hookProvider runs before on the first request, then answers with a final message.
type hookProvider struct {
	before func()
}

func (p *hookProvider) Model() string { return "hook" }
func (p *hookProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if p.before != nil {
		p.before()
		p.before = nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
}

// hostSetsKey simulates a host patch that lands in the store while a run is active.
func hostSetsKey(store *memoryStore, key string, value any) {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := cloneMetadata(store.state.Metadata)
	next[key] = value
	store.state.Metadata = next
}

func TestLoopMetadataWritesKeepHostKeysChangedDuringTheRun(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "s", Goal: "debug the failing migration and verify the fix", Status: "running",
			Metadata: map[string]any{"contextPaths": []any{"a.go"}}},
		steps: map[int64]any{},
	}
	ui := map[string]any{"title": "Renamed during run"}
	provider := &hookProvider{before: func() { hostSetsKey(store, "ui", ui) }}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}

	result, err := loop.Run(context.Background(), "s", Options{MaxSteps: 2, CognitionEnabled: true})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	if got := store.state.Metadata["ui"]; !reflect.DeepEqual(got, ui) {
		t.Fatalf("ui after the run = %#v, want %#v", got, ui)
	}
	if got := store.state.Metadata["contextPaths"]; !reflect.DeepEqual(got, []any{"a.go"}) {
		t.Fatalf("contextPaths after the run = %#v", got)
	}
	// The run did write its own keys, and only those.
	if _, ok := store.state.Metadata["cognition"]; !ok {
		t.Fatalf("cognition not persisted: %#v", store.state.Metadata)
	}
	if _, ok := store.state.Metadata["workUnits"]; !ok {
		t.Fatalf("workUnits not persisted: %#v", store.state.Metadata)
	}
	for _, key := range store.metadataKeyWrites {
		if !slices.Contains(loopMetadataKeys, key) {
			t.Fatalf("loop wrote host key %q (writes: %v)", key, store.metadataKeyWrites)
		}
	}
}

func TestLoopWritesNoMetadataWhenItOwnsNone(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "s", Goal: "plain", Status: "running", Metadata: map[string]any{"ui": map[string]any{"title": "t"}}},
		steps: map[int64]any{},
	}
	loop := Loop{Provider: &hookProvider{}, Store: store, Tools: fakeTools{}}
	if _, err := loop.Run(context.Background(), "s", Options{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if len(store.metadataKeyWrites) != 0 {
		t.Fatalf("metadata writes = %v, want none", store.metadataKeyWrites)
	}
}

func TestLoopDoesNotRewriteUnchangedLoopKeys(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "s", Goal: "plain", Status: "running", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	loop := Loop{Provider: &hookProvider{}, Store: store, Tools: fakeTools{}}
	if _, err := loop.Run(context.Background(), "s", Options{MaxSteps: 2, CognitionEnabled: true}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, key := range store.metadataKeyWrites {
		seen[key]++
	}
	// One step: workUnits is written once at the step end and not again at finish.
	if seen["workUnits"] != 1 {
		t.Fatalf("workUnits written %d times (%v), want 1", seen["workUnits"], store.metadataKeyWrites)
	}
}

type failingMetadataStore struct {
	*memoryStore
}

func (s failingMetadataStore) SetMetadataKey(context.Context, string, string, json.RawMessage) error {
	return errors.New("metadata write failed")
}

func TestLoopRecordsTerminalStatusWhenMetadataWriteFails(t *testing.T) {
	mem := &memoryStore{
		state: SessionState{ID: "s", Goal: "plain", Status: "running", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	loop := Loop{Provider: &hookProvider{}, Store: failingMetadataStore{mem}, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "s", Options{MaxSteps: 2, CognitionEnabled: true})
	if err == nil {
		t.Fatalf("expected an error, result=%#v", result)
	}
	if mem.state.Status != StatusCompleted {
		t.Fatalf("status=%q, want %q (the terminal write must happen despite the failed metadata write)", mem.state.Status, StatusCompleted)
	}
}
