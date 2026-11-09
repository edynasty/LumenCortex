package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestDeliveryNoGitFileLifecycle(t *testing.T) {
	for _, change := range []string{"modified", "created", "deleted"} {
		t.Run(change, func(t *testing.T) {
			// A PATH with bash only models a task image that never supplied Git.
			bin := t.TempDir()
			bash, err := exec.LookPath("bash")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(bash, filepath.Join(bin, "bash")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			if _, err := exec.LookPath("git"); err == nil {
				t.Fatal("Git unexpectedly available")
			}
			engine, id := notebookEngine(t)
			file := filepath.Join(engine.workspace, "result.txt")
			if change != "created" {
				if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			call := toolCallResponse("edit", "write_file", `{"path":"result.txt","content":"after"}`)
			if change == "deleted" {
				// Absolute rm is an Agent tool, not a verifier dependency.
				rm, err := os.Stat("/bin/rm")
				if err != nil || !rm.Mode().IsRegular() {
					t.Skip("requires /bin/rm")
				}
				call = toolCallResponse("delete", "bash", `{"command":"/bin/rm result.txt"}`)
			}
			p := &notebookProvider{responses: []protocol.ProviderResponse{call, finalResponse("done")}}
			result, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Policy: "full", Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt", Changed: true, Absent: change == "deleted"}}}})
			if err != nil || result.Status != "completed" || result.Delivery == nil || result.Delivery.Status != DeliveryVerified {
				t.Fatalf("%s: result=%+v delivery=%+v err=%v", change, result, result.Delivery, err)
			}
			if result.Delivery.Files[0].Change != change || len(p.requests) != 2 {
				t.Fatalf("evidence=%+v requests=%d", result.Delivery, len(p.requests))
			}
			row, err := engine.store.Get(context.Background(), id)
			if err != nil || row.Status != "completed" || len(row.Error) != 0 {
				t.Fatalf("stored execution=%+v err=%v", row, err)
			}
			info, err := engine.TaskState(context.Background(), id)
			if err != nil || info.State.DeliveryResult.Status != DeliveryVerified {
				t.Fatalf("stored verification=%+v err=%v", info, err)
			}
		})
	}
}

type deliveryStepProvider struct {
	step int
	next func(context.Context, int) (protocol.ProviderResponse, error)
}

func (p *deliveryStepProvider) Model() string { return "delivery-fixture" }
func (p *deliveryStepProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	return p.next(ctx, p.step)
}

func TestDeliveryUnavailableDuringExecutionDoesNotCancelOrReplay(t *testing.T) {
	engine, id := notebookEngine(t)
	ctx := context.Background()
	file := filepath.Join(engine.workspace, "result.txt")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	// A fixture-controlled permission error occurs only after the non-idempotent
	// tool result has committed. Agent must still reach its own final response.
	p := &deliveryStepProvider{next: func(ctx context.Context, step int) (protocol.ProviderResponse, error) {
		switch step {
		case 1:
			return toolCallResponse("append-once", "bash", `{"command":"printf 'RAN\\n' >> marker.txt; printf after > result.txt"}`), nil
		case 2:
			if err := os.Chmod(file, 0); err != nil {
				t.Fatal(err)
			}
			probe, err := os.Open(file)
			if err == nil {
				probe.Close()
				t.Skip("permission failure needs non-root execution")
			}
			if ctx.Err() != nil {
				t.Fatalf("verification cancelled running Agent: %v", ctx.Err())
			}
			info, err := engine.TaskState(ctx, id)
			if err != nil || info.State.DeliveryResult.Status != DeliveryPending {
				t.Fatalf("query started verification: %+v %v", info, err)
			}
			if _, err := engine.VerifyDelivery(ctx, id); !errors.Is(err, ErrRunHeld) {
				t.Fatalf("verification during run: %v", err)
			}
			return finalResponse("completed despite unavailable inspection"), nil
		default:
			t.Fatalf("unexpected Agent replay step %d", step)
			return protocol.ProviderResponse{}, errors.New("unexpected replay")
		}
	}}
	defer os.Chmod(file, 0600)
	result, err := engine.runAgentWith(ctx, id, p, AgentOptions{Policy: "full", Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt", Changed: true}}}})
	if err != nil || result.Status != "completed" || result.Delivery.Status != DeliveryUnavailable {
		t.Fatalf("result=%+v delivery=%+v err=%v", result, result.Delivery, err)
	}
	issue := result.Delivery.Issues[0]
	if issue.Path != "result.txt" || issue.Phase != "verification" || !strings.Contains(issue.Reason, "permission denied") {
		t.Fatalf("missing evidence: %+v", issue)
	}
	row, err := engine.store.Get(ctx, id)
	if err != nil || row.Status != "completed" || len(row.Error) != 0 {
		t.Fatalf("execution polluted: %+v %v", row, err)
	}
	info, err := engine.TaskState(ctx, id)
	if err != nil || info.State.DeliveryResult.Status != DeliveryUnavailable {
		t.Fatalf("unavailable evidence not durable: %+v %v", info, err)
	}
	baseline := *info.State.Delivery.Files[0].Baseline
	version := info.Version
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	// Engine restart and verification-only recovery cannot ask a model to repeat
	// the successful append. State and transcript are the recovery source.
	workspace := engine.workspace
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	verified, err := reopened.VerifyDelivery(ctx, id)
	if err != nil || verified.Status != DeliveryVerified {
		t.Fatalf("verification-only recovery: %+v %v", verified, err)
	}
	info, err = reopened.TaskState(ctx, id)
	if err != nil || info.Version <= version || *info.State.Delivery.Files[0].Baseline != baseline {
		t.Fatalf("recovery baseline changed: %+v %v", info, err)
	}
	marker, err := os.ReadFile(filepath.Join(workspace, "marker.txt"))
	if err != nil || string(marker) != "RAN\n" || p.step != 2 {
		t.Fatalf("non-idempotent effect replayed: %q steps=%d %v", marker, p.step, err)
	}
	row, err = reopened.store.Get(ctx, id)
	if err != nil || row.Status != "completed" || len(row.Error) != 0 {
		t.Fatalf("restart polluted execution: %+v %v", row, err)
	}
}

func TestDeliveryBaselineFailureDoesNotPreventAgentExecution(t *testing.T) {
	engine, id := notebookEngine(t)
	file := filepath.Join(engine.workspace, "result.txt")
	if err := os.WriteFile(file, []byte("before"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(file, 0600)
	probe, err := os.Open(file)
	if err == nil {
		probe.Close()
		t.Skip("permission failure needs non-root execution")
	}
	p := &deliveryStepProvider{next: func(ctx context.Context, step int) (protocol.ProviderResponse, error) {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if step == 1 {
			if err := os.Chmod(file, 0600); err != nil {
				t.Fatal(err)
			}
			return toolCallResponse("write", "write_file", `{"path":"result.txt","content":"after"}`), nil
		}
		return finalResponse("done"), nil
	}}
	r, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Policy: "workspace", Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt", Changed: true}}}})
	if err != nil || r.Status != "completed" || p.step != 2 || r.Delivery.Status != DeliveryUnavailable {
		t.Fatalf("baseline error stopped Agent: %+v delivery=%+v %v", r, r.Delivery, err)
	}
	if r.Delivery.Issues[0].Phase != "baseline" || r.Delivery.Issues[0].Path != "result.txt" {
		t.Fatalf("baseline evidence: %+v", r.Delivery)
	}
	retry, err := engine.VerifyDelivery(context.Background(), id)
	if err != nil || retry.Status != DeliveryUnavailable {
		t.Fatalf("lost baseline falsely reconstructed: %+v %v", retry, err)
	}
}

func TestDeliveryVerifierPanicIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := inspectDelivery(func() DeliveryResult { panic("hash read failed") })
	if result.Status != DeliveryUnavailable || len(result.Issues) != 1 || result.Issues[0].Phase != "panic" || !strings.Contains(result.Issues[0].Reason, "hash read failed") || ctx.Err() != nil {
		t.Fatalf("panic classification=%+v ctx=%v", result, ctx.Err())
	}
}

func TestDeliveryCancelledVerificationKeepsCompleted(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &notebookProvider{responses: []protocol.ProviderResponse{toolCallResponse("write", "write_file", `{"path":"result.txt","content":"done"}`), finalResponse("done")}}
	opts := AgentOptions{Policy: "workspace", Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}}
	opts.ModelRef = bind(engine, p)
	life, done := engine.trackRun(context.Background(), id)
	defer done()
	prepared, err := engine.prepareRun(life, id, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prepared.finishOwned = cancel // completion cleanup ends the run context before verification
	r, err := prepared.execute(ctx)
	if err != nil || r.Status != "completed" || r.Delivery.Status != DeliveryUnavailable {
		t.Fatalf("cancel after completion: %+v delivery=%+v %v", r, r.Delivery, err)
	}
	info, err := engine.TaskState(context.Background(), id)
	if err != nil || info.State.DeliveryResult.Status != DeliveryUnavailable {
		t.Fatalf("cancel evidence=%+v %v", info, err)
	}
}

func TestDeliveryExecutionFailureRemainsSeparate(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &deliveryStepProvider{next: func(context.Context, int) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAuth, Err: errors.New("unauthorized")}
	}}
	r, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}})
	if err == nil || r.Status != "failed" {
		t.Fatalf("Agent failure lost: %+v %v", r, err)
	}
	if r.Delivery == nil || r.Delivery.Status != DeliveryPending || r.Delivery.RunID == "" {
		t.Fatalf("result omits the recorded verification: %+v", r.Delivery)
	}
	info, getErr := engine.TaskState(context.Background(), id)
	if getErr != nil || info.State.DeliveryResult.Status != DeliveryPending || info.State.DeliveryResult.RunID != r.Delivery.RunID {
		t.Fatalf("unexecuted verification mislabeled: %+v %v", info, getErr)
	}
}

