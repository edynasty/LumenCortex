package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func metadataMap(t *testing.T, engine *Engine, id string) map[string]any {
	t.Helper()
	_, info, err := engine.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(info.Metadata, &out); err != nil {
		t.Fatalf("metadata %s: %v", info.Metadata, err)
	}
	return out
}

func renameUI(t *testing.T, engine *Engine, id, title string) {
	t.Helper()
	if _, err := engine.UpdateSessionUI(context.Background(), id, SessionUIPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
}

// F1: a UI patch made while a run is active used to be lost when the run wrote
// its own metadata: {"ui":{"title":"Renamed during run"}} during the run, {} after it.
func TestF1UIMetadataSurvivesRunMetadataWrites(t *testing.T) {
	engine := openTestEngine(t)
	handle, err := engine.NewSession(context.Background(), SessionOptions{
		Goal:     "debug the failing migration and verify the fix",
		Metadata: map[string]any{"contextPaths": []string{"a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &exitProvider{fn: func(_ context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		renameUI(t, engine, handle.ID, "Renamed during run")
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
	}}

	result, err := engine.RunAgent(context.Background(), handle.ID, provider, AgentOptions{MaxSteps: 2, CognitionEnabled: true})
	if err != nil || result.Status != "completed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	metadata := metadataMap(t, engine, handle.ID)
	if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Renamed during run"}) {
		t.Fatalf("ui after the run = %#v, want the rename made during it; metadata=%#v", metadata["ui"], metadata)
	}
	if !reflect.DeepEqual(metadata["contextPaths"], []any{"a.go"}) {
		t.Fatalf("contextPaths=%#v", metadata["contextPaths"])
	}
	if _, ok := metadata["workUnits"]; !ok {
		t.Fatalf("the run's own metadata was not written: %#v", metadata)
	}
	if _, ok := metadata["cognition"]; !ok {
		t.Fatalf("the run's own metadata was not written: %#v", metadata)
	}
}

const humanGateWorkflow = `{"id":"approval","facts":{"ready":true},"actions":{"done":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},"gates":[{"id":"ship","type":"human","fact":"approved","equals":true}]}}}`

func waitingGateSession(t *testing.T, engine *Engine) *SessionHandle {
	t.Helper()
	handle, err := engine.NewSession(context.Background(), SessionOptions{
		Goal:     "ship it after approval",
		Metadata: map[string]any{"contextPaths": []string{"a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		t.Error("the provider must not be called while a gate is pending")
		return protocol.ProviderResponse{}, errors.New("unexpected call")
	}}
	result, err := engine.RunAgent(context.Background(), handle.ID, provider, AgentOptions{MaxSteps: 2, Workflow: json.RawMessage(humanGateWorkflow)})
	if err != nil || result.Status != "waiting_gate" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	return handle
}

// approvedGateIn reports whether the persisted workflow snapshot records the
// human gate "ship" as approved (fact approved == true).
func approvedGateIn(t *testing.T, engine *Engine, id string) bool {
	t.Helper()
	snapshot, _ := metadataMap(t, engine, id)["workflow"].(map[string]any)
	facts, _ := snapshot["facts"].(map[string]any)

	return facts["approved"] == true
}

func TestApprovalAndUIRenameBothSurvive(t *testing.T) {
	t.Run("rename then approve", func(t *testing.T) {
		engine := openTestEngine(t)
		handle := waitingGateSession(t, engine)
		renameUI(t, engine, handle.ID, "Renamed")
		if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); err != nil {
			t.Fatal(err)
		}
		metadata := metadataMap(t, engine, handle.ID)
		if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Renamed"}) || !approvedGateIn(t, engine, handle.ID) {
			t.Fatalf("metadata=%#v", metadata)
		}
	})
	t.Run("approve then rename", func(t *testing.T) {
		engine := openTestEngine(t)
		handle := waitingGateSession(t, engine)
		if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); err != nil {
			t.Fatal(err)
		}
		renameUI(t, engine, handle.ID, "Renamed")
		metadata := metadataMap(t, engine, handle.ID)
		if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Renamed"}) || !approvedGateIn(t, engine, handle.ID) {
			t.Fatalf("metadata=%#v", metadata)
		}
	})
	t.Run("rename racing the approval", func(t *testing.T) {
		for range 20 {
			engine := openTestEngine(t)
			handle := waitingGateSession(t, engine)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				title := "Renamed"
				if _, err := engine.UpdateSessionUI(context.Background(), handle.ID, SessionUIPatch{Title: &title}); err != nil {
					t.Error(err)
				}
			}()
			wg.Wait()
			metadata := metadataMap(t, engine, handle.ID)
			if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Renamed"}) || !approvedGateIn(t, engine, handle.ID) {
				t.Fatalf("a concurrent write was lost: %#v", metadata)
			}
		}
	})
}

