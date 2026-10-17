package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// C1 (ADR-0006 D3) at the loop: what a text answer does in an interactive run, and what it never
// changes. Every case runs the real loop against the in-memory store.

const openUnits = `{"workUnits":[{"id":"fix","goal":"fix the transaction bug","checks":["go-test"]},{"id":"docs","goal":"update the docs"}]}`

func answer(text string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{Content: text}, FinishReason: "stop"}
}

func toolStep(calls ...protocol.ToolCall) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: calls}, FinishReason: "tool_calls"}
}

func blockedRows(s *memoryStore) []string {
	var out []string
	for _, m := range s.messages {
		if m.Origin == protocol.OriginCompletionBlocked {
			out = append(out, m.Content)
		}
	}
	return out
}

func eventTypes(events *[]string) func(Event) {
	return func(e Event) { *events = append(*events, e.Type) }
}

// A conversation turn with open work units and an unpassed required check: the answer ends the run
// completed, with no correction row and no check run; the units and the open resolution are kept. The
// same state in a non-interactive run is today's gate: the answer is blocked and the run continues.
func TestInteractiveTurnEndsWithOpenUnits(t *testing.T) {
	setup := func(t *testing.T) (*memoryStore, *fakeChecks, *[]string, *Loop, *scriptedProvider) {
		store := newState("conv")
		store.seed(t, nil, []byte(openUnits))
		checks := &fakeChecks{all: []string{"go-test"}, required: []string{"go-test"},
			queue: map[string][]string{"go-test": {evaluator.StatusFailed, evaluator.StatusFailed}}}
		var events []string
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{
			answer("The transaction bug comes from the missing rollback."),
			answer("Still the missing rollback."),
		}}
		loop := &Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Checks: checks, Emit: eventTypes(&events),
			// No stored result satisfies go-test at the current fingerprint.
			Evidence: func(context.Context) (taskstate.Evidence, error) {
				return taskstate.Evidence{Checks: []taskstate.CheckEvidence{{ID: "go-test"}}}, nil
			}}
		return store, checks, &events, loop, provider
	}

	t.Run("interactive", func(t *testing.T) {
		store, checks, events, loop, provider := setup(t)
		result, err := loop.Run(context.Background(), "conv", Options{Session: testRef, MaxSteps: 4, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusCompleted || result.Final != "The transaction bug comes from the missing rollback." {
			t.Fatalf("result = %+v", result)
		}
		if store.state.Status != StatusCompleted || store.cause != nil {
			t.Fatalf("stored status %q cause %v", store.state.Status, store.cause)
		}
		if len(provider.seen) != 1 {
			t.Fatalf("the answer was followed by %d more requests", len(provider.seen)-1)
		}
		if rows := blockedRows(store); len(rows) != 0 {
			t.Fatalf("an interactive answer appended correction rows: %q", rows)
		}
		if len(checks.calls) != 0 {
			t.Fatalf("ending a turn decided checks: %v", checks.calls)
		}
		for _, kind := range []string{"work_unit.blocked_final", "checks.blocked_final", "workflow.blocked_final"} {
			if slices.Contains(*events, kind) {
				t.Fatalf("event %s at an interactive turn end", kind)
			}
		}
		units := store.ts.Units()
		if got := units.Incomplete(); len(got) != 2 {
			t.Fatalf("open units after the turn: %v, want fix and docs", got)
		}
		for _, u := range units.List() {
			if u.Status == taskstate.WorkCompleted || u.Status == taskstate.WorkCancelled || len(u.CheckResults) != 0 {
				t.Fatalf("unit %s changed by the turn: %+v", u.ID, u)
			}
		}
		if r := store.ts.Resolution; r.State != taskstate.ResolutionOpen || r.By != "" {
			t.Fatalf("resolution = %+v, want open and untouched", r)
		}
		ev := taskstate.Evidence{Checks: []taskstate.CheckEvidence{{ID: "go-test"}}}
		if got := store.ts.Obligations(ev); !slices.Equal(got, []string{taskstate.ObligationWorkUnits, taskstate.ObligationChecks}) {
			t.Fatalf("obligations = %v, want work units and checks kept", got)
		}
	})

	t.Run("non-interactive keeps the gate", func(t *testing.T) {
		store, _, events, loop, provider := setup(t)
		result, _ := loop.Run(context.Background(), "conv", Options{Session: testRef, MaxSteps: 2})
		if result.Status != StatusInterrupted || store.cause["kind"] != KindMaxSteps {
			t.Fatalf("result = %+v cause %v, want interrupted/max_steps", result, store.cause)
		}
		rows := blockedRows(store)
		if len(rows) != 2 || !strings.Contains(rows[0], "fix") {
			t.Fatalf("correction rows = %q, want one per blocked answer naming the open units", rows)
		}
		if len(provider.seen) != 2 || !slices.Contains(*events, "work_unit.blocked_final") {
			t.Fatalf("requests %d, events %v", len(provider.seen), *events)
		}
	})
}

// An active host workflow keeps the whole completion gate in an interactive run: CanFinish
// continuation, the tool boundary, the work-unit gate and human approval gates.
func TestInteractiveRunKeepsTheWorkflowGate(t *testing.T) {
	t.Run("a workflow that cannot finish continues, inside its tool boundary", func(t *testing.T) {
		store := newState("wf")
		store.seed(t, []byte(finishableWorkflow), nil)
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("p1", "apply_patch", `{}`)),
			answer("done too early"),
			toolStep(call("s1", "shell", `{"command":"test-pass"}`)),
			answer("verified"),
		}}
		var events []string
		result, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Emit: eventTypes(&events)}).
			Run(context.Background(), "wf", Options{Session: testRef, MaxSteps: 6, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusCompleted || result.Final != "verified" || len(provider.seen) != 4 {
			t.Fatalf("result = %+v after %d requests", result, len(provider.seen))
		}
		rows := blockedRows(store)
		if len(rows) != 1 || !strings.HasPrefix(rows[0], "Workflow contract rejected completion") || !slices.Contains(events, "workflow.blocked_final") {
			t.Fatalf("correction rows %q events %v", rows, events)
		}
		if tool := store.messages[2]; tool.Role != "tool" || !strings.Contains(tool.Content, "not allowed") {
			t.Fatalf("apply_patch outside the workflow's tools: %+v", tool)
		}
		if store.ts.Resolution.State != taskstate.ResolutionDone || store.ts.Resolution.By != taskstate.ByEvidence {
			t.Fatalf("a workflow that finished is the evidence: %+v", store.ts.Resolution)
		}
	})

	t.Run("a workflow that can finish keeps the work-unit gate", func(t *testing.T) {
		store := newState("wf-units")
		workflow := strings.Replace(finishableWorkflow, `"allowedTools":["shell"]`, `"allowedTools":["shell","work_unit_update"]`, 1)
		store.seed(t, []byte(workflow), []byte(`{"workUnits":[{"id":"u","goal":"g"}]}`))
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("s1", "shell", `{"command":"test-pass"}`)),
			answer("done"),
			toolStep(call("u1", "work_unit_update", `{"id":"u","status":"completed"}`)),
			answer("done now"),
		}}
		result, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).
			Run(context.Background(), "wf-units", Options{Session: testRef, MaxSteps: 6, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		rows := blockedRows(store)
		if result.Status != StatusCompleted || result.Final != "done now" || len(rows) != 1 || strings.HasPrefix(rows[0], "Workflow") {
			t.Fatalf("result = %+v, correction rows %q: want one work-unit correction", result, rows)
		}
	})

	t.Run("a human gate stops the run", func(t *testing.T) {
		store := newState("gate")
		store.seed(t, []byte(`{"id":"approval","facts":{"ready":true},"actions":{"done":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},"gates":[{"id":"ship","type":"human","fact":"approved","equals":true}]}}}`), nil)
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{answer("shipping")}}
		result, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).
			Run(context.Background(), "gate", Options{Session: testRef, MaxSteps: 3, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusWaitingGate || store.state.Status != StatusWaitingGate {
			t.Fatalf("result = %+v, stored %q", result, store.state.Status)
		}
		if store.ts.Resolution.State != taskstate.ResolutionOpen {
			t.Fatalf("a pending gate resolved the thread: %+v", store.ts.Resolution)
		}
	})
}

