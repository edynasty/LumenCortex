//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestR5RedR402DLoopAndCallbackDropPartialDetails reproduces R4-02-D on unmodified 0b3b8e72:
// Loop.Run (loop.go:707) and callbackDispatcher.Call (callback.go:314) overwrite toolset's
// partial-publication JSON with plain {"error":"input/output error"}.
func TestR5RedR402DLoopAndCallbackDropPartialDetails(t *testing.T) {
	ctx := context.Background()
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepoForTest(t, ws)
	editEngine, err := editing.New(editing.Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
		CloseAfterPublish: func(string) error { return syscall.EIO },
	})
	tools, err := toolset.New(toolset.Options{
		Workspace: ws,
		Policy:    toolset.PolicyFull,
		SessionID: "s-r5-d",
		Editing:   editEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "r5d.db")
	raw, err := session.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.Create(ctx, session.Session{ID: "s-r5-d", Goal: "R4-02-D", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.BeginRun(ctx, "s-r5-d", session.RunStart{
		RunID: "run-1",
		Input: &protocol.Message{Role: "user", Content: "create artifact"},
	}); err != nil {
		t.Fatal(err)
	}
	st := r401SettleStore{realSQLiteAgentStore{s: raw}}

	const ref = config.ModelRef("test/m")
	prov := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("c-top", "write_file", `{"path":"top_pub.txt","content":"hello\n"}`)),
		answerOf("done"),
	}}
	_, _ = (&Loop{
		Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
		Tools:     tools,
		Store:     st,
		Snapshots: repo,
	}).Run(ctx, "s-r5-d", Options{Session: ref, RunID: "run-1", MaxSteps: 1})

	var toolContent string
	_ = st.Transcript(ctx, "s-r5-d", 0, func(_ int64, m protocol.Message) error {
		if m.Role == "tool" && m.ToolCallID == "c-top" {
			toolContent = m.Content
		}
		return nil
	})
	t.Logf("top-level tool row content = %s", toolContent)
	if !strings.Contains(toolContent, "top_pub.txt") || !strings.Contains(toolContent, "published_with_post_error") {
		t.Errorf("R4-02-D DEFECT (loop.go): top-level tool row dropped partial publication details: %s", toolContent)
	}

	// Nested callback check directly through callbackDispatcher.Call
	d := &callbackDispatcher{
		loop:       &Loop{Store: st, Tools: tools, Snapshots: repo},
		sessionID:  "s-r5-d",
		runID:      "run-1",
		requestID:  "req-cb",
		step:       2,
		parentCall: "c-eval",
		specs:      tools.Specs(nil),
		spend:      &Spend{},
		journal:    &callbackJournal{},
	}
	f0, l0 := int64(0), int64(0)
	if _, err := st.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "req-cb", SessionID: "s-r5-d", RunID: "run-1", Purpose: session.PurposeMain,
		Step: 2, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2, Refs: []string{"model:test/m", "proj:mode:full", "ts:v0"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendAnswered(ctx, "s-r5-d", "req-cb", protocol.Message{
		Role:      "assistant",
		ToolCalls: []protocol.ToolCall{{ID: "c-eval", Name: "eval", Arguments: json.RawMessage(`{}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	cbRes, _ := d.Call(ctx, "write_file", map[string]any{"path": "nested_pub.txt", "content": "nested\n"})
	t.Logf("nested callback result content = %s", cbRes.Content)
	if !strings.Contains(cbRes.Content, "nested_pub.txt") || !strings.Contains(cbRes.Content, "published_with_post_error") {
		t.Errorf("R4-02-D DEFECT (callback.go): nested callback dropped partial publication details: %s", cbRes.Content)
	}
}