func TestDeliveryVerifyWithoutContractIsAnError(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	r, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{})
	if err != nil || r.Status != "completed" || r.Delivery != nil {
		t.Fatalf("no contract: %+v %v", r, err)
	}
	if got, err := engine.VerifyDelivery(context.Background(), id); !errors.Is(err, ErrNoDeliveryContract) || got != nil {
		t.Fatalf("verify without contract: %+v %v", got, err)
	}
}

func TestDeliveryResumeAfterSuccessfulToolDoesNotReplay(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &notebookProvider{responses: []protocol.ProviderResponse{toolCallResponse("append-once", "bash", `{"command":"printf 'ONCE\\n' >> marker.txt"}`)}}
	d := &DeliveryContract{Files: []DeliveryFile{{Path: "marker.txt", Changed: true}}}
	first, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Policy: "full", Delivery: d, MaxSteps: 1})
	if err == nil || first.Status != "interrupted" {
		t.Fatalf("step boundary: %+v %v", first, err)
	}
	resume := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	second, err := engine.runAgentWith(context.Background(), id, resume, AgentOptions{Policy: "full"})
	if err != nil || second.Status != "completed" || second.Delivery.Status != DeliveryVerified {
		t.Fatalf("resume=%+v delivery=%+v %v", second, second.Delivery, err)
	}
	raw, err := os.ReadFile(filepath.Join(engine.workspace, "marker.txt"))
	if err != nil || string(raw) != "ONCE\n" {
		t.Fatalf("tool replayed: %q %v", raw, err)
	}
	// The successful tool result is present as a paired observable recovery message.
	encoded, _ := json.Marshal(resume.requests)
	if !strings.Contains(string(encoded), "append-once") || strings.Contains(string(encoded), "reasoning_content") {
		t.Fatalf("invalid recovery transcript: %s", encoded)
	}
}