// What a conversation turn's end may do to the resolution: keep a user's done or dropped through a
// question with an inspection tool, accept nothing without stored evidence, and latch only on a stored
// result, such as a human acceptance, that satisfies every required check.
func TestInteractiveTurnResolution(t *testing.T) {
	inspect := []protocol.ProviderResponse{toolStep(call("s1", "shell", `{"command":"git log"}`)), answer("It was merged on Monday.")}

	for _, state := range []string{taskstate.ResolutionDone, taskstate.ResolutionDropped} {
		t.Run("a question keeps the user's "+state, func(t *testing.T) {
			store := newState("q")
			store.queue(taskstate.Edit{Resolve: &taskstate.ResolveEdit{State: state, Actor: "user"}})
			if _, err := (&Loop{Models: testModels(&scriptedProvider{responses: slices.Clone(inspect)}), Store: store, Tools: fakeTools{}}).
				Run(context.Background(), "q", Options{Session: testRef, MaxSteps: 3, Interactive: true}); err != nil {
				t.Fatal(err)
			}
			if r := store.ts.Resolution; r.State != state || r.By != taskstate.ByUser {
				t.Fatalf("resolution = %+v, want the user's %s", r, state)
			}
			if store.ts.WorkUnits != nil && len(store.ts.Units().List()) > 0 {
				t.Fatalf("a question created work: %+v", store.ts.Units().List())
			}
		})
	}

	run := func(t *testing.T, ev taskstate.Evidence) (taskstate.Resolution, *fakeChecks) {
		store := newState("accept")
		// As the SQLite store decodes a thread no version has resolved yet.
		store.ts.Resolution.State = taskstate.ResolutionOpen
		checks := &fakeChecks{all: []string{"go-test"}, required: []string{"go-test"}}
		loop := &Loop{Models: testModels(&scriptedProvider{responses: slices.Clone(inspect)}), Store: store, Tools: fakeTools{}, Checks: checks,
			Evidence: func(context.Context) (taskstate.Evidence, error) { return ev, nil }}
		result, err := loop.Run(context.Background(), "accept", Options{Session: testRef, MaxSteps: 3, Interactive: true})
		if err != nil || result.Status != StatusCompleted {
			t.Fatalf("result %+v err %v", result, err)
		}
		return store.ts.Resolution, checks
	}
	t.Run("an answer is not acceptance", func(t *testing.T) {
		r, checks := run(t, taskstate.Evidence{Checks: []taskstate.CheckEvidence{{ID: "go-test"}}, Refs: []string{"head:abc"}})
		if r.State != taskstate.ResolutionOpen || r.By != "" || len(checks.calls) != 0 {
			t.Fatalf("resolution = %+v, checks decided %v", r, checks.calls)
		}
	})
	t.Run("no checks and no workflow is not acceptance", func(t *testing.T) {
		if r, _ := run(t, taskstate.Evidence{Refs: []string{"head:abc"}}); r.State != taskstate.ResolutionOpen {
			t.Fatalf("resolution = %+v", r)
		}
	})
	t.Run("a stored acceptance latches at the turn end", func(t *testing.T) {
		r, checks := run(t, taskstate.Evidence{Checks: []taskstate.CheckEvidence{{ID: "go-test", Satisfied: true, Ref: "check:go-test@eval:7"}}, Refs: []string{"head:abc"}})
		if r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence || !slices.Contains(r.Refs, "check:go-test@eval:7") || len(checks.calls) != 0 {
			t.Fatalf("resolution = %+v, checks decided %v", r, checks.calls)
		}
	})
}

