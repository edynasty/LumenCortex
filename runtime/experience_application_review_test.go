package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func reviewObservedProcedure(t *testing.T, f *cognitionFixture, actions []experienceAction) CognitiveNode {
	t.Helper()
	p := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step >= len(actions) {
			return cogFinal("observed ordered procedure"), nil
		}
		raw, err := json.Marshal(actions[step].Arguments)
		if err != nil {
			return protocol.ProviderResponse{}, err
		}
		response := cogCall(actions[step].Tool, string(raw))
		response.Message.ToolCalls[0].ID = fmt.Sprintf("observed_%d", step)
		return response, nil
	}}
	origin := expMustRun(t, f, "ordered build marker procedure", p)
	proposal := expProposal("ordered build marker procedure")
	proposal.Strategy = []string{"Repeat the observed tool sequence in order under the current run authority."}
	return expValidate(t, f, origin, expExtract(t, f, origin, proposal))
}

func TestReviewExperienceActionsObserveLiveWorkflow(t *testing.T) {
	for _, mode := range []string{"allowed", "restricted", "human_gate", "hook_denied"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, true)
			expInstallChecks(t, f)
			actions := []experienceAction{
				{Tool: "write_file", Arguments: map[string]any{"path": "build/first", "content": "one"}},
				{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}},
			}
			n := reviewObservedProcedure(t, f, actions)
			for _, path := range []string{"build/first", "build/reused"} {
				if err := os.Remove(filepath.Join(f.root, path)); err != nil {
					t.Fatal(err)
				}
			}
			consumer := expNewSession(t, f, "reuse ordered build marker procedure")
			adopted, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
			if err != nil {
				t.Fatal(err)
			}
			next := `{"terminal":true,"allowedTools":["write_file"],"outcomes":[{"when":{"tool":"write_file","ok":true},"set":{"second":true}}],"completeWhen":{"fact":"second","equals":true}}`
			if mode == "restricted" {
				next = `{"terminal":true,"allowedTools":["read_file"],"completeWhen":{"fact":"first","equals":true}}`
			}
			if mode == "human_gate" {
				next = `{"terminal":true,"allowedTools":["write_file"],"completeWhen":{"fact":"first","equals":true},"gates":[{"id":"approve-second","type":"human","fact":"approved","equals":true}]}`
			}
			workflow := fmt.Sprintf(`{"id":"application-workflow","entry":"first","facts":{"first":false,"second":false},"actions":{"first":{"allowedTools":["apply_experience","write_file"],"outcomes":[{"when":{"tool":"write_file","ok":true},"set":{"first":true}}],"completeWhen":{"fact":"first","equals":true},"routes":[{"to":"next"}]},"next":%s}}`, next)
			var before, after atomic.Int32
			var secondSawCommittedState atomic.Bool
			if err := f.engine.RegisterLifecycleHook(&platformHook{id: "application-workflow", events: []LifecycleEvent{LifecycleBeforeWorkflowTransition, LifecycleAfterWorkflowTransition, LifecycleBeforeToolCall}, fn: func(ctx context.Context, r LifecycleRequest) (LifecycleResult, error) {
				if r.SessionID != consumer {
					return LifecycleResult{Decision: HookContinue}, nil
				}
				if r.Event == LifecycleBeforeWorkflowTransition {
					before.Add(1)
					if mode == "hook_denied" {
						return LifecycleResult{Decision: HookDeny, Reason: "transition blocked"}, nil
					}
				}
				if r.Event == LifecycleAfterWorkflowTransition {
					after.Add(1)
				}
				if r.Event == LifecycleBeforeToolCall && r.ToolName == "write_file" && r.ToolArgs["path"] == "build/reused" {
					state, err := f.engine.loadTaskState(ctx, consumer)
					if err != nil {
						return LifecycleResult{}, err
					}
					wf, err := state.RestoreWorkflow()
					if err != nil {
						return LifecycleResult{}, err
					}
					secondSawCommittedState.Store(wf != nil && wf.CurrentAction == "next")
				}
				return LifecycleResult{Decision: HookContinue}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			provider := completionApplyProvider(t, n, adopted.ID, actions)
			if mode == "human_gate" {
				apply := provider.respond
				provider.respond = func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					response, err := apply(step, req)
					if step == 0 {
						response.Message.ToolCalls = append(response.Message.ToolCalls, protocol.ToolCall{ID: "after_gate", Name: "write_file", Arguments: json.RawMessage(`{"path":"build/after-gate","content":"forbidden"}`)})
					}
					return response, err
				}
			}
			result, runErr := f.engine.runAgentWith(ctx, consumer, provider, AgentOptions{MaxSteps: 3, Policy: "workspace", Workflow: json.RawMessage(workflow)})
			if data, err := os.ReadFile(filepath.Join(f.root, "build/first")); err != nil || string(data) != "one" {
				t.Fatalf("first action not observed: %q %v", data, err)
			}
			_, secondErr := os.Stat(filepath.Join(f.root, "build/reused"))
			if mode == "human_gate" {
				if _, err := os.Stat(filepath.Join(f.root, "build/after-gate")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("same-response call crossed human gate: %v", err)
				}
			}
			receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adopted.ID)
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.engine.loadTaskState(ctx, consumer)
			if err != nil {
				t.Fatal(err)
			}
			wf, err := state.RestoreWorkflow()
			if err != nil || wf == nil {
				t.Fatalf("workflow state: %+v %v", wf, err)
			}
			if mode == "allowed" {
				if runErr != nil || result.Status != "completed" || secondErr != nil || !receipt.Executed() || !secondSawCommittedState.Load() || before.Load() != 1 || after.Load() != 1 || wf.CurrentAction != "next" {
					t.Fatalf("allowed sequence lost live state: result=%+v err=%v second=%v receipt=%+v state=%+v hooks=%d/%d committed=%v", result, runErr, secondErr, receipt, wf, before.Load(), after.Load(), secondSawCommittedState.Load())
				}
			} else {
				if !errors.Is(secondErr, os.ErrNotExist) || receipt.State != "unknown" || receipt.Executed() || before.Load() != 1 {
					t.Fatalf("workflow boundary bypass: result=%+v err=%v second=%v receipt=%+v hooks=%d/%d", result, runErr, secondErr, receipt, before.Load(), after.Load())
				}
				if mode == "human_gate" && (result.Status != "waiting_gate" || len(wf.WaitingHumanGates()) != 1) {
					t.Fatalf("human gate lost: %+v %v %+v", result, runErr, wf)
				}
				if mode == "hook_denied" && (result.Status != "interrupted" || wf.CurrentAction != "first" || after.Load() != 0) {
					t.Fatalf("denied transition committed: %+v %v %+v", result, runErr, wf)
				}
				read, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if fb := read.Experience.Uses[0].Feedback; fb != nil && (fb.Used || fb.Outcome == "success") {
					t.Fatalf("incomplete sequence earned reuse credit: %+v", fb)
				}
			}
		})
	}
}