func TestDeliveryExplicitGitRequirementUnavailableNotFailed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	engine, id := notebookEngine(t)
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	r, err := engine.runAgentWith(ctx, id, p, AgentOptions{Delivery: &DeliveryContract{Branch: "task", Commit: true}})
	if err != nil || r.Status != "completed" || r.Delivery.Status != DeliveryUnavailable || len(p.requests) != 1 {
		t.Fatalf("Git requirement: %+v delivery=%+v %v", r, r.Delivery, err)
	}
}

func TestDeliveryCompletionCrashWindowDoesNotReplayAgent(t *testing.T) {
	engine, id := notebookEngine(t)
	ctx := context.Background()
	p := &notebookProvider{responses: []protocol.ProviderResponse{toolCallResponse("once", "bash", `{"command":"printf 'ONCE\\n' >> marker.txt"}`), finalResponse("done")}}
	opts := AgentOptions{Policy: "full", ModelRef: bind(engine, p), Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "marker.txt", Changed: true}}}}
	life, done := engine.trackRun(ctx, id)
	// The simulated crashed run ends before this test reopens its store.
	defer func() {
		if done != nil {
			done()
		}
	}()
	prepared, err := engine.prepareRun(life, id, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after the execution terminal commit, before verification.
	r, err := prepared.loop.Run(life, id, prepared.loopOpts)
	if err != nil || r.Status != "completed" {
		t.Fatalf("execution=%+v %v", r, err)
	}
	done()
	done = nil
	workspace := engine.workspace
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	info, err := reopened.TaskState(ctx, id)
	if err != nil || info.State.DeliveryResult.Status != DeliveryPending {
		t.Fatalf("crash window fabricated verification: %+v %v", info, err)
	}
	row, err := reopened.store.Get(ctx, id)
	if err != nil || row.Status != "completed" {
		t.Fatalf("crash window changed execution: %+v %v", row, err)
	}
	verified, err := reopened.VerifyDelivery(ctx, id)
	if err != nil || verified.Status != DeliveryVerified {
		t.Fatalf("verification-only resume: %+v %v", verified, err)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "marker.txt"))
	if err != nil || string(raw) != "ONCE\n" {
		t.Fatalf("effect replayed: %q %v", raw, err)
	}
}

