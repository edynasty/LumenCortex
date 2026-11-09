package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// callbackFakeTools offers one read tool, one write tool, bash and the eval family, with the
// permissions the real catalog carries.
type callbackFakeTools struct{ calls []string }

func (f *callbackFakeTools) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{
		{Name: "read_file", Permission: "read"},
		{Name: "write_file", Permission: "write", MutatesWorkspace: true, Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required": []any{"path"}, "additionalProperties": false,
		}},
		{Name: "bash", Permission: "exec", MutatesWorkspace: true},
		{Name: "eval", Permission: "exec", MutatesWorkspace: true},
		{Name: "eval_create", Permission: "exec"},
		{Name: "spawn_subagent"},
		{Name: "mcp__docs__read", Permission: "read"},
		{Name: "mcp__shell__run", Permission: "exec"},
	}
}

func (f *callbackFakeTools) Execute(_ context.Context, name string, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	f.calls = append(f.calls, name)
	switch name {
	case "read_file", "mcp__docs__read":
		return protocol.ToolResult{OK: true, Content: `{"content":"ok"}`}, nil
	case "write_file":
		return protocol.ToolResult{OK: true, MutatesWorkspace: true, Content: `{"changed":true}`}, nil
	}
	return protocol.ToolResult{}, errors.New("unknown tool")
}

func callbackLoop(tools *callbackFakeTools) *Loop { return &Loop{Tools: tools} }

func callbackSpecs() []protocol.ToolSpec { return (&callbackFakeTools{}).Specs(nil) }

func TestCallbackToolsIsTheSandboxSubset(t *testing.T) {
	d := &callbackDispatcher{loop: callbackLoop(&callbackFakeTools{}), specs: callbackSpecs(),
		spend: &Spend{}, budget: config.Budget{}, journal: &callbackJournal{}}
	specs, err := d.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"mcp__docs__read", "read_file", "write_file"}) {
		t.Fatalf("tools=%v", names)
	}
	for _, denied := range []string{"bash", "eval", "eval_create", "spawn_subagent", "mcp__shell__run"} {
		if slices.Contains(names, denied) {
			t.Fatalf("%s must not be offered to a cell: %v", denied, names)
		}
	}
}

func TestCallbackRefusesExecAndRecursion(t *testing.T) {
	tools := &callbackFakeTools{}
	d := &callbackDispatcher{loop: callbackLoop(tools), specs: callbackSpecs(), spend: &Spend{},
		budget: config.Budget{}, journal: &callbackJournal{}}
	for _, name := range []string{"bash", "eval", "eval_create", "spawn_subagent", "mcp__shell__run"} {
		result, err := d.Call(context.Background(), name, map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !result.Denied || result.OK {
			t.Fatalf("%s: result=%+v, want a denied result", name, result)
		}
	}
	if len(tools.calls) != 0 {
		t.Fatalf("a refused callback reached the tool: %v", tools.calls)
	}
	result, err := d.Call(context.Background(), "read_file", map[string]any{"path": "a"})
	if err != nil || !result.OK {
		t.Fatalf("read_file: %+v %v", result, err)
	}
	if !slices.Equal(tools.calls, []string{"read_file"}) {
		t.Fatalf("calls=%v", tools.calls)
	}
}

// settlingStore settles a nested execution on its own, as the real store does.
type settlingStore struct {
	memoryStore
	err     error
	settled int
}

func (s *settlingStore) SettleExecution(context.Context, string, session.ExecutionResult, ...session.DerivedEdge) error {
	s.settled++
	return s.err
}

// A workspace write runs only when its execution can be settled; a settlement failure is never a
// successful callback.
func TestCallbackSettlesWorkspaceWritesOrFails(t *testing.T) {
	specs := callbackSpecs()
	// No settler: a side-effecting tool is refused.
	noSettler := &callbackDispatcher{loop: callbackLoop(&callbackFakeTools{}), specs: specs, spend: &Spend{},
		budget: config.Budget{}, journal: &callbackJournal{}}
	result, err := noSettler.Call(context.Background(), "write_file", map[string]any{"path": "a"})
	if err != nil || !result.Denied {
		t.Fatalf("without a settler: %+v %v", result, err)
	}

	// A settler that succeeds: the write is admitted and settled without a transcript row.
	store := &settlingStore{}
	store.state = SessionState{ID: "s", Status: "running"}
	store.steps = map[int64]any{}
	ok := &callbackDispatcher{loop: &Loop{Store: store, Tools: &callbackFakeTools{}}, specs: specs,
		spend: &Spend{}, budget: config.Budget{}, journal: &callbackJournal{}}
	result, err = ok.Call(context.Background(), "write_file", map[string]any{"path": "a"})
	if err != nil || !result.OK || store.settled != 1 {
		t.Fatalf("with a settler: %+v %v settled=%d", result, err, store.settled)
	}

	// A settlement failure must reach the worker as a failure, never as success.
	broken := &settlingStore{err: errors.New("fence")}
	broken.state = SessionState{ID: "s", Status: "running"}
	broken.steps = map[int64]any{}
	d := &callbackDispatcher{loop: &Loop{Store: broken, Tools: &callbackFakeTools{}}, specs: specs,
		spend: &Spend{}, budget: config.Budget{}, journal: &callbackJournal{}}
	result, err = d.Call(context.Background(), "write_file", map[string]any{"path": "a"})
	if err == nil || result.OK || !result.Interrupted {
		t.Fatalf("a settlement failure leaked success: %+v %v", result, err)
	}
}

func TestCallbackDeadlineIsEnforced(t *testing.T) {
	tools := &callbackFakeTools{}
	d := &callbackDispatcher{loop: callbackLoop(tools), specs: callbackSpecs(), spend: &Spend{},
		budget: config.Budget{}, journal: &callbackJournal{}}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result, err := d.Call(ctx, "read_file", map[string]any{"path": "a"})
	if err != nil || !result.Denied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("a call past its deadline ran: %v", tools.calls)
	}
}

