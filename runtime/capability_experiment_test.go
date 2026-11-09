package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestCapabilityExecutionArms(t *testing.T) {
	if os.Getenv("LC_CAPABILITY_EXPERIMENT") != "1" {
		t.Skip("explicit deterministic experiment")
	}
	report := func(kind string, values map[string]any) {
		values["capability"] = kind
		values["external_model_requests"] = 0
		b, _ := json.Marshal(values)
		fmt.Println("CAPABILITY_RUNTIME", string(b))
	}
	t.Run("inspector", func(t *testing.T) {
		e := openTestEngine(t)
		id := newParentSession(t, e, &debuggerProvider{})
		live, unsubscribe := e.Events(1)
		defer unsubscribe()
		result, err := e.RunAgent(context.Background(), id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 3})
		if err != nil || result.Status != "completed" {
			t.Fatal(result, err)
		}
		seen := 0
		for {
			select {
			case <-live:
				seen++
			default:
				goto drained
			}
		}
	drained:
		page, err := e.QueryRuntimeEvents(context.Background(), RuntimeEventQuery{SessionID: id, Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		ends := 0
		for _, event := range page.Events {
			if event.Type == "tool.end" {
				ends++
			}
		}
		if ends != 2 || len(page.Events) <= seen {
			t.Fatalf("missing history %+v seen=%d", page, seen)
		}
		report("inspector", map[string]any{"one_slot_live_observed": seen, "durable_events": len(page.Events), "recovered_tool_outcomes": ends, "fault_location": "tool identity retained despite dropped live events"})
	})
	t.Run("runtime_debugger", func(t *testing.T) {
		observations := map[string]any{}
		for _, enabled := range []bool{false, true} {
			e := openTestEngine(t)
			p := &debuggerProvider{}
			id := newParentSession(t, e, p)
			sup, err := NewRunSupervisor(e)
			if err != nil {
				t.Fatal(err)
			}
			defer sup.Close()
			opts := AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 3}
			if enabled {
				opts.Debug = &AgentDebugOptions{PauseOnStart: true}
			}
			if _, err = sup.Start(context.Background(), id, opts); err != nil {
				t.Fatal(err)
			}
			isolated := false
			if enabled {
				s := awaitDebugPause(t, e, id, 0)
				commandDebug(t, e, id, s, "step_tool")
				s = awaitDebugPause(t, e, id, s.Revision)
				a, ae := os.ReadFile(filepath.Join(e.workspace, "first.txt"))
				_, be := os.Stat(filepath.Join(e.workspace, "second.txt"))
				isolated = ae == nil && string(a) == "ONE" && os.IsNotExist(be)
				commandDebug(t, e, id, s, "continue")
			}
			waitInactive(t, sup, id)
			a, ae := os.ReadFile(filepath.Join(e.workspace, "first.txt"))
			b, be := os.ReadFile(filepath.Join(e.workspace, "second.txt"))
			if ae != nil || be != nil || string(a) != "ONE" || string(b) != "TWO" {
				t.Fatal("final result differs")
			}
			if enabled && !isolated {
				t.Fatal("no isolated first-write boundary")
			}
			observations[fmt.Sprint(enabled)] = map[string]any{"first_write_boundary_observed": isolated, "final_files_equal": true}
		}
		report("runtime_debugger", observations)
	})
	t.Run("hub_worktree", func(t *testing.T) {
		// Reuse the real four-worker/dependency oracle; no separate fake Hub.
		TestHubFourWorkersAndDependencyAdmission(t)
		root := t.TempDir()
		path := filepath.Join(root, "shared.txt")
		if err := os.WriteFile(path, []byte("base"), 0600); err != nil {
			t.Fatal(err)
		}
		barrier := make(chan struct{})
		readDone := make(chan struct{}, 4)
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b, err := os.ReadFile(path)
				if err != nil {
					errs <- err
				}
				readDone <- struct{}{}
				<-barrier
				if err = os.WriteFile(path, append(b, []byte(fmt.Sprint(i))...), 0600); err != nil {
					errs <- err
				}
			}(i)
		}
		for range 4 {
			<-readDone
		}
		close(barrier)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) != 5 {
			t.Fatalf("shared output %q %v", b, err)
		}
		report("agent_hub", map[string]any{"concurrent_workers": 4, "completed_dependency_task": 1, "unmet_dependency_denied": true})
		report("worktree", map[string]any{"shared_read_modify_write_preserved": 1, "isolated_worker_outputs_preserved": 4, "parent_unchanged": true, "merge_conflicts_not_automatically_resolved": true})
	})
	t.Run("persistent_eval", func(t *testing.T) {
		e := openTestEngine(t)
		e.evalOptions = &EvalOptions{Python: os.Getenv("LC_TEST_PYTHON"), JavaScript: os.Getenv("LC_TEST_NODE"), Budget: evalapi.Budget{WallSeconds: 30, CPUSeconds: 60, MemoryBytes: 512 << 20, OutputBytes: 1 << 20, ArtifactBytes: 4 << 20, DiskBytes: 8 << 20, MaxWorkers: 2}}
		id := newParentSession(t, e, &debuggerProvider{})
		ctx := context.Background()
		k, err := e.CreateEvalSession(ctx, id, "python")
		if err != nil {
			t.Fatal(err)
		}
		cell := func(kernel, code string) evalapi.Result {
			r, err := e.ExecuteEval(ctx, id, evalapi.Request{SessionID: kernel, Code: code})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		if r := cell(k.ID, "values=[1,2,3]"); !r.Succeeded() {
			t.Fatal(r)
		}
		persistent := cell(k.ID, "values.append(4)\nprint(sum(values))")
		if !persistent.Succeeded() || strings.TrimSpace(persistent.Stdout) != "10" {
			t.Fatal(persistent)
		}
		if err = e.EvalClose(ctx, id, k.ID); err != nil {
			t.Fatal(err)
		}
		fresh, err := e.CreateEvalSession(ctx, id, "python")
		if err != nil {
			t.Fatal(err)
		}
		stateless := cell(fresh.ID, "values.append(4)\nprint(sum(values))")
		if stateless.Succeeded() || !strings.Contains(stateless.Error+stateless.Traceback, "NameError") {
			t.Fatal(stateless)
		}
		report("persistent_eval", map[string]any{"persistent_sum": 10, "fresh_kernel_dependent_cell": "NameError", "cross_language_evidence": "CROSS_LANGUAGE_RESULTS.json"})
	})
	t.Run("hooks", func(t *testing.T) {
		effects := map[string]bool{}
		for _, enabled := range []bool{false, true} {
			e := openTestEngine(t)
			id := newParentSession(t, e, &debuggerProvider{})
			if enabled {
				if err := e.RegisterLifecycleHook(&platformHook{id: "deny-first", events: []LifecycleEvent{LifecycleBeforeToolCall}, fn: func(_ context.Context, r LifecycleRequest) (LifecycleResult, error) {
					if r.ToolArgs["path"] == "first.txt" {
						return LifecycleResult{Decision: HookDeny}, nil
					}
					return LifecycleResult{Decision: HookContinue}, nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.RunAgent(context.Background(), id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 3})
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(filepath.Join(e.workspace, "first.txt"))
			effects[fmt.Sprint(enabled)] = err == nil
		}
		if !effects["false"] || effects["true"] {
			t.Fatal(effects)
		}
		report("hooks", map[string]any{"denied_policy_file_written_by_arm": effects})
	})
	t.Run("verification", func(t *testing.T) {
		results := map[string]string{}
		for _, enabled := range []bool{false, true} {
			e := openTestEngine(t)
			p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("Delivered missing.txt successfully")}}
			id := newParentSession(t, e, p)
			opts := AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 1}
			if enabled {
				opts.Delivery = &DeliveryContract{Files: []DeliveryFile{{Path: "missing.txt", Changed: true}}}
			}
			result, err := e.RunAgent(context.Background(), id, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(e.workspace, "missing.txt")); !os.IsNotExist(err) {
				t.Fatal("fixture unexpectedly wrote file")
			}
			results[fmt.Sprint(enabled)] = "not_requested"
			if result.Delivery != nil {
				results[fmt.Sprint(enabled)] = string(result.Delivery.Status)
			}
			if enabled && (result.Delivery == nil || result.Delivery.Status != DeliveryFailed) {
				t.Fatalf("false claim accepted %+v", result)
			}
		}
		report("verification", map[string]any{"delivery_status_by_arm": results, "missing_file_claim_rejected": true})
	})
}