func TestReviewIdenticalExperienceActionsHaveDistinctReceipts(t *testing.T) {
	for _, secondUnknown := range []bool{false, true} {
		t.Run(fmt.Sprint(secondUnknown), func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, true)
			expInstallChecks(t, f)
			writeFile(t, filepath.Join(f.root, "build/input"), "input")
			writeFile(t, filepath.Join(f.root, "build/reused"), "pre-existing outcome")
			actions := []experienceAction{{Tool: "read_file", Arguments: map[string]any{"path": "build/input"}}, {Tool: "read_file", Arguments: map[string]any{"path": "build/input"}}}
			n := reviewObservedProcedure(t, f, actions)
			consumer := expNewSession(t, f, "reuse ordered read procedure")
			adopted, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
			if err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			if err := f.engine.RegisterLifecycleHook(&platformHook{id: "remove-second-read", events: []LifecycleEvent{LifecycleAfterToolCall}, fn: func(_ context.Context, r LifecycleRequest) (LifecycleResult, error) {
				if secondUnknown && r.SessionID == consumer && r.ToolName == "read_file" && reads.Add(1) == 1 {
					if err := os.Remove(filepath.Join(f.root, "build/input")); err != nil {
						return LifecycleResult{}, err
					}
				}
				return LifecycleResult{Decision: HookContinue}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			expMustRunSession(t, f, consumer, completionApplyProvider(t, n, adopted.ID, actions))
			receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adopted.ID)
			if err != nil || len(receipt.Actions) != 2 || receipt.Actions[0].ExecutionID == "" || receipt.Actions[1].ExecutionID == "" || receipt.Actions[0].ExecutionID == receipt.Actions[1].ExecutionID {
				t.Fatalf("invocations borrowed execution identity: %+v %v", receipt, err)
			}
			second, err := f.engine.store.Execution(ctx, receipt.Actions[1].ExecutionID)
			if err != nil {
				t.Fatal(err)
			}
			read, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			fb := read.Experience.Uses[0].Feedback
			if secondUnknown {
				if receipt.State != "unknown" || receipt.Executed() || second.Outcome != session.OutcomeUnknown || fb == nil || fb.Used || fb.Outcome != "unknown" || read.Experience.State != knowledge.ExperienceValidated {
					t.Fatalf("unknown action received success/counterexample credit: receipt=%+v second=%+v feedback=%+v state=%s", receipt, second, fb, read.Experience.State)
				}
			} else if receipt.State != "completed" || !receipt.Executed() || second.Outcome != session.OutcomeComplete || fb == nil || !fb.Used || fb.Outcome != "success" {
				t.Fatalf("distinct successful actions lost credit: %+v %+v %+v", receipt, second, fb)
			}
		})
	}
}