// A conversation turn that changed the workspace has its required checks decided once at its end,
// without a correction: a failure keeps the thread open, a pass is the evidence the rule latches on.
// The fake evidence is satisfied only by a decision that passed, so a latch can only come from it.
func TestInteractiveTurnThatChangedTheWorkspaceDecidesChecks(t *testing.T) {
	for _, c := range []struct {
		status string
		want   string
	}{{evaluator.StatusFailed, taskstate.ResolutionOpen}, {evaluator.StatusPassed, taskstate.ResolutionDone}} {
		t.Run(c.status, func(t *testing.T) {
			store := newState("edit")
			store.ts.Resolution.State = taskstate.ResolutionOpen
			checks := &fakeChecks{all: []string{"go-test"}, required: []string{"go-test"}, queue: map[string][]string{"go-test": {c.status}}}
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(call("p1", "apply_patch", `{}`)), answer("Patched the rollback.")}}
			loop := &Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Checks: checks,
				Evidence: func(context.Context) (taskstate.Evidence, error) {
					passed := len(checks.calls) == 1 && c.status == evaluator.StatusPassed
					return taskstate.Evidence{Checks: []taskstate.CheckEvidence{{ID: "go-test", Satisfied: passed, Ref: "check:go-test@eval:1"}}}, nil
				}}
			result, err := loop.Run(context.Background(), "edit", Options{Session: testRef, MaxSteps: 4, Interactive: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusCompleted || len(provider.seen) != 2 || len(blockedRows(store)) != 0 {
				t.Fatalf("result %+v after %d requests, correction rows %q", result, len(provider.seen), blockedRows(store))
			}
			if !slices.Equal(checks.calls, []string{":go-test"}) {
				t.Fatalf("checks decided %v, want go-test once", checks.calls)
			}
			if r := store.ts.Resolution; r.State != c.want {
				t.Fatalf("resolution = %+v, want %s", r, c.want)
			}
		})
	}
}

// The run's purpose reaches the model: a conversation turn is not told to work until the outcome is
// implemented, a task run is.
func TestSystemPromptFollowsTheRunPurpose(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		provider := &scriptedProvider{responses: []protocol.ProviderResponse{answer("ok")}}
		if _, err := (&Loop{Models: testModels(provider), Store: newState("p"), Tools: fakeTools{}}).
			Run(context.Background(), "p", Options{Session: testRef, MaxSteps: 1, Interactive: interactive}); err != nil {
			t.Fatal(err)
		}
		system := provider.seen[0].Messages[0]
		task := strings.Contains(system.Content, "Use tools until the requested outcome is achieved and verified")
		turn := strings.Contains(system.Content, "Your reply ends this turn, not the task")
		if system.Role != "system" || task == interactive || turn != interactive {
			t.Fatalf("interactive=%v system prompt: %q", interactive, system.Content)
		}
	}
}
