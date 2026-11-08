package runtime

import (
	"context"
	"encoding/json"
	"github.com/edynasty/LumenCortex/protocol"
	"strings"
	"testing"
)

func TestInspectorResolvesToolContentWithinRun(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, path := range []string{"first-marker", "second-marker"} {
		raw, _ := json.Marshal(map[string]string{"path": path, "password": "PASSWORD_SENTINEL"})
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{
			{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "same-call", Name: "list_dir", Arguments: raw}}}},
			{Message: protocol.Message{Content: "observed"}},
		}}
		if _, err = engine.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 3}); err != nil {
			t.Fatal(err)
		}
		page, err := engine.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: handle.ID, Type: "tool.end"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, page.Events[len(page.Events)-1].ID)
	}
	for i, marker := range []string{"first-marker", "second-marker"} {
		detail, err := engine.GetRuntimeEventDetails(ctx, handle.ID, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if detail.ContentStatus != "bounded_transcript" || !strings.Contains(string(detail.Arguments), marker) || strings.Contains(string(detail.Arguments), "PASSWORD_SENTINEL") {
			t.Fatalf("incorrect or unsafe detail %+v", detail)
		}
	}
	foreign, err := engine.NewSession(ctx, SessionOptions{Goal: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.GetRuntimeEventDetails(ctx, foreign.ID, ids[0]); err == nil {
		t.Fatal("cross-session detail disclosed")
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	tree, err := supervisor.GetWorkerTree(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, current, err := engine.Session(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Root.SessionID != handle.ID || tree.Root.ModelRef != current.ModelRef || tree.Root.Active {
		t.Fatalf("incorrect worker snapshot %+v", tree)
	}
}