func TestNewSessionRejectsCoreMetadataKeys(t *testing.T) {
	engine := openTestEngine(t)
	for _, key := range []string{session.MetadataKeyRuntime, session.MetadataKeySubagent} {
		_, err := engine.NewSession(context.Background(), SessionOptions{
			Goal:     "g",
			Metadata: map[string]any{key: map[string]any{"kind": "worktree", "path": "/tmp/elsewhere"}},
		})
		if !errors.Is(err, session.ErrCoreMetadataKey) {
			t.Errorf("NewSession with %q: err=%v, want ErrCoreMetadataKey", key, err)
		}
	}
	if _, err := engine.NewSession(context.Background(), SessionOptions{Goal: "g", Metadata: map[string]any{"ui": map[string]any{"title": "ok"}}}); err != nil {
		t.Fatalf("host metadata rejected: %v", err)
	}
}

func TestWorktreeBindingKeepsHostMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{
		Goal:     "isolated",
		Metadata: map[string]any{"contextPaths": []string{"a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	renameUI(t, engine, handle.ID, "Before attach")

	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	metadata := metadataMap(t, engine, handle.ID)
	if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Before attach"}) || !reflect.DeepEqual(metadata["contextPaths"], []any{"a.go"}) {
		t.Fatalf("attach changed host metadata: %#v", metadata)
	}
	if runtimeMeta, _ := metadata["runtime"].(map[string]any); runtimeMeta["path"] != identity.Path || runtimeMeta["kind"] != RuntimeWorktree {
		t.Fatalf("runtime=%#v want %#v", metadata["runtime"], identity)
	}

	renameUI(t, engine, handle.ID, "While attached")
	if err := engine.RemoveSessionWorktree(context.Background(), handle.ID, true); err != nil {
		t.Fatal(err)
	}
	metadata = metadataMap(t, engine, handle.ID)
	if _, ok := metadata["runtime"]; ok {
		t.Fatalf("runtime survived removal: %#v", metadata)
	}
	if !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "While attached"}) || !reflect.DeepEqual(metadata["contextPaths"], []any{"a.go"}) {
		t.Fatalf("remove changed host metadata: %#v", metadata)
	}
}

func TestSubagentSessionKeepsCoreMetadata(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentID := newParentSession(t, engine)
	controller := &subagentController{supervisor: supervisor, parentID: parentID, provider: &finalProvider{}, parentOpts: AgentOptions{ProviderName: "test-provider"}}
	value, err := controller.Spawn(context.Background(), "finish quickly")
	if err != nil {
		t.Fatal(err)
	}
	childID := value.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 5*time.Second)

	want := map[string]any{"parentSessionId": parentID, "mode": "read-only"}
	if got := metadataMap(t, engine, childID)["subagent"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("subagent=%#v want %#v", got, want)
	}
	// A host UI patch and the run's own writes leave the core key alone.
	renameUI(t, engine, childID, "Child")
	metadata := metadataMap(t, engine, childID)
	if !reflect.DeepEqual(metadata["subagent"], want) || !reflect.DeepEqual(metadata["ui"], map[string]any{"title": "Child"}) {
		t.Fatalf("metadata=%#v", metadata)
	}
	// Hosts cannot overwrite it through the store's host path.
	if err := engine.store.SetHostMetadataKey(context.Background(), childID, session.MetadataKeySubagent, json.RawMessage(`{"parentSessionId":"x"}`)); !errors.Is(err, session.ErrCoreMetadataKey) {
		t.Fatalf("host overwrite of subagent: err=%v", err)
	}
}
