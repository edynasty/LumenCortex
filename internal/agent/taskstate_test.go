package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

func taskBlock(req protocol.ProviderRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, taskstate.BlockHeader) {
			return m.Content
		}
	}
	return ""
}

// ADR-0006 D1′: an edit committed while step 1's request is in flight is not in that request, is in
// step 2's, and is consumed in one version with reason "user".
func TestInboxEditVisibleAtTheNextStep(t *testing.T) {
	store := newState("inbox")
	brief := "fix the flaky retry test only"
	provider := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch n {
		case 1:
			if strings.Contains(taskBlock(req), brief) {
				t.Error("step 1 saw an edit queued after its request was built")
			}
			store.queue(taskstate.Edit{Brief: &brief})
			return shellCall("c1"), nil
		default:
			if !strings.Contains(taskBlock(req), brief) {
				t.Errorf("step %d did not see the queued brief:\n%s", n, taskBlock(req))
			}
			return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
		}
	}}
	if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "inbox", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if store.ts.Brief != brief || len(store.inbox) != 0 {
		t.Fatalf("brief=%q inbox=%d, want the edit applied and consumed", store.ts.Brief, len(store.inbox))
	}
	if !slices.Contains(store.reasons, taskstate.ReasonUser) {
		t.Fatalf("reasons=%v, want a user version", store.reasons)
	}
}

// The agent may only suggest: it cannot re-suggest or override a target the user excluded.
func TestAgentCannotOverrideUserExclusion(t *testing.T) {
	store := newState("refs")
	store.queue(taskstate.Edit{Reference: &taskstate.ReferenceEdit{Kind: taskstate.RefPath, Target: "vendor/big.go", State: taskstate.RefExcluded}})
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			call("s1", "reference_suggest", `{"kind":"path","target":"./vendor/big.go","note":"looks relevant"}`),
			call("s2", "reference_suggest", `{"kind":"path","target":"internal/retry.go"}`),
		}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "refs", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]taskstate.Reference{}
	for _, r := range store.ts.References {
		byTarget[r.Target] = r
	}
	if r := byTarget["vendor/big.go"]; r.State != taskstate.RefExcluded || r.By != taskstate.ByUser {
		t.Fatalf("excluded reference = %+v, want excluded by user", r)
	}
	if r := byTarget["internal/retry.go"]; r.State != taskstate.RefSuggested || r.By != taskstate.ByAgent {
		t.Fatalf("new reference = %+v, want suggested by agent", r)
	}
	for _, m := range store.messages {
		if m.ToolCallID == "s1" && !strings.Contains(m.Content, "excluded") {
			t.Fatalf("re-suggesting an excluded target must fail: %s", m.Content)
		}
	}
}

// A Task State write that fails ends the run failed with a storage cause, recorded on the row.
func TestTaskStateWriteFailureEndsTheRunFailed(t *testing.T) {
	store := newState("put")
	store.putErr = errors.New("disk full")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "shell", `{"command":"test-fail"}`)}}},
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "put", Options{Session: testRef, MaxSteps: 3})
	if err == nil {
		t.Fatal("want the storage error")
	}
	assertExit(t, store, result, StatusFailed, KindStorage)
}

// ADR-0006 D2: a successful mutating tool reopens a thread the user marked done.
func TestMutationReopensADoneThread(t *testing.T) {
	store := newState("reopen")
	store.queue(taskstate.Edit{Resolve: &taskstate.ResolveEdit{State: taskstate.ResolutionDone, Actor: "user"}})
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "nothing to do"}},
	}}
	if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "reopen", Options{Session: testRef, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if store.ts.Resolution.State != taskstate.ResolutionDone {
		t.Fatalf("a question-only run must not reopen: %+v", store.ts.Resolution)
	}
	store.state.Status = StatusRunning
	provider.responses = []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("p1", "apply_patch", `{}`)}}},
		{Message: protocol.Message{Content: "patched"}},
	}
	if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "reopen", Options{Session: testRef, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if r := store.ts.Resolution; r.State != taskstate.ResolutionOpen || r.By != taskstate.ByReopen {
		t.Fatalf("resolution = %+v, want open by reopen", r)
	}
}