func TestReviewNestedThreadWarningRetainsWithdrawal(t *testing.T) {
	for _, op := range []string{"revoke", "delete"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, true)
			x, _ := expConditionalValidated(t, f, "counterexample warning provenance")
			thread := expNewSession(t, f, "counterexample warning provenance")
			adopted, err := f.engine.AdoptExperience(ctx, thread, x.ID, x.Version)
			if err != nil {
				t.Fatal(err)
			}
			actions := []experienceAction{{Tool: "read_thread", Arguments: map[string]any{"id": thread}}}
			y := reviewObservedProcedure(t, f, actions)
			reader := expNewSession(t, f, "reuse ordered thread read procedure")
			use, err := f.engine.AdoptExperience(ctx, reader, y.ID, y.Version)
			if err != nil {
				t.Fatal(err)
			}
			reporter := expNewSession(t, f, "private counterexample reporter")
			reportUse, err := f.engine.AdoptExperience(ctx, reporter, x.ID, adopted.Version)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := expApplyCounterexample(t, f, reporter, x, reportUse.ID); err != nil || result.Status != "completed" {
				t.Fatalf("reporter: %+v %v", result, err)
			}
			writeFile(t, filepath.Join(f.root, "build/reused"), "pre-existing outcome")
			p := completionApplyProvider(t, y, use.ID, actions)
			expMustRunSession(t, f, reader, p)
			var outer struct {
				Results []protocol.ToolResult `json:"results"`
				Sources []string              `json:"sourceSessions"`
			}
			content := cogToolResult(t, p.request(t, 1))
			if err := json.Unmarshal([]byte(content), &outer); err != nil || len(outer.Results) != 1 || !strings.Contains(outer.Results[0].Content, "contested") || !slices.Contains(outer.Sources, x.SessionID) || strings.Contains(content, reporter) {
				t.Fatalf("nested warning lost lineage or exposed private reporter: %s %v", content, err)
			}
			if op == "revoke" {
				rev, err := f.engine.RevokeSource(ctx, SourceSession, reporter, "privacy_review")
				if err != nil || !slices.Contains(rev.Sessions, reader) {
					t.Fatalf("nested warning recipient not reached: %+v %v", rev, err)
				}
			} else if _, err := f.engine.DeleteSession(ctx, reporter, WorktreeCleanup{}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.GetContextGraph(ctx, reader); !errors.Is(err, session.ErrFenced) {
				t.Fatalf("nested warning recipient remained readable: %v", err)
			}
		})
	}
}

func TestReviewExperienceShellMustFinishInForeground(t *testing.T) {
	for _, mode := range []string{"", "foreground", "background"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, true)
			expInstallChecks(t, f)
			writeFile(t, filepath.Join(f.root, "build/reused"), "ready")
			args := map[string]any{"command": "printf observed"}
			if mode != "" {
				args["mode"] = mode
			}
			origin := expNewSession(t, f, "observed shell procedure")
			p := &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if step == 0 {
					raw, _ := json.Marshal(args)
					return cogCall("bash", string(raw)), nil
				}
				if step == 1 && mode == "background" {
					var job struct {
						ID string `json:"jobId"`
					}
					if err := json.Unmarshal([]byte(cogToolResult(t, req)), &job); err != nil || job.ID == "" {
						return protocol.ProviderResponse{}, fmt.Errorf("origin job: %+v %v", job, err)
					}
					if _, done, err := f.engine.jobs.Wait(ctx, []string{job.ID}, 10*time.Second); err != nil || !done {
						return protocol.ProviderResponse{}, fmt.Errorf("origin job not settled: done=%v %v", done, err)
					}
				}
				return cogFinal("observed shell completion"), nil
			}}
			if result, err := f.engine.runAgentWith(ctx, origin, p, AgentOptions{MaxSteps: 4, Policy: "full"}); err != nil || result.Status != "completed" {
				t.Fatalf("origin: %+v %v", result, err)
			}
			n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("observed foreground shell procedure")))
			consumer := expNewSession(t, f, "reuse foreground shell procedure")
			adopted, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adopted.ID, []experienceAction{{Tool: "bash", Arguments: args}}), AgentOptions{MaxSteps: 3, Policy: "full"})
			receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adopted.ID)
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := f.engine.Jobs(ctx, JobQuery{SessionID: consumer})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "background" {
				if len(jobs) != 0 || receipt.State != "unknown" || receipt.Executed() || receipt.Actions[0].ExecutionID != "" {
					t.Fatalf("background submission passed for completed execution: jobs=%+v receipt=%+v", jobs, receipt)
				}
				read, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if fb := read.Experience.Uses[0].Feedback; fb != nil && (fb.Used || fb.Outcome == "success") {
					t.Fatalf("background submission received reuse credit: %+v", fb)
				}
			} else if len(jobs) != 0 || receipt.State != "completed" || !receipt.Executed() {
				t.Fatalf("foreground execution lost: jobs=%+v receipt=%+v", jobs, receipt)
			}
		})
	}
}
