package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

const conversationUnits = `{"workUnits":[{"id":"fix","goal":"fix the rollback in the transaction path","checks":["ok"]}]}`

func blockedMessages(t *testing.T, engine *Engine, id string) int {
	t.Helper()
	page, err := engine.MessagePage(context.Background(), id, -1, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range page.Messages {
		var row protocol.Message
		if err := json.Unmarshal(m.JSON, &row); err != nil {
			t.Fatal(err)
		}
		if row.Origin == protocol.OriginCompletionBlocked {
			n++
		}
	}
	return n
}

// C1 through the engine (ADR-0006 D3): two conversation turns on a session with an open work unit and
// a required check. Each turn ends completed with its answer; the unit, the check obligation and the
// open resolution carry over to the next turn; no turn end runs a check; the reasoning echo serves only
// the tool turn that produced it and is nowhere at rest. A later non-interactive Start on the same
// session gets the full completion gate again.
func TestInteractiveConversationAcrossTurns(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"checks": {"ok": {"kind": "test", "command": "echo checked"}}`))
	engine, err := Open(Options{Workspace: echoWorkspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := engine.Events(4096)
	p := &echoProvider{script: []echoStep{
		{call: "c1", echo: echoSentinel + "-1"},
		{final: "The rollback is missing.", echo: echoSentinel + "-2"},
		{final: "In commit(), after the write fails.", echo: echoSentinel + "-3"},
		{final: "fixed"},
		{final: "fixed, really"},
	}}
	bindRef(engine, "a", p)
	ctx := context.Background()
	handle := newTestSession(t, engine, "explain the transaction bug")

	r1 := run(t, engine, handle.ID, AgentOptions{Interactive: true, WorkUnits: json.RawMessage(conversationUnits), MaxSteps: 5})
	r2 := run(t, engine, handle.ID, AgentOptions{Interactive: true, Input: "Where exactly?", MaxSteps: 5})
	if r1.Status != "completed" || r1.Final != "The rollback is missing." || r2.Status != "completed" || r2.Final != "In commit(), after the write fails." {
		t.Fatalf("turns: %+v / %+v", r1, r2)
	}
	if len(p.seen) != 3 || blockedMessages(t, engine, handle.ID) != 0 {
		t.Fatalf("requests %d, correction rows %d: each turn must end with its answer", len(p.seen), blockedMessages(t, engine, handle.ID))
	}
	if results, err := engine.CheckResults(ctx, handle.ID, -1, 0); err != nil || len(results) != 0 {
		t.Fatalf("a turn end ran checks: %+v err=%v", results, err)
	}

	info, err := engine.TaskState(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if open := info.State.Units().Incomplete(); len(open) != 1 || open[0].ID != "fix" {
		t.Fatalf("open units after two turns: %v", open)
	}
	if info.State.Resolution.State != taskstate.ResolutionOpen || !slices.Contains(info.Obligations, taskstate.ObligationWorkUnits) || !slices.Contains(info.Obligations, taskstate.ObligationChecks) {
		t.Fatalf("resolution %+v obligations %v", info.State.Resolution, info.Obligations)
	}

	// The echo of the tool turn went back inside that run only; the next turn's request has none, but
	// carries the Task State and the previous answer, under the conversation prompt for a read-only run.
	if got := p.seen[1].echoAt(); len(got) != 1 {
		t.Fatalf("the tool turn's echo was not sent back within its run: %v", got)
	}
	next := p.seen[2]
	if got := next.echoAt(); len(got) != 0 {
		t.Fatalf("the next turn's request carries echo %v", got)
	}
	raw, _ := json.Marshal(next.messages)
	for _, want := range []string{"fix the rollback in the transaction path", "The rollback is missing.", "Where exactly?"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("the second turn's request lacks %q", want)
		}
	}
	if system := next.messages[0].Content; !strings.Contains(system, "Your reply ends this turn, not the task") || !strings.Contains(system, "cannot change files") {
		t.Fatalf("system prompt of a read-only conversation turn: %q", system)
	}

	// The option belongs to the Start, not the session.
	r3 := run(t, engine, handle.ID, AgentOptions{Input: "Now fix it.", MaxSteps: 2})
	if r3.Status != "interrupted" || blockedMessages(t, engine, handle.ID) != 2 {
		t.Fatalf("non-interactive Start: %+v, correction rows %d", r3, blockedMessages(t, engine, handle.ID))
	}
	if system := p.seen[3].messages[0].Content; !strings.Contains(system, "until the requested outcome is achieved and verified") {
		t.Fatalf("system prompt of a task run: %q", system)
	}

	unsubscribe()
	if published := drainEvents(events); strings.Contains(published, echoSentinel) {
		t.Fatal("published events hold the echo")
	}
	path := engine.store.Path()
	engine.Close()
	assertNoSentinelAtRest(t, path)
}

// Subagent runs are never interactive (ADR-0006 D3), whatever their options say: a child with an open
// unit is held to the completion gate.
func TestSubagentRunsAreNeverInteractive(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	p := &answering{final: "found it"}
	bindRef(engine, "a", p)
	for _, c := range []struct {
		parent string
		want   string
		calls  int32
	}{{"", "completed", 1}, {"parent", "interrupted", 2}} {
		p.calls.Store(0)
		h := newTestSession(t, engine, "investigate")
		result, _ := engine.RunAgent(context.Background(), h.ID, AgentOptions{Interactive: true, MaxSteps: 2,
			WorkUnits: json.RawMessage(`{"workUnits":[{"id":"u","goal":"investigate"}]}`), parentSessionID: c.parent})
		if result.Status != c.want || p.calls.Load() != c.calls {
			t.Fatalf("parent %q: status %s after %d requests, want %s after %d", c.parent, result.Status, p.calls.Load(), c.want, c.calls)
		}
	}
}

// The evaluator path of a conversation turn (ADR-0006 D3, design §4.7): a turn that writes a file has
// the required check decided at its end by the real evaluator, and the pass latches the thread done; a
// following question runs no check and keeps the resolution.
func TestInteractiveTurnThatEditsDecidesChecksWithTheEvaluator(t *testing.T) {
	useChecks(t, `{"status": {"kind": "test", "command": "test -f ok.txt"}}`)
	engine, root := gitEngine(t)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "add the ok file"})
	if err != nil {
		t.Fatal(err)
	}
	p := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch n {
		case 1:
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "w1", Name: "write_file",
				Arguments: json.RawMessage(`{"path":"ok.txt","content":"ok\n"}`)}}}, FinishReason: "tool_calls"}, nil
		case 2:
			return protocol.ProviderResponse{Message: protocol.Message{Content: "Added ok.txt."}, FinishReason: "stop"}, nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "It holds one line."}, FinishReason: "stop"}, nil
	}}
	r1, err := engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Interactive: true, Policy: "workspace", MaxSteps: 4})
	if err != nil || r1.Status != "completed" || r1.Final != "Added ok.txt." {
		t.Fatalf("edit turn: %+v err=%v", r1, err)
	}
	results, err := engine.CheckResults(ctx, handle.ID, -1, 0)
	if err != nil || len(results) != 1 || results[0].Status != "passed" || results[0].Provenance != "runtime" {
		t.Fatalf("the edit turn's check results: %+v err=%v", results, err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatal(err)
	}
	info := taskResolution(t, engine, handle.ID)
	if r := info.State.Resolution; r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence || !slices.Contains(r.Refs, fmt.Sprintf("check:status@eval:%d", results[0].Seq)) {
		t.Fatalf("resolution after a passing edit turn: %+v", r)
	}

	r2, err := engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Interactive: true, Policy: "workspace", Input: "What is in it?", MaxSteps: 4})
	if err != nil || r2.Status != "completed" {
		t.Fatalf("question turn: %+v err=%v", r2, err)
	}
	if again, _ := engine.CheckResults(ctx, handle.ID, -1, 0); len(again) != 1 {
		t.Fatalf("a question turn decided checks: %+v", again)
	}
	if r := taskResolution(t, engine, handle.ID).State.Resolution; r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence {
		t.Fatalf("a question turn changed the resolution: %+v", r)
	}
}