func TestCallbackHookArgsAreRevalidated(t *testing.T) {
	registry, err := verify.NewRegistry(verify.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&lifecycleTestHook{id: "rewrite", version: 1, events: []verify.Event{verify.EventBeforeToolCall},
		mutable: true,
		run: func(context.Context, verify.Request) (verify.Result, error) {
			return verify.Result{Decision: verify.DecisionModifyAllowedFields, Args: map[string]any{"path": 42}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	tools := &callbackFakeTools{}
	d := &callbackDispatcher{loop: &Loop{Tools: tools, Hooks: registry}, specs: callbackSpecs(), spend: &Spend{},
		budget: config.Budget{}, journal: &callbackJournal{}}
	result, err := d.Call(context.Background(), "write_file", map[string]any{"path": "a"})
	if err != nil || !result.Denied {
		t.Fatalf("a hook-mutated invalid argument must be refused: %+v %v", result, err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("the tool ran with invalid hook arguments: %v", tools.calls)
	}
}

func TestCallbackRefusesToolsOutsideTheEffectiveSet(t *testing.T) {
	tools := &callbackFakeTools{}
	d := &callbackDispatcher{loop: callbackLoop(tools), specs: []protocol.ToolSpec{{Name: "read_file", Permission: "read"}},
		spend: &Spend{}, budget: config.Budget{}, journal: &callbackJournal{}}
	result, err := d.Call(context.Background(), "notebook_read", nil)
	if err != nil || !result.Denied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCallbackCountsAgainstTheRunBudget(t *testing.T) {
	tools := &callbackFakeTools{}
	spend := &Spend{ToolCalls: 2}
	d := &callbackDispatcher{loop: callbackLoop(tools), specs: callbackSpecs(), spend: spend,
		budget: config.Budget{MaxToolCalls: 2}, journal: &callbackJournal{}}
	result, err := d.Call(context.Background(), "read_file", nil)
	if err != nil || !result.Denied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("a budget-refused call ran: %v", tools.calls)
	}
}

func TestCallbackJournalIsBounded(t *testing.T) {
	j := &callbackJournal{}
	for i := range MaxCallbackJournal + 5 {
		j.add(callbackEntry{Seq: i, Tool: "read_file"})
	}
	if len(j.Entries) != MaxCallbackJournal || j.Dropped != 5 {
		t.Fatalf("journal=%d dropped=%d", len(j.Entries), j.Dropped)
	}
}

func TestToolCallbackBindsOnlyEvalTools(t *testing.T) {
	l := callbackLoop(&callbackFakeTools{})
	read := protocol.ToolCall{ID: "1", Name: "read_file", Arguments: json.RawMessage(`{}`)}
	if l.toolCallback("s", "run", "rq", 1, read, callbackSpecs(), &Spend{}, config.Budget{}, true) != nil {
		t.Fatal("a non-eval tool must not get a callback")
	}
	evalCall := protocol.ToolCall{ID: "2", Name: "eval", Arguments: json.RawMessage(`{}`)}
	if l.toolCallback("s", "run", "rq", 1, evalCall, callbackSpecs(), &Spend{}, config.Budget{}, true) == nil {
		t.Fatal("the eval tool must get a callback")
	}
}

func TestIsEvalTool(t *testing.T) {
	for _, name := range []string{"eval", "eval_create", "eval_control", "eval_artifact", "eval_runtimes", "eval_sessions"} {
		if !isEvalTool(name) {
			t.Fatalf("%s must be an eval tool", name)
		}
	}
	for _, name := range []string{"read_file", "notebook_update", "spawn_subagent"} {
		if isEvalTool(name) {
			t.Fatalf("%s must not be an eval tool", name)
		}
	}
}