func TestDeliveryChildVerificationSettlementIsDeliveredSeparately(t *testing.T) {
	engine, child := notebookEngine(t)
	ctx := context.Background()
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	r, err := engine.runAgentWith(ctx, child, p, AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result"}}}})
	if err != nil || r.Status != "completed" || r.Delivery.Status != DeliveryFailed {
		t.Fatalf("child=%+v %v", r, err)
	}
	parent, err := engine.NewSession(ctx, SessionOptions{Goal: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: parent.ID, ChildSessionID: child, Kind: RelationExecution}); err != nil {
		t.Fatal(err)
	}
	var state taskstate.State
	check := func(want string) {
		t.Helper()
		rows, _, err := engine.taskResults(ctx, parent.ID, &state)
		if err != nil || len(rows) != 1 {
			t.Fatalf("delivery notification: %d %v", len(rows), err)
		}
		var result TaskResult
		if err := json.Unmarshal([]byte(strings.TrimPrefix(rows[0].Content, protocol.TaskResultHeader+"\n")), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" || result.Delivery.Status != want || result.ErrorKind != "" {
			t.Fatalf("execution/verification conflated: %+v", result)
		}
		rows, _, err = engine.taskResults(ctx, parent.ID, &state)
		if err != nil || len(rows) != 0 {
			t.Fatalf("duplicate verification notification: %d %v", len(rows), err)
		}
	}
	check(DeliveryFailed)
	info, err := engine.TaskState(ctx, parent.ID)
	if err != nil || !strings.Contains(strings.Join(info.Obligations, ","), "tasks") {
		t.Fatalf("unverified child discharged obligation: %+v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(engine.workspace, "result"), []byte("delivered"), 0600); err != nil {
		t.Fatal(err)
	}
	verified, err := engine.VerifyDelivery(ctx, child)
	if err != nil || verified.Status != DeliveryVerified {
		t.Fatalf("child verification: %+v %v", verified, err)
	}
	check(DeliveryVerified)
	info, err = engine.TaskState(ctx, parent.ID)
	if err != nil || strings.Contains(strings.Join(info.Obligations, ","), "tasks") {
		t.Fatalf("verified child still an obligation: %+v %v", info, err)
	}
}