const finishableWorkflow = `{"id":"wf","facts":{"passed":false},"actions":{"verify":{"terminal":true,"allowedTools":["shell"],` +
	`"outcomes":[{"when":{"result":"exitCode","equals":0},"set":{"passed":true}}],"completeWhen":{"fact":"passed","equals":true}}}}`

// Auto-done latches only on positive evidence with no obligation left.
func TestAutoDoneNeedsEvidenceAndNoObligation(t *testing.T) {
	run := func(t *testing.T, workflow string, units string, pending bool, script []protocol.ProviderResponse) taskstate.Resolution {
		t.Helper()
		store := newState("auto")
		var wf, wu []byte
		if workflow != "" {
			wf = []byte(workflow)
		}
		if units != "" {
			wu = []byte(units)
		}
		store.seed(t, wf, wu)
		loop := &Loop{Models: testModels(&scriptedProvider{responses: script}), Store: store, Tools: fakeTools{},
			Evidence: func(context.Context) (taskstate.Evidence, error) {
				return taskstate.Evidence{PendingApplication: pending, Refs: []string{"head:abc"}}, nil
			}}
		if _, err := loop.Run(context.Background(), "auto", Options{Session: testRef, MaxSteps: 4}); err != nil {
			t.Fatal(err)
		}
		return store.ts.Resolution
	}
	pass := []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "shell", `{"command":"ok"}`)}}},
		{Message: protocol.Message{Content: "verified"}},
	}

	t.Run("workflow that can finish latches done", func(t *testing.T) {
		r := run(t, finishableWorkflow, "", false, pass)
		if r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence || !slices.Contains(r.Refs, "head:abc") {
			t.Fatalf("resolution = %+v", r)
		}
	})
	t.Run("pending application keeps it open", func(t *testing.T) {
		if r := run(t, finishableWorkflow, "", true, pass); r.State != taskstate.ResolutionOpen {
			t.Fatalf("resolution = %+v", r)
		}
	})
	t.Run("no workflow is no evidence", func(t *testing.T) {
		if r := run(t, "", "", false, pass); r.State != taskstate.ResolutionOpen {
			t.Fatalf("resolution = %+v", r)
		}
	})
	t.Run("a cancelled unit is not acceptance", func(t *testing.T) {
		script := []protocol.ProviderResponse{
			{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("c1", "shell", `{"command":"ok"}`),
				call("u1", "work_unit_update", `{"id":"extra","status":"cancelled"}`),
			}}},
			{Message: protocol.Message{Content: "verified"}},
		}
		// The workflow's tool boundary also bounds the task tools, so it admits work_unit_update.
		workflow := strings.Replace(finishableWorkflow, `"allowedTools":["shell"]`, `"allowedTools":["shell","work_unit_update"]`, 1)
		r := run(t, workflow, `{"workUnits":[{"id":"extra","goal":"optional cleanup"}]}`, false, script)
		if r.State != taskstate.ResolutionOpen || !slices.Contains(r.CancelledUnits, "extra") {
			t.Fatalf("resolution = %+v", r)
		}
	})
}

// Design §4.5: effort escalates while Task State progress records a stall and returns to the default
// once a completed unit resets it.
func TestResetStallEndsEscalation(t *testing.T) {
	store := newState("effort")
	store.seed(t, nil, []byte(`{"workUnits":[{"id":"fix","goal":"fix it"}]}`))
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "shell", `{"command":"test-fail"}`)}}},
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c2", "shell", `{"command":"test-fail"}`)}}},
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("u1", "work_unit_update", `{"id":"fix","status":"completed"}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := &Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	opts := Options{Session: testRef, MaxSteps: 5, EffortDefault: config.EffortLow,
		Escalation: &config.Escalation{SameFailure: 2, NoProgressSteps: 0, Max: config.EffortHigh}}
	if _, err := loop.Run(context.Background(), "effort", opts); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, req := range provider.seen {
		got = append(got, req.ReasoningEffort)
	}
	want := []string{"low", "low", "medium", "low"}
	if !slices.Equal(got, want) {
		t.Fatalf("efforts = %v, want %v", got, want)
	}
}
